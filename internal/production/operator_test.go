package production

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"

	"bambastic/internal/access"
)

func TestDryingScheduleUsesRealTelemetry(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	r := f.s.devices[1].report
	r["firmware_version"] = "01.11.02.00"
	unit := map[string]any{"id": "0", "dry_time": 0, "dry_sf_reason": []any{8}, "tray": []any{map[string]any{"id": "0", "tray_type": "PETG"}}}
	r["ams"] = map[string]any{"ams": []any{unit}}
	f.req("POST", "/printers/1/drying/start?ams_id=0", nil, f.admin, "", 409)
	f.req("POST", "/scheduled-dryings", row{"printer_id": 1, "temp": 55, "duration_hours": 2}, f.admin, "", 200)
	check := func(state, reason string) {
		t.Helper()
		j, e := one(ctx, f.s.db, "SELECT * FROM scheduled_dryings ORDER BY id DESC LIMIT 1")
		if e != nil || j["status"] != state || val(j["waiting_reason"]) != reason {
			t.Fatal(j, e)
		}
	}
	f.s.tickDry(ctx)
	check("pending", "ams_power_required")
	if len(f.hw.messages) != 0 {
		t.Fatal(f.hw.messages)
	}
	unit["dry_sf_reason"] = []any{}
	f.s.tickDry(ctx)
	check("running", "")
	cmd := f.hw.messages[0]["print"].(row)
	if cmd["filament"] != "PETG" || num(cmd["duration"]) != 2 || num(cmd["temp"]) != 55 || cmd["command"] != "ams_filament_drying" {
		t.Fatal(cmd)
	}
	f.s.tickDry(ctx)
	if len(f.hw.messages) != 1 {
		t.Fatal("republished ambiguous start")
	}
	if _, e := f.s.db.Exec("UPDATE scheduled_dryings SET started_at=datetime('now','-3 minutes')"); e != nil {
		t.Fatal(e)
	}
	f.s.tickDry(ctx)
	check("failed", "")
	f.req("DELETE", "/scheduled-dryings/1", nil, f.admin, "", 200)
	f.req("POST", "/scheduled-dryings", row{"printer_id": 1, "temp": 55, "duration_hours": 2}, f.admin, "", 200)
	f.s.tickDry(ctx)
	unit["dry_time"] = 120
	unit["info"] = "20"
	f.s.tickDry(ctx)
	if _, e := f.s.db.Exec("UPDATE scheduled_dryings SET started_at=datetime('now','-116 minutes') WHERE status='running'"); e != nil {
		t.Fatal(e)
	}
	unit["dry_time"] = 0
	unit["info"] = "0"
	f.s.tickDry(ctx)
	check("completed", "")
	f.req("PATCH", "/printers/1", row{"model": "P1S"}, f.admin, "", 200)
	f.req("POST", "/printers/1/drying/start?ams_id=0", nil, f.admin, "", 400)
}
func TestDiagnosticCameraPort(t *testing.T) {
	f := farmFixture(t)
	for _, tc := range []struct {
		model, open      string
		probed, reported []string
	}{
		{"P1S", "6000", []string{"6000"}, []string{"Chamber Image 6000 pass"}},
		{"N1", "322", []string{"6000"}, []string{"Chamber Image 6000 warn"}},
		{"N9", "6000", []string{"6000"}, []string{"Chamber Image 6000 pass"}},
		{"H2D Pro", "322", []string{"322"}, []string{"RTSPS 322 pass"}},
		{"N7", "6000", []string{"322"}, []string{"RTSPS 322 warn"}},
		{"", "322", []string{"322", "6000"}, []string{"RTSPS 322 pass"}},
		{"unknown", "6000", []string{"322", "6000"}, []string{"Chamber Image 6000 pass"}},
		{"unknown", "", []string{"322", "6000"}, []string{"Chamber Image 6000 warn", "RTSPS 322 warn"}},
	} {
		t.Run(tc.model+"_"+tc.open, func(t *testing.T) {
			calls := make(chan string, 4)
			f.s.probePort = func(_ context.Context, _, port, _ string, _ bool) error {
				if port == "990" || port == "8883" {
					return nil
				}
				calls <- port
				if port != tc.open {
					return errors.New("port closed")
				}
				return nil
			}
			var result struct {
				Checks []struct {
					ID, Status string
					Params     struct{ Port, Protocol string }
				}
			}
			body := f.req("POST", "/printers/diagnostic", row{"model": tc.model, "ip_address": "127.0.0.1"}, f.admin, "", 200)
			if err := json.Unmarshal(body, &result); err != nil {
				t.Fatal(err)
			}
			close(calls)
			var probed, reported []string
			for port := range calls {
				probed = append(probed, port)
			}
			slices.Sort(probed)
			for _, check := range result.Checks {
				if check.ID == "port_rtsps" {
					reported = append(reported, check.Params.Protocol+" "+check.Params.Port+" "+check.Status)
				}
			}
			if !slices.Equal(probed, tc.probed) || !slices.Equal(reported, tc.reported) {
				t.Fatalf("probed %v, reported %v", probed, reported)
			}
		})
	}
}

func TestDiagnosticPortTLS(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	u, _ := url.Parse(server.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	if err := defaultProbe(t.Context(), host, port, "", true); err != nil {
		t.Fatal(err)
	}
	err := defaultProbe(t.Context(), host, port, "different-key", true)
	if !errors.Is(err, errPortTLS) || !errors.Is(err, errPrinterKeyChanged) {
		t.Fatalf("expected a reachable port with a different key, got %v", err)
	}
	plain := httptest.NewServer(nil)
	defer plain.Close()
	u, _ = url.Parse(plain.URL)
	host, port, _ = net.SplitHostPort(u.Host)
	if err := defaultProbe(t.Context(), host, port, "", false); err != nil {
		t.Fatal(err)
	}
	if err := defaultProbe(t.Context(), host, port, "", true); !errors.Is(err, errPortTLS) {
		t.Fatalf("open plaintext port lost the TLS failure: %v", err)
	}
}

func TestDiagnosticConnectionEvidence(t *testing.T) {
	f := farmFixture(t)
	var ports map[string]error
	f.s.probePort = func(_ context.Context, _, port, _ string, secure bool) error {
		if secure != (port == "990") {
			t.Errorf("unexpected TLS check for port %s", port)
		}
		return ports[port]
	}
	var openErr error
	opens := 0
	f.s.open = func(_ context.Context, _ row, _ string, receive func(row)) (hardware, error) {
		opens++
		if receive == nil {
			t.Error("saved printer diagnostic did not subscribe to status")
		}
		return f.hw, openErr
	}
	check := func(overall string, want map[string]string) {
		t.Helper()
		var result struct {
			Overall string
			Checks  []struct {
				ID, Status string
				Params     struct{ Reason string }
			}
		}
		if err := json.Unmarshal(f.req("GET", "/printers/1/diagnostic", nil, f.admin, "", 200), &result); err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, c := range result.Checks {
			got[c.ID] = strings.TrimSpace(c.Status + " " + c.Params.Reason)
		}
		if result.Overall != overall {
			t.Errorf("overall %q, checks %v", result.Overall, got)
		}
		for id, value := range want {
			if got[id] != value {
				t.Errorf("%s = %q, want %q", id, got[id], value)
			}
		}
	}
	ports = map[string]error{"8883": syscall.EHOSTUNREACH, "990": errPortTLS}
	check("warnings", map[string]string{"port_mqtt": "pass", "mqtt_auth": "pass", "printer_publishing": "pass", "port_ftps": "warn no_tls"})
	if opens != 0 {
		t.Fatal("diagnosing a live session opened a redundant MQTT login")
	}
	f.s.devices[1].h = nil
	ports = nil
	check("ok", map[string]string{"port_mqtt": "pass", "mqtt_auth": "pass", "printer_publishing": "pass"})
	openErr = errPrinterStatus
	check("problems", map[string]string{"port_mqtt": "pass", "mqtt_auth": "pass", "printer_publishing": "fail"})
	openErr = errPrinterKeyChanged
	ports = map[string]error{"990": errPrinterKeyChanged}
	check("problems", map[string]string{"port_mqtt": "pass", "mqtt_auth": "fail certificate_changed", "printer_publishing": "skip", "port_ftps": "warn certificate_changed"})
	ports = map[string]error{"8883": syscall.EHOSTUNREACH, "990": syscall.EHOSTUNREACH, "322": syscall.EHOSTUNREACH}
	blocked := map[string]string{"port_mqtt": "fail", "mqtt_auth": "skip", "printer_publishing": "skip"}
	if runtime.GOOS == "darwin" {
		blocked["macos_local_network"] = "warn permission"
	}
	check("problems", blocked)
	if opens != 3 {
		t.Fatalf("unexpected number of MQTT checks: %d", opens)
	}
}
func TestDiagnosticsConfigurationAndFaultActions(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	f.s.probePort = func(ctx context.Context, ip, port, pin string, secure bool) error {
		if port == "322" {
			return errors.New("port closed")
		}
		return nil
	}
	var diag row
	json.Unmarshal(f.req("GET", "/printers/1/diagnostic", nil, p, "", 200), &diag)
	if diag["overall"] != "warnings" || len(diag["checks"].([]any)) != 5 {
		t.Fatal(diag)
	}
	f.req("POST", "/printers/diagnostic", row{"ip_address": "foo"}, p, "", 422)
	f.req("PATCH", "/printers/1", row{"wled_config": row{"enabled": true, "base_url": "http://u:p@wled.local"}}, p, "", 422)
	config := row{"wled_config": row{"enabled": true, "base_url": "http://led.local", "presets": row{"printing": 2}}, "external_camera_type": "rtsp", "external_camera_url": "rtsp://viewer:private@cam.local/live", "plate_detection_roi": row{"x": 0.3, "y": 0.4, "w": 0.2, "h": 0.3}}
	var v row
	json.Unmarshal(f.req("PATCH", "/printers/1", config, p, "", 200), &v)
	if v["wled_config"].(map[string]any)["base_url"] != "http://led.local" || strings.Contains(val(v["external_camera_url"]), "private") {
		t.Fatal(v)
	}
	f.req("PATCH", "/printers/1", row{"external_camera_url": v["external_camera_url"]}, p, "", 200)
	raw, e := one(t.Context(), f.s.db, "SELECT external_camera_url FROM printers WHERE id=1")
	if e != nil || raw["external_camera_url"] != "rtsp://viewer:private@cam.local/live" {
		t.Fatal(raw, e)
	}
	visible := string(f.req("GET", "/printers/1", nil, access.Principal{Permissions: map[string]bool{"printers:read": true}}, "", 200))
	if strings.Contains(visible, "cam.local") || strings.Contains(visible, "led.local") {
		t.Fatal(visible)
	}
	d := f.s.devices[1]
	f.req("POST", "/printers/1/logging/enable", nil, p, "", 200)
	d.logs.add("report", "in", row{"nested": row{"access_code": "key"}, "nozzle_temper": 23})
	got := string(f.req("GET", "/printers/1/logging", nil, p, "", 200))
	if strings.Contains(got, `"key"`) || !strings.Contains(got, "redacted") {
		t.Fatal(got)
	}
	f.req("DELETE", "/printers/1/logging", nil, p, "", 200)
	if got = string(f.req("GET", "/printers/1/logging", nil, p, "", 200)); !strings.Contains(got, `"logs":[]`) {
		t.Fatal(got)
	}
	if _, e = f.s.db.Exec("UPDATE printers SET serial_number='31BTEST'"); e != nil {
		t.Fatal(e)
	}
	d.report["subtask_id"] = "41"
	d.report["print_error"] = int64(0x07008028)
	m := row{"print_error": "07008028", "action": "CONTINUE", "job_id": "22"}
	f.req("POST", "/printers/1/hms/execute-action", m, p, "", 409)
	m["job_id"] = "41"
	f.req("POST", "/printers/1/hms/execute-action", m, p, "", 200)
	if f.hw.messages[0]["print"].(row)["command"] != "ams_control" {
		t.Fatal(f.hw.messages)
	}
}
