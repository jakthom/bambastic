package production

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"bambastic/internal/access"
)

type catalogTransport func(*http.Request) (*http.Response, error)

func (f catalogTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFirmwareStagesVerifiedModelPackage(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	_, e := f.s.db.Exec("INSERT INTO bb_firmware_cache VALUES('X1C','01.02.03.04',unixepoch()+300)")
	if e != nil {
		t.Fatal(e)
	}
	f.s.devices[1].report["sdcard"] = true
	f.s.devices[1].report["firmware_version"] = "01.00.00.00"
	files := mockCard{}
	f.s.openCard = func(_ context.Context, pr row) (card, error) {
		if pr["fingerprint"] != "fixture" {
			t.Fatal(pr)
		}
		return files, nil
	}
	payload := strings.Repeat("signed-fixture", 100)
	f.s.firmwareTransport = catalogTransport(func(r *http.Request) (*http.Response, error) {
		data := payload
		switch r.URL.Path {
		case "/en/support/firmware-download/all":
			data = `<script type="application/json" id="__NEXT_DATA__">{"props":{"pageProps":{}},"buildId":"test-build"}</script>`
		case "/_next/data/test-build/en/support/firmware-download/x1.json":
			data = `{"pageProps":{"printerMap":{"x1":{"versions":[{"version":"01.02.03.04","url":"https://public-cdn.bambulab.com/fw.sig"},{"version":"01.00.00.01","url":"https://bambulab.com.example.org/x1.sig"}]},"p1":{"versions":[{"version":"02.00.00.00","url":"https://bambulab.com/p1.sig"}]}}}}`
		case "/fw.sig":
			if r.URL.Host != "public-cdn.bambulab.com" {
				t.Fatal(r.URL)
			}
		default:
			t.Fatalf("unexpected outbound firmware request %v", r.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(data)), ContentLength: int64(len(data)), Header: make(http.Header), Request: r}, nil
	})
	url := "/firmware/updates/1"
	f.req("POST", url+"/upload", nil, access.Principal{Permissions: map[string]bool{"firmware:read": true}}, "", 403)
	var out row
	json.Unmarshal(f.req("GET", url+"/prepare", nil, f.admin, "", 200), &out)
	if !flag(out["can_proceed"]) || out["firmware_filename"] != "fw.sig" || num(out["firmware_size"]) != int64(len(payload)) {
		t.Fatal(out)
	}
	f.req("POST", url+"/upload?version=01.00.00.01", nil, f.admin, "", 400)
	f.req("POST", url+"/upload?version=02.00.00.00", nil, f.admin, "", 400)
	f.req("POST", url+"/upload", nil, f.admin, "", 200)
	json.Unmarshal(f.req("POST", url+"/upload", nil, f.admin, "", 200), &out)
	if flag(out["started"]) {
		t.Fatal("duplicate staging")
	}
	f.s.tickFirmware(ctx)
	json.Unmarshal(f.req("GET", url+"/upload/status", nil, f.admin, "", 200), &out)
	if out["status"] != "complete" || string(files["/fw.sig"]) != payload || len(files) != 1 {
		t.Fatal(out, files)
	}
	f.s.devices[1].report["gcode_state"] = "RUNNING"
	f.req("POST", url+"/upload", nil, f.admin, "", 400)
	f.s.devices[1].report["gcode_state"] = "IDLE"
	files["/fw.sig"] = []byte("operator file")
	f.req("POST", url+"/upload", nil, f.admin, "", 200)
	f.s.tickFirmware(ctx)
	json.Unmarshal(f.req("GET", url+"/upload/status", nil, f.admin, "", 200), &out)
	if out["status"] != "error" || string(files["/fw.sig"]) != "operator file" || !strings.Contains(val(out["error"]), "verification") {
		t.Fatal(out, files)
	}
}
func TestMaterialDeficitAndCommitEvents(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	stock := map[int64]row{0: {"material_key": "PLA-red-A", "remaining_g": 30, "extruder": 0}, 1: {"material_key": "PLA-red-A", "remaining_g": 50, "extruder": 0}, 2: {"material_key": "PLA-red-A", "remaining_g": 100, "extruder": 1}}
	pl := plate{Filaments: []row{{"id": 1, "type": "PLA", "used_g": 50}, {"id": 2, "type": "PLA", "used_g": 25}}}
	if deficit(stock, pl, []int64{0, 1}, true) != "" || deficit(stock, pl, []int64{0, 1}, false) == "" {
		t.Fatal("invalid pooled estimate")
	}
	pl.Filaments[1]["used_g"] = 31
	if deficit(stock, pl, []int64{0, 1}, true) == "" {
		t.Fatal("pooled stock drawn from different extruder")
	}
	var events []map[string]any
	f.s.SetEventSink(func(e map[string]any) { events = append(events, e) })
	f.s.flushEvents(ctx)
	events = nil
	tx, e := f.s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec("UPDATE printers SET awaiting_plate_clear=1"); e != nil {
		t.Fatal(e)
	}
	tx.Rollback()
	f.s.flushEvents(ctx)
	if len(events) != 0 {
		t.Fatal("rollback published", events)
	}
	f.s.db.Exec("UPDATE printers SET awaiting_plate_clear=1 WHERE id=1")
	f.s.flushEvents(ctx)
	if len(events) != 1 || events[0]["type"] != "plate_clear_state" || num(events[0]["printer_id"]) != 1 || !flag(events[0]["awaiting_plate_clear"]) {
		t.Fatal(events)
	}
	f.s.flushEvents(ctx)
	if len(events) != 1 {
		t.Fatal("duplicate event", events)
	}
}
func TestAmsSlotWireAddressAndRFIDGate(t *testing.T) {
	f := farmFixture(t)
	pr := row{"model": "A2L"}
	rp := row{"ams": map[string]any{"ams": []any{map[string]any{"id": "16", "tray": []any{map[string]any{"id": "2", "tray_type": "PLA"}}}}}}
	a, e := resolveSlot(pr, rp, 6, 2)
	if e != nil || a.wire != 16 || a.tray != 2 || a.global != 66 || a.current["tray_type"] != "PLA" {
		t.Fatal(a, e)
	}
	a, e = resolveSlot(row{"model": "X1C"}, row{}, 2, 3)
	if e != nil || a.wire != 2 || a.global != 11 {
		t.Fatal(a, e)
	}
	f.s.devices[1].report["ams"] = map[string]any{"tray_now": "0", "ams": []any{map[string]any{"id": "0", "tray": []any{map[string]any{"id": "0", "tray_type": "PLA"}}}}}
	f.req("POST", "/printers/1/ams/0/slot/0/refresh", nil, f.admin, "", 409)
	f.s.devices[1].report["ams"].(map[string]any)["tray_now"] = "255"
	f.req("POST", "/printers/1/ams/0/slot/0/refresh", nil, f.admin, "", 200)
	got := f.req("POST", "/printers/1/slots/0/0/configure?tray_type=PLA&tray_color=112233&nozzle_temp_min=190&nozzle_temp_max=220", nil, f.admin, "", 200)
	if !bytes.Contains(got, []byte("success")) || !strings.Contains(jstr(f.hw.messages), `"tray_color":"112233FF"`) {
		t.Fatal(string(got), f.hw.messages)
	}
}
