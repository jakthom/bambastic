package production

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"runtime"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"bambastic/internal/access"
)

type messageHistory struct {
	mu      sync.Mutex
	enabled bool
	logs    []row
}

func sanitized(v any) any {
	switch obj := v.(type) {
	case row:
		return sanitized(map[string]any(obj))
	case map[string]any:
		out := row{}
		for k, x := range obj {
			key := strings.ToLower(k)
			if strings.Contains(key, "password") || strings.Contains(key, "access_code") || strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "credential") || key == "url" || key == "ssid" {
				out[k] = "[redacted]"
			} else {
				out[k] = sanitized(x)
			}
		}
		return out
	case []any:
		out := make([]any, 0, len(obj))
		for _, x := range obj {
			out = append(out, sanitized(x))
		}
		return out
	default:
		return v
	}
}
func (m *messageHistory) add(topic, dir string, msg row) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.enabled {
		return
	}
	payload := sanitized(msg)
	if b, e := json.Marshal(payload); e != nil || len(b) > 16384 {
		payload = row{"command": msg["command"], "truncated": true}
	}
	if len(m.logs) >= 200 {
		copy(m.logs, m.logs[1:])
		m.logs = m.logs[:199]
	}
	m.logs = append(m.logs, row{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "topic": topic, "direction": dir, "payload": payload})
}
func (s *Service) logging(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := "printers:control"
	if r.Method == "GET" {
		perm = "printers:read"
	}
	if _, e := s.printerRow(r, perm); e != nil {
		return nil, e
	}
	s.mu.RLock()
	d := s.devices[pathID(r)]
	s.mu.RUnlock()
	if d == nil {
		if r.Method == "GET" {
			return row{"logging_enabled": false, "logs": []row{}}, nil
		}
		return nil, fail(409, "Printer is disconnected")
	}
	m := &d.logs
	m.mu.Lock()
	defer m.mu.Unlock()
	switch r.Method {
	case "GET":
		return row{"logging_enabled": m.enabled, "logs": append([]row{}, m.logs...)}, nil
	case "DELETE":
		m.logs = nil
		return row{"status": "cleared"}, nil
	}
	m.enabled = strings.HasSuffix(r.URL.Path, "/enable")
	return row{"logging_enabled": m.enabled}, nil
}

// CameraPort identifies a built-in printer camera transport.
type CameraPort string

const (
	ChamberImage CameraPort = "6000"
	RTSPS        CameraPort = "322"
)

func (p CameraPort) protocol() string {
	if p == ChamberImage {
		return "Chamber Image"
	}
	return "RTSPS"
}

// CameraPorts returns the model's camera endpoint, or both supported endpoints
// in viewing preference order when the printer has no recognized model.
func CameraPorts(model string) []CameraPort {
	name := modelName(strings.TrimSpace(model))
	if in(name, "P1P", "P1S", "A1", "A1MINI", "A2L") {
		return []CameraPort{ChamberImage}
	}
	for _, family := range []string{"X1", "X2", "H2", "H3", "P2"} {
		if strings.HasPrefix(name, family) {
			return []CameraPort{RTSPS}
		}
	}
	return []CameraPort{ChamberImage, RTSPS}
}

var errPortTLS = errors.New("port answered but TLS handshake failed")

func defaultProbe(ctx context.Context, ip, port, pin string, tlsCheck bool) error {
	d := net.Dialer{Timeout: 3 * time.Second}
	target := net.JoinHostPort(ip, port)
	c, e := d.DialContext(ctx, "tcp", target)
	if e != nil {
		return e
	}
	defer c.Close()
	if tlsCheck {
		handshake, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		cfg, _ := checkedTLS(pin)
		if e := tls.Client(c, cfg).HandshakeContext(handshake); e != nil {
			return fmt.Errorf("%w: %w", errPortTLS, e)
		}
	}
	return nil
}
func (s *Service) diagnostic(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	var pr row
	var e error
	var pin string
	var online bool
	saved := r.Method == http.MethodGet
	if !saved {
		if e = permitted(r, "printers:create"); e != nil {
			return nil, e
		}
		if p := access.From(ctx); p.PrinterIDs != nil {
			return nil, fail(403, "Fleet-wide registration required")
		}
		pr, e = input(w, r)
	} else {
		pr, e = s.printerRow(r, "printers:read")
		if e == nil {
			_ = s.db.QueryRowContext(ctx, "SELECT fingerprint FROM bb_printer_trust WHERE printer_id=?", pr["id"]).Scan(&pin)
			online = flag(s.state(num(pr["id"]), pr)["connected"])
		}
	}
	if e != nil {
		return nil, e
	}
	ip := net.ParseIP(val(pr["ip_address"]))
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() {
		return nil, fail(422, "Enter the printer LAN IP address")
	}
	type portCheck struct {
		key, port     string
		tls, optional bool
	}
	jobs := []portCheck{{"port_mqtt", "8883", false, false}, {"port_ftps", "990", true, true}}
	cameraPorts := CameraPorts(val(pr["model"]))
	for _, port := range cameraPorts {
		jobs = append(jobs, portCheck{"port_rtsps", string(port), false, true})
	}
	checks := make([]row, len(jobs))
	routeDenied := make([]bool, len(jobs))
	var wg sync.WaitGroup
	for i, job := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status := "pass"
			params := row{"port": job.port}
			// An authenticated live session already proves reachability on 8883.
			var probeErr error
			if !(job.key == "port_mqtt" && online) {
				probeErr = s.probePort(ctx, ip.String(), job.port, pin, job.tls)
			}
			if probeErr != nil {
				routeDenied[i] = !errors.Is(probeErr, errPortTLS) && (errors.Is(probeErr, syscall.EHOSTUNREACH) ||
					errors.Is(probeErr, syscall.ENETUNREACH) || errors.Is(probeErr, syscall.EACCES) || errors.Is(probeErr, syscall.EPERM))
				status = "fail"
				if job.optional {
					status = "warn"
				}
				switch {
				case errors.Is(probeErr, errPrinterKeyChanged):
					params["reason"] = "certificate_changed"
				case errors.Is(probeErr, errPortTLS):
					params["reason"] = "no_tls"
				}
			}
			if job.key == "port_rtsps" {
				params["protocol"] = CameraPort(job.port).protocol()
			}
			checks[i] = row{"id": job.key, "status": status, "params": params}
		}()
	}
	wg.Wait()
	if saved {
		online = flag(s.state(num(pr["id"]), pr)["connected"])
		if online {
			checks[0]["status"] = "pass"
		}
	}
	// The two ports are alternatives. If either answers for an unknown model,
	// report the reachable endpoint. If neither answers, show both attempts.
	if len(cameraPorts) > 1 {
		for _, check := range checks {
			if check["id"] == "port_rtsps" && check["status"] == "pass" {
				checks = append(checks[:len(checks)-len(cameraPorts)], check)
				break
			}
		}
	}
	if runtime.GOOS == "darwin" && !online && !slices.Contains(routeDenied, false) {
		// macOS can report "no route to host" when this launcher's LAN access
		// is restricted. The same errno is possible with a real routing issue.
		checks = append(checks, row{"id": "macos_local_network", "status": "warn", "params": row{"reason": "permission"}})
	}
	auth, publishing := "skip", "skip"
	authParams := row{}
	if online {
		// The live session becomes ready after it receives a printer status report.
		auth, publishing = "pass", "pass"
	} else if val(pr["serial_number"]) != "" && val(pr["access_code"]) != "" && checks[0]["status"] == "pass" {
		// Pre-registration checks only the login. For a saved printer also listen
		// on its exact report topic, even if the background monitor is reconnecting.
		var receive func(row)
		if saved {
			receive = func(row) {}
		}
		read, done := context.WithTimeout(ctx, 10*time.Second)
		h, e := s.open(read, pr, pin, receive)
		done()
		switch {
		case e == nil:
			auth = "pass"
			if saved {
				publishing = "pass"
			}
			h.Close()
		case errors.Is(e, errPrinterStatus):
			auth, publishing = "pass", "fail"
		default:
			auth = "fail"
			if mqttAuthRejected(e) {
				authParams["reason"] = "auth_rejected"
			} else if errors.Is(e, errPrinterKeyChanged) {
				authParams["reason"] = "certificate_changed"
			}
		}
	}
	checks = append(checks, row{"id": "mqtt_auth", "status": auth, "params": authParams}, row{"id": "printer_publishing", "status": publishing, "params": row{}})
	overall := "ok"
	for _, v := range checks {
		if v["status"] == "fail" {
			overall = "problems"
			break
		}
		if v["status"] == "warn" {
			overall = "warnings"
		}
	}
	return row{"printer_id": pr["id"], "name": pr["name"], "ip_address": ip.String(), "overall": overall, "checks": checks, "mqtt_connected": online, "access_code_set": val(pr["access_code"]) != "", "certificate_enrolled": pin != ""}, nil
}
