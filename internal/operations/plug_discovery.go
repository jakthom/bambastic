package operations

import (
	"bambastic/internal/access"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

type plugDiscovery struct {
	mu             sync.Mutex
	stop           context.CancelFunc
	done           chan struct{}
	total, scanned int
	devices        []obj
}

func (d *plugDiscovery) status() obj {
	return obj{"running": d.stop != nil, "total": d.total, "scanned": d.scanned}
}
func (d *plugDiscovery) close() {
	d.mu.Lock()
	done := d.done
	if d.stop != nil {
		d.stop()
	}
	d.mu.Unlock()
	if done != nil {
		<-done
	}
}
func discoverRange(from, to string) ([]netip.Addr, error) {
	if from == "" && to == "" {
		addresses, _ := net.InterfaceAddrs()
		for _, a := range addresses {
			p, e := netip.ParsePrefix(a.String())
			if e != nil || !p.Addr().Is4() || !p.Addr().IsPrivate() || p.Bits() >= 31 {
				continue
			}
			bits := max(24, p.Bits())
			start := netip.PrefixFrom(p.Addr(), bits).Masked().Addr()
			end := start
			for range (1 << (32 - bits)) - 2 {
				end = end.Next()
			}
			from = start.Next().String()
			to = end.String()
			break
		}
	}
	a, e := netip.ParseAddr(from)
	b, f := netip.ParseAddr(to)
	if e != nil || f != nil || !a.Is4() || !b.Is4() || !a.IsPrivate() || !b.IsPrivate() || a.Compare(b) > 0 {
		return nil, fail(422, "Select a private IPv4 LAN range")
	}
	hosts := []netip.Addr{}
	for ip := a; ip.Compare(b) <= 0; ip = ip.Next() {
		if !ip.IsPrivate() || len(hosts) >= 1024 {
			return nil, fail(422, "Select up to 1024 LAN addresses")
		}
		hosts = append(hosts, ip)
	}
	return hosts, nil
}
func probePlug(ctx context.Context, base, ip string) obj {
	var p obj
	err := send(ctx, "GET", base+"/cm?cmnd=Power", nil, nil, &p)
	var f fault
	authRequired := errors.As(err, &f) && strings.HasSuffix(f.s, "HTTP 401")
	if err != nil && !authRequired {
		return nil
	}
	state := power(p)
	if state != "ON" && state != "OFF" && !authRequired && p["WARNING"] == nil {
		return nil
	}
	device := obj{"ip_address": ip, "name": "Tasmota (" + ip + ")", "state": state, "discovered_at": now(), "module": nil}
	if authRequired || p["WARNING"] != nil {
		device["state"] = "UNKNOWN"
		return device
	}
	var info obj
	if send(ctx, "GET", base+"/cm?cmnd=Status%200", nil, nil, &info) == nil {
		v := sub(info["Status"])
		device["module"] = v["Module"]
		if n := str(v["DeviceName"]); n != "" {
			device["name"] = n
		} else if names, ok := v["FriendlyName"].([]any); ok && len(names) > 0 {
			device["name"] = names[0]
		}
	}
	return device
}
func (d *plugDiscovery) run(ctx context.Context, hosts []netip.Addr, timeout time.Duration, done chan struct{}) {
	queue := make(chan netip.Addr)
	var wg sync.WaitGroup
	for range min(len(hosts), 16) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for host := range queue {
				call, cancel := context.WithTimeout(ctx, timeout)
				ip := host.String()
				v := probePlug(call, "http://"+ip, ip)
				cancel()
				d.mu.Lock()
				d.scanned++
				if v != nil {
					d.devices = append(d.devices, v)
				}
				d.mu.Unlock()
			}
		}()
	}
loop:
	for _, ip := range hosts {
		select {
		case <-ctx.Done():
			break loop
		case queue <- ip:
		}
	}
	close(queue)
	wg.Wait()
	d.mu.Lock()
	d.stop = nil
	close(done)
	d.mu.Unlock()
}
func (s *Service) discoverPlugs(m *http.ServeMux) {
	d := &s.plugDiscovery
	s.route(m, "POST", "smart-plugs/discover/scan", "smart_plugs:create", func(r *http.Request) (any, error) {
		req := struct {
			From    string  `json:"from_ip"`
			To      string  `json:"to_ip"`
			Timeout float64 `json:"timeout"`
		}{Timeout: 1}
		if e := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); e != nil && e != io.EOF {
			return nil, fail(422, "Invalid scan request")
		}
		hosts, e := discoverRange(req.From, req.To)
		if e != nil {
			return nil, e
		}
		if req.Timeout <= 0 || req.Timeout > 5 {
			return nil, fail(422, "Timeout must be greater than zero and at most five seconds")
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.stop == nil {
			ctx, cancel := context.WithCancel(context.Background())
			d.stop = cancel
			d.done = make(chan struct{})
			d.devices = nil
			d.scanned = 0
			d.total = len(hosts)
			go d.run(ctx, hosts, time.Duration(req.Timeout*float64(time.Second)), d.done)
		}
		return d.status(), nil
	})
	for _, a := range []string{"status", "devices", "stop"} {
		method, permission := "GET", "smart_plugs:read"
		if a == "stop" {
			method, permission = "POST", "smart_plugs:create"
		}
		s.route(m, method, "smart-plugs/discover/"+a, permission, func(r *http.Request) (any, error) {
			if a == "stop" {
				d.close()
			}
			d.mu.Lock()
			defer d.mu.Unlock()
			if a == "devices" {
				out := append([]obj{}, d.devices...)
				sort.Slice(out, func(i, j int) bool { return str(out[i]["ip_address"]) < str(out[j]["ip_address"]) })
				return out, nil
			}
			return d.status(), nil
		})
	}
	s.route(m, "POST", "smart-plugs/rest/test-connection", "smart_plugs:control", func(r *http.Request) (any, error) {
		var req struct {
			URL, Method, Headers string
			ID                   int64 `json:"plug_id"`
		}
		if e := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); e != nil {
			return nil, fail(422, "Invalid test request")
		}
		if _, e := splitURL(req.URL); e != nil {
			return nil, e
		}
		req.Method = strings.ToUpper(req.Method)
		switch req.Method {
		case "":
			req.Method = "GET"
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
		default:
			return nil, fail(422, "Unsupported HTTP method")
		}
		if req.Headers == mask {
			if !access.From(r.Context()).Has("smart_plugs:update") {
				return nil, fail(403, "Plug update permission required to use saved headers")
			}
			p, e := s.get(r.Context(), plugTable, req.ID)
			if e != nil {
				return nil, e
			}
			if !permitted(r, p["printer_id"]) {
				return nil, fail(404, "Plug not found")
			}
			if str(p["rest_headers"]) == "" || req.URL != p["rest_on_url"] && req.URL != p["rest_off_url"] && req.URL != p["rest_status_url"] && req.URL != p["rest_power_url"] && req.URL != p["rest_energy_url"] {
				return nil, fail(400, "Re-enter headers for this URL")
			}
			req.Headers = str(p["rest_headers"])
		}
		h, e := restHeaders(req.Headers)
		if e != nil {
			return nil, e
		}
		e = send(r.Context(), req.Method, req.URL, h, nil, nil)
		if e != nil {
			return obj{"success": false, "error": fmt.Sprint(e)}, nil
		}
		return obj{"success": true}, nil
	})
}
