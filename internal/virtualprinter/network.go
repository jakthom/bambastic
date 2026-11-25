package virtualprinter

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

var dnsLabel = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$`)

func tailStatus(ctx context.Context) object {
	out := object{"available": false, "fqdn": nil, "hostname": nil, "tailnet_name": nil, "tailscale_ips": []string{}}
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	b, e := exec.CommandContext(c, "tailscale", "status", "--json").Output()
	if e != nil {
		out["error"] = "Tailscale is unavailable"
		return out
	}
	var data struct {
		BackendState string
		TailscaleIPs []string
		Self         struct {
			HostName, DNSName string
			TailscaleIPs      []string
		}
		CurrentTailnet struct{ Name string }
	}
	if json.Unmarshal(b, &data) != nil || data.BackendState != "Running" {
		out["error"] = "Tailscale is disconnected"
		return out
	}
	out["available"] = true
	out["fqdn"] = strings.TrimSuffix(data.Self.DNSName, ".")
	out["hostname"] = data.Self.HostName
	out["tailnet_name"] = data.CurrentTailnet.Name
	if len(data.Self.TailscaleIPs) > 0 {
		data.TailscaleIPs = data.Self.TailscaleIPs
	}
	out["tailscale_ips"] = data.TailscaleIPs
	return out
}
func (s *Service) tailscaleCert() (*tls.Config, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v := tailStatus(ctx)
	fqdn := str(v["fqdn"])
	if !dnsLabel.MatchString(fqdn) {
		return nil, os.ErrNotExist
	}
	temp, e := os.MkdirTemp(s.dir, "tailscale-cert-")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(temp)
	cert, key := filepath.Join(temp, "leaf.pem"), filepath.Join(temp, "key.pem")
	if e = exec.CommandContext(ctx, "tailscale", "cert", "--cert-file", cert, "--key-file", key, fqdn).Run(); e != nil {
		return nil, e
	}
	pair, e := tls.LoadX509KeyPair(cert, key)
	if e != nil {
		return nil, e
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}, nil
}
func (s *Service) networkRoutes(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/virtual-printers/tailscale-status", func(w http.ResponseWriter, r *http.Request) {
		if access.Require(w, r, "settings:read") {
			httpx.JSON(w, 200, tailStatus(r.Context()))
		}
	})
	for _, method := range []string{"GET", "PUT"} {
		m.HandleFunc(method+" /api/v1/settings/virtual-printer", s.legacySettings)
	}
}
func (s *Service) legacySettings(w http.ResponseWriter, r *http.Request) {
	perm := "settings:read"
	if r.Method == "PUT" {
		perm = "settings:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	fleet, e := s.config(ctx)
	if e != nil {
		httpx.Error(w, 500, "Cannot load virtual printers")
		return
	}
	disabled, _ := access.Setting(ctx, s.db, "virtual_printer_tailscale_disabled", "true")
	source, _ := access.Setting(ctx, s.db, "virtual_printer_archive_name_source", "metadata")
	if r.Method == "PUT" {
		if len(fleet) > 0 && !s.visible(p, fleet[0]) {
			httpx.Error(w, 404, "Virtual printer not found")
			return
		}
		q := r.URL.Query()
		for _, key := range []string{"enabled", "access_code", "mode", "model", "target_printer_id", "remote_interface_ip"} {
			if q.Has(key) {
				httpx.Error(w, 400, "Change "+key+" on the individual virtual printer")
				return
			}
		}
		if q.Has("archive_name_source") {
			source = q.Get("archive_name_source")
			if source != "filename" && source != "metadata" {
				httpx.Error(w, 422, "Unknown archive name source")
				return
			}
		}
		b := disabled == "true"
		if q.Has("tailscale_disabled") {
			b, e = strconv.ParseBool(q.Get("tailscale_disabled"))
			if e != nil {
				httpx.Error(w, 422, "Invalid Tailscale option")
				return
			}
			disabled = strconv.FormatBool(b)
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e == nil {
			defer tx.Rollback()
			e = access.Put(tx, "virtual_printer_archive_name_source", source)
			if e == nil {
				e = access.Put(tx, "virtual_printer_tailscale_disabled", disabled)
			}
			if e == nil && len(fleet) > 0 {
				_, e = tx.Exec("UPDATE virtual_printers SET tailscale_disabled=? WHERE id=?", b, fleet[0].ID)
			}
			if e == nil {
				e = tx.Commit()
			}
		}
		if e != nil {
			httpx.Error(w, 500, "Cannot save virtual printer settings")
			return
		}
		s.changed()
	}
	status := object{"running": false}
	if len(fleet) > 0 && s.visible(p, fleet[0]) {
		status = s.describe(fleet[0])["status"].(object)
		if p.PrinterIDs == nil {
			status["name"] = fleet[0].Name
			status["serial"] = fleet[0].Serial
		}
	}
	httpx.JSON(w, 200, object{"tailscale_disabled": disabled == "true", "archive_name_source": source, "status": status})
}

func (s *Service) diagnose(c cfg) object {
	checks := []object{}
	overall := "ok"
	check := func(id, status string, params object) {
		if params == nil {
			params = object{}
		}
		if status == "fail" {
			overall = "problems"
		} else if status == "warn" && overall == "ok" {
			overall = "warnings"
		}
		checks = append(checks, object{"id": id, "status": status, "params": params})
	}
	condition := func(ok bool, other string) string {
		if ok {
			return "pass"
		}
		return other
	}
	check("enabled", condition(c.Enabled, "warn"), nil)
	s.mu.Lock()
	v, problem := s.instances[c.ID], s.problems[c.ID]
	s.mu.Unlock()
	status := condition(v != nil, "fail")
	if !c.Enabled {
		status = "skip"
	}
	check("running", status, object{"error": problem})
	local := c.Bind == "" || c.Bind == "0.0.0.0"
	addrs, _ := net.InterfaceAddrs()
	for _, addr := range addrs {
		ip, _, _ := net.ParseCIDR(addr.String())
		if ip != nil && ip.Equal(net.ParseIP(c.Bind)) {
			local = true
		}
	}
	check("bind_interface", condition(local, "fail"), object{"ip": c.Bind})
	check("access_code", condition(len(c.Code) == 8, "fail"), nil)
	status = "skip"
	if c.Target != 0 || c.Mode == "proxy" {
		status = condition(c.TargetIP != "" && c.Pin != "", "fail")
	}
	check("target_printer", status, object{"ip": c.TargetIP})
	ports := map[int]bool{}
	if v != nil {
		v.mu.Lock()
		for _, l := range v.listeners {
			if addr, ok := l.Addr().(*net.TCPAddr); ok {
				ports[addr.Port] = true
			}
		}
		v.mu.Unlock()
	}
	for _, x := range []struct {
		key  string
		port int
	}{{"port_ftps", 990}, {"port_mqtt", 8883}, {"port_bind", 3002}} {
		status = condition(ports[x.port], "fail")
		if !c.Enabled {
			status = "skip"
		}
		check(x.key, status, object{"port": x.port})
	}
	certOK := false
	issuer := ""
	if v != nil && v.tls != nil && len(v.tls.Certificates) > 0 {
		leaf := v.tls.Certificates[0]
		if len(leaf.Certificate) > 0 {
			cert, e := x509.ParseCertificate(leaf.Certificate[0])
			if e == nil {
				issuer = cert.Issuer.CommonName
				certOK = time.Now().After(cert.NotBefore) && time.Now().Before(cert.NotAfter)
			}
		}
	}
	status = condition(certOK, "fail")
	if v == nil {
		status = "skip"
	}
	check("certificate", status, object{"issuer": issuer})
	return object{"vp_id": c.ID, "vp_name": c.Name, "mode": c.Mode, "overall": overall, "checks": checks}
}
