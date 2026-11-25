package production

import (
	"bambastic/internal/access"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWebhookAuthorityUsesNormalAdmission(t *testing.T) {
	f := farmFixture(t)
	aid := f.upload(f.admin)
	if _, e := f.s.db.Exec("INSERT INTO users(id,username,role,auth_source,is_active) VALUES(7,'modeler','user','local',1)"); e != nil {
		t.Fatal(e)
	}
	key := access.Principal{ID: 7, KeyID: 5, PrinterIDs: map[int64]bool{1: true}, Permissions: map[string]bool{"printers:read": true, "printers:control": true, "queue:create": true, "queue:read_own": true, "queue:update_own": true, "archives:reprint_all": true, "archives:read_all": true}}
	f.s.SetUserResolver(func(context.Context, int64) (access.Principal, error) { return key, nil })
	auth, e := access.New(f.s.db, f.s.dir)
	if e != nil {
		t.Fatal(e)
	}
	f.s.auth = auth
	if _, e = f.s.db.Exec("UPDATE users SET role='admin' WHERE id=7; INSERT INTO api_keys(id,name,key_hash,key_prefix,user_id,can_queue,can_control_printer,can_read_status,printer_ids,enabled) VALUES(5,'automation','hash','prefix',7,1,1,1,'[1]',1)"); e != nil {
		t.Fatal(e)
	}
	request := row{"printer_id": 1, "archive_id": aid}
	f.req("POST", "/webhook/queue/add", request, f.admin, "", 401)
	f.req("POST", "/webhook/queue/add", request, key, "hook", 200)
	f.req("POST", "/webhook/queue/add", request, key, "hook", 200)
	f.req("POST", "/webhook/printer/1/start", nil, key, "", 403)
	f.req("GET", "/webhook/printer/2/status", nil, key, "", 404)
	out := string(f.req("GET", "/webhook/queue", nil, key, "", 200))
	if !strings.Contains(out, `"pending":1`) {
		t.Fatal(out)
	}
	out = string(f.req("GET", "/bulk/printer-queues?ids=1,2", nil, key, "", 200))
	if !strings.Contains(out, `"1":`) || strings.Contains(out, `"2":`) {
		t.Fatal(out)
	}
	f.req("POST", "/queue/1/start", nil, f.admin, "", 200)
	if _, e = f.s.db.Exec("UPDATE api_keys SET enabled=0 WHERE id=5"); e != nil {
		t.Fatal(e)
	}
	if job, e := f.s.claim(t.Context(), f.pr); e != nil || job != nil {
		t.Fatal("revoked admission", job, e)
	}
	if _, e = f.s.db.Exec("UPDATE api_keys SET enabled=1 WHERE id=5"); e != nil {
		t.Fatal(e)
	}
	job, e := f.s.claim(context.Background(), f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
}
func TestAlternativeCanWinOneOrder(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	for _, part := range []struct{ name, model string }{{"a.gcode", "H2D"}, {"b.gcode", "X1C"}} {
		v := f.file("/library/files", part.name, "; valid gcode", f.admin, 200)
		d, _ := json.Marshal(plateInfo{Plates: []plate{{Index: 1, Printable: true}}, Printable: true, Model: part.model})
		if _, e := f.s.db.Exec("UPDATE library_files SET file_metadata=? WHERE id=?", string(d), num(v["id"])); e != nil {
			t.Fatal(e)
		}
	}
	members := []row{{"library_file_id": 1}, {"library_file_id": 2}}
	f.req("POST", "/library/variant-groups", row{"members": members, "name": "Bracket"}, f.admin, "", 200)
	f.req("POST", "/library/variant-groups", row{"members": members}, f.admin, "", 409)
	f.req("POST", "/queue/", row{"variants": members, "quantity": 1}, f.admin, "variants", 200)
	got := string(f.req("GET", "/queue/1", nil, f.admin, "", 200))
	if !strings.Contains(got, "H2D") || !strings.Contains(got, "X1C") {
		t.Fatal(got)
	}
	job, e := f.s.claim(ctx, f.pr)
	if e != nil || job == nil || num(job["library_file_id"]) != 2 {
		t.Fatal(job, e)
	}
	f.s.send(ctx, job)
	if again, e := f.s.claim(ctx, f.pr); e != nil || again != nil {
		t.Fatal("claimed one demand twice", again, e)
	}
	f.req("DELETE", "/library/variant-groups/1", nil, f.admin, "", 200)
	var count int
	if e = f.s.db.QueryRow("SELECT count(*) FROM print_queue_variants WHERE queue_item_id=1").Scan(&count); e != nil || count != 2 {
		t.Fatal("saved job lost its alternatives", count, e)
	}
}
func TestTelemetryInterlockAndLogAmendment(t *testing.T) {
	f := farmFixture(t)
	aid := f.upload(f.admin)
	f.req("POST", "/queue/", row{"archive_id": aid, "printer_id": 1}, f.admin, "", 200)
	if _, e := f.s.db.Exec("INSERT INTO printer_ha_sensors(printer_id,name,entity_id,kind,alert_state,block_print,last_state,last_checked) VALUES(1,'Door','binary_sensor.door','binary','on',1,'on',CURRENT_TIMESTAMP)"); e != nil {
		t.Fatal(e)
	}
	job, e := f.s.claim(context.Background(), f.pr)
	if e != nil || job != nil {
		t.Fatal("blocking sensor ignored", job, e)
	}
	if _, e = f.s.db.Exec("UPDATE printer_ha_sensors SET last_state='off'"); e != nil {
		t.Fatal(e)
	}
	job, e = f.s.claim(context.Background(), f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
	f.s.send(context.Background(), job)
	for _, st := range []string{"RUNNING", "FINISH"} {
		if e = f.s.observe(context.Background(), f.pr, row{"gcode_state": st, "gcode_file": job["remote"]}); e != nil {
			t.Fatal(e)
		}
	}
	f.req("PATCH", "/print-log/1", row{"status": "failed", "failure_reason": "adhesionFailure", "notes": "inspection"}, f.admin, "", 200)
	f.req("POST", "/queue/history/clear", nil, f.admin, "", 200)
	got := string(f.req("GET", "/queue/history", nil, f.admin, "", 200))
	if !strings.Contains(got, `"total":0`) {
		t.Fatal(got)
	}
	f.req("DELETE", "/print-log/1", nil, f.admin, "", 200)
}
func TestFirmwareReleaseParsingAndCache(t *testing.T) {
	versions := releaseVersions(`<h2 id="h-01020304-20260101"><h2 id="ota-01030405">`)
	if len(versions) != 2 || versions[0] != "01.03.04.05" {
		t.Fatal(versions)
	}
	f := farmFixture(t)
	f.s.devices[1].report["firmware_version"] = "01.02.03.04"
	if _, e := f.s.db.Exec("INSERT INTO bb_firmware_cache VALUES('X1C',?,?)", versions[0], time.Now().Unix()+120); e != nil {
		t.Fatal(e)
	}
	if got := string(f.req("GET", "/bulk/firmware-updates?ids=1", nil, f.admin, "", 200)); !strings.Contains(got, `"update_available":true`) {
		t.Fatal(got)
	}
}
