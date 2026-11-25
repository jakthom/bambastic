package operations

import (
	"bambastic/internal/access"
	"context"

	"bambastic/internal/testdb"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"

	"strings"
	"testing"
	"time"
)

type rig struct {
	t *testing.T
	s *Service
	m *http.ServeMux
}

func setup(t *testing.T) rig {
	t.Helper()
	db := testdb.Open(t)
	s, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	m := http.NewServeMux()
	s.Register(m)
	return rig{t, s, m}
}
func (f rig) call(p access.Principal, method, path, body string, code int) any {
	f.t.Helper()
	r := httptest.NewRequest(method, "/api/v1/"+path, strings.NewReader(body))
	w := httptest.NewRecorder()
	f.m.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != code {
		f.t.Fatalf("%s %s: expected %d, got %d %s", method, path, code, w.Code, w.Body)
	}
	var result any
	if e := json.Unmarshal(w.Body.Bytes(), &result); e != nil {
		f.t.Fatal(e)
	}
	return result
}
func (f rig) exec(q string, a ...any) {
	f.t.Helper()
	if _, e := f.s.db.Exec(q, a...); e != nil {
		f.t.Fatal(e)
	}
}
func (f rig) printer() {
	f.exec("INSERT INTO printers(id,name,serial_number,ip_address,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(1,'Hana','serial','127.0.0.1','code',1,1,1,0,0,0,0,0,0)")
}
func TestScopedPowerAndLifecycle(t *testing.T) {
	f := setup(t)
	f.printer()
	ctx := context.Background()
	admin := access.Principal{Admin: true}
	calls := []string{}
	actual := "on"
	ha := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer abc" {
			t.Error("HA credential missing")
		}
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/states/switch.printer":
			fmt.Fprintf(w, `{"state":%q}`, actual)
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/api/services/switch/turn_"):
			b, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(b), "switch.printer") {
				t.Error("Missing entity")
			}
			actual = strings.TrimPrefix(r.URL.Path, "/api/services/switch/turn_")
			calls = append(calls, actual)
			fmt.Fprint(w, `[]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ha.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('ha_url',?)", ha.URL)
	f.s.ReadSecret = func(context.Context, string) (string, error) { return "abc", nil }
	f.call(admin, "POST", "smart-plugs/", `{"name":"Main outlet","plug_type":"homeassistant","ha_entity_id":"switch.printer","printer_id":1}`, 200)
	f.call(admin, "PATCH", "smart-plugs/1", `{"auto_off":true,"off_delay_minutes":0,"auto_on":false}`, 200)
	v := f.call(admin, "GET", "smart-plugs/1/status", "", 200).(obj)
	if v["state"] != "ON" || v["reachable"] != true {
		t.Fatal(v)
	}
	restricted := access.Principal{Permissions: map[string]bool{"smart_plugs:control": true}, PrinterIDs: map[int64]bool{2: true}}
	f.call(restricted, "POST", "smart-plugs/1/control", `{"action":"off"}`, 404)
	f.exec("INSERT INTO print_log_entries(id,printer_id,status,print_name,completed_at) VALUES(1,1,'failed','A',CURRENT_TIMESTAMP)")
	evt := obj{"type": "print_complete", "printer_id": 1, "data": obj{"run_id": 1, "status": "failed"}}
	f.s.Publish(evt)
	if e := f.s.process(ctx); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 0 {
		t.Fatal("powered off after failure")
	}
	f.exec("UPDATE print_log_entries SET status='completed' WHERE id=1")
	evt["data"] = obj{"run_id": 1, "status": "completed"}
	f.s.Publish(evt)
	f.s.Publish(obj{"type": "printer_status", "printer_id": 1, "data": obj{"connected": true, "state": "RUNNING"}})
	if e := f.s.process(ctx); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 0 {
		t.Fatal("powered off during new print")
	}
	f.s.Publish(obj{"type": "printer_status", "printer_id": 1, "data": obj{"connected": true, "state": "IDLE", "temperatures": obj{"nozzle": 40}}})
	if e := f.s.process(ctx); e != nil {
		t.Fatal(e)
	}
	if fmt.Sprint(calls) != "[off]" {
		t.Fatal(calls)
	}
	f.s.Publish(evt)
	if e := f.s.process(ctx); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 1 {
		t.Fatal("duplicate power action")
	}
}
func TestNotificationsMaskScopeDeliverAndReplay(t *testing.T) {
	f := setup(t)
	f.printer()
	p := access.Principal{Admin: true}
	ctx := context.Background()
	var payloads []obj
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token" {
			t.Error("missing auth_header")
		}
		var v obj
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
			t.Error(e)
		}
		payloads = append(payloads, v)
		w.WriteHeader(204)
	}))
	defer receiver.Close()
	in := fmt.Sprintf(`{"name":"Hook","provider_type":"webhook","config":{"webhook_url":%q,"auth_header":"token"},"enabled":true,"on_print_complete":true}`, receiver.URL)
	v := f.call(p, "POST", "notifications/", in, 200).(obj)
	if sub(v["config"])["webhook_url"] != mask || sub(v["config"])["auth_header"] != mask {
		t.Fatal("leaked stored credential")
	}
	f.call(p, "PATCH", "notifications/1", `{"config":{"webhook_url":"https://example.com","auth_header":"********"}}`, 422)
	restricted := access.Principal{Permissions: map[string]bool{"notifications:read": true}, PrinterIDs: map[int64]bool{1: true}}
	f.call(restricted, "GET", "notifications/1", "", 404)
	f.exec("INSERT INTO print_log_entries(id,printer_id,printer_name,status,print_name,filament_used_grams,completed_at) VALUES(1,1,'Hana','completed','Fixture',35,CURRENT_TIMESTAMP)")
	for range 2 {
		f.s.Publish(obj{"type": "print_complete", "printer_id": 1, "data": obj{"run_id": 1, "status": "completed"}})
		if e := f.s.process(ctx); e != nil {
			t.Fatal(e)
		}
	}
	if len(payloads) != 1 || payloads[0]["event"] != "print_complete" || !strings.Contains(str(payloads[0]["message"]), "Hana: Fixture") {
		t.Fatalf("invalid payloads %v", payloads)
	}
	logs := f.call(p, "GET", "notifications/logs/stats", "", 200).(obj)
	if logs["success_count"] != float64(1) {
		t.Fatal(logs)
	}
	f.call(p, "POST", "notifications/1/test", "", 200)
	if len(payloads) != 2 {
		t.Fatal("missing test delivery")
	}
	for _, at := range []string{"23:59", "00:00", "05:59", "22:00"} {
		tm, _ := time.Parse("15:04", at)
		if !quiet(obj{"quiet_hours_enabled": true, "quiet_hours_start": "22:00", "quiet_hours_end": "06:00"}, tm) {
			t.Fatalf("quiet hours at %s", at)
		}
	}
}

func TestSensorInterlockAndScope(t *testing.T) {
	f := setup(t)
	admin := access.Principal{Admin: true}
	f.printer()
	var e error
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/states/binary_sensor.door" || r.Header.Get("Authorization") != "Bearer identity" {
			http.Error(w, "unexpected request", 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"state":"on"}`)
	}))
	defer server.Close()
	t.Setenv("HA_URL", server.URL)
	t.Setenv("HA_TOKEN", "identity")
	f.call(admin, "POST", "ha-sensors/", `{"printer_id":1,"name":"Door","kind":"binary","entity_id":"binary_sensor.door","alert_state":"on","block_print":true}`, 200)
	f.s.pollSensors(t.Context())
	held := func() bool {
		tx, e := f.s.db.Begin()
		if e != nil {
			t.Fatal(e)
		}
		defer tx.Rollback()
		h, e := SensorHold(t.Context(), tx, 1)
		if e != nil {
			t.Fatal(e)
		}
		return h
	}
	if !held() {
		t.Fatal("reachable alerting sensor did not hold dispatch")
	}
	restricted := access.Principal{Permissions: map[string]bool{"smart_plugs:read": true}, PrinterIDs: map[int64]bool{2: true}}
	f.call(restricted, "GET", "ha-sensors/1", "", 404)
	if _, e = f.s.db.Exec("UPDATE printer_ha_sensors SET last_state='unavailable'"); e != nil {
		t.Fatal(e)
	}
	if held() {
		t.Fatal("unavailable sensor remained in an active alert")
	}
}

func TestWLEDScopedPresetsAndLifecycle(t *testing.T) {
	f := setup(t)
	f.printer()
	var applied []int
	device := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/presets.json":
			io.WriteString(w, `{"0":{},"7":{"n":" Ocean "},"3":{}}`)
		case "/json/info":
			io.WriteString(w, `{"name":"Farm","ver":"0.15"}`)
		case "/json/state":
			var d struct{ PS int }
			if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&d) != nil {
				t.Error("invalid state command")
			}
			applied = append(applied, d.PS)
			io.WriteString(w, `{"success":true}`)
		default:
			t.Error(r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer device.Close()
	body := fmt.Sprintf(`{"base_url":%q}`, device.URL)
	admin := access.Principal{Admin: true}
	list := f.call(admin, "POST", "printers/1/wled/presets", body, 200).([]any)
	if len(list) != 2 || list[0].(obj)["name"] != "3" || list[1].(obj)["name"] != "Ocean" {
		t.Fatal(list)
	}
	outside := access.Principal{Permissions: map[string]bool{"printers:update": true}, PrinterIDs: map[int64]bool{2: true}}
	f.call(outside, "POST", "printers/1/wled/test-connection", body, 404)
	f.call(admin, "POST", "printers/1/wled/test-preset", fmt.Sprintf(`{"base_url":%q,"preset_id":251}`, device.URL), 422)
	cfg := fmt.Sprintf(`{"enabled":true,"base_url":%q,"presets":{"finished":5,"idle":3},"finished_timeout_seconds":3}`, device.URL)
	f.exec("UPDATE printers SET wled_config=?", cfg)
	f.s.Publish(obj{"type": "printer_status", "printer_id": 1, "data": obj{"connected": true, "state": "FINISH"}})
	states := map[int64]lightState{}
	start := time.Now()
	for _, d := range []time.Duration{0, time.Second, 4 * time.Second, 7 * time.Second} {
		f.s.syncLights(context.Background(), states, start.Add(d))
	}
	if fmt.Sprint(applied) != "[5 3]" {
		t.Fatal(applied)
	}
}

func TestPlugDiscoveryAndCredentialBoundProbe(t *testing.T) {
	f := setup(t)
	admin := access.Principal{Admin: true}
	calls := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("cmnd") {
		case "Power":
			io.WriteString(w, `{"POWER":"ON"}`)
		case "Status 0":
			io.WriteString(w, `{"Status":{"DeviceName":"Bench","Module":4}}`)
		default:
			if r.Header.Get("Authorization") != "Bearer private" {
				t.Error("missing probe credential")
			}
			calls++
		}
	}))
	defer host.Close()
	v := probePlug(context.Background(), host.URL, "192.168.1.5")
	if v["name"] != "Bench" || v["state"] != "ON" {
		t.Fatal(v)
	}
	f.call(admin, "POST", "smart-plugs/discover/scan", `{"from_ip":"8.8.8.8","to_ip":"8.8.8.8"}`, 422)
	hosts, e := discoverRange("192.168.0.254", "192.168.1.0")
	if e != nil || len(hosts) != 3 {
		t.Fatal(hosts, e)
	}
	req := fmt.Sprintf(`{"name":"Probe","plug_type":"rest","rest_status_url":%q,"rest_headers":"{\"Authorization\":\"Bearer private\"}"}`, host.URL+"/power")
	f.call(admin, "POST", "smart-plugs/", req, 200)
	probe := func(u string) string {
		return fmt.Sprintf(`{"url":%q,"method":"GET","headers":"********","plug_id":1}`, u)
	}
	f.call(admin, "POST", "smart-plugs/rest/test-connection", probe(host.URL+"/other"), 400)
	f.call(admin, "POST", "smart-plugs/rest/test-connection", probe(host.URL+"/power"), 200)
	if calls != 1 {
		t.Fatal(calls)
	}
}
func TestPrintOwnerNotificationsHonorPreferencesAndDeduplicate(t *testing.T) {
	f := setup(t)
	f.printer()
	ctx := context.Background()
	f.exec("INSERT INTO users(id,username,password_hash,role,is_active,email,auth_source) VALUES(1,'owner','test','user',1,'owner@example.test','local')")
	f.exec("INSERT INTO settings(key,value) VALUES('advanced_auth_enabled','true'),('smtp_host','localhost'),('external_url','https://farm.example.test')")
	f.exec("INSERT INTO print_log_entries(id,printer_id,printer_name,print_name,status,created_by_id,confirm_token) VALUES(1,1,'Bench','Bracket','completed',1,'single-use-verdict')")
	emails := 0
	f.s.AccountMail = func(_ context.Context, to, title, body string) error {
		emails++
		if to != "owner@example.test" || !strings.Contains(body, "https://farm.example.test/confirm/single-use-verdict/good") {
			t.Fatal(to, body)
		}
		return nil
	}
	event := obj{"type": "print_complete", "printer_id": 1, "data": obj{"run_id": 1, "status": "completed"}}
	f.s.Publish(event)
	f.s.Publish(event)
	if e := f.s.personalEmail(ctx); e != nil {
		t.Fatal(e)
	}
	f.exec("INSERT INTO user_email_preferences(user_id,notify_print_start,notify_print_complete,notify_print_failed,notify_print_stopped) VALUES(1,0,1,0,1)")
	event["data"] = obj{"run_id": 1, "status": "failed"}
	f.s.Publish(event)
	if e := f.s.personalEmail(ctx); e != nil {
		t.Fatal(e)
	}
	if emails != 1 {
		t.Fatal(emails)
	}
}
