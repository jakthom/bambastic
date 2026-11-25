package management

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"

	"testing"

	"bambastic/internal/access"
	"bambastic/internal/testdb"
)

type fixture struct {
	t   *testing.T
	db  *sql.DB
	mux *http.ServeMux
}

func setup(t *testing.T) fixture {
	t.Helper()
	db := testdb.Open(t)
	s, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	s.Register(mux)
	return fixture{t, db, mux}
}
func (f fixture) call(p access.Principal, method, path, payload string, status int) any {
	f.t.Helper()
	r := httptest.NewRequest(method, "/api/v1/"+path, bytes.NewBufferString(payload))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != status {
		f.t.Fatalf("%s %s: expected %d, got %d %s", method, path, status, w.Code, w.Body)
	}
	var v any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		f.t.Fatal(e)
	}
	return v
}
func (f fixture) exec(q string, args ...any) {
	f.t.Helper()
	if _, e := f.db.Exec(q, args...); e != nil {
		f.t.Fatal(e, q)
	}
}
func (f fixture) value(q string) float64 {
	f.t.Helper()
	var n float64
	if e := f.db.QueryRow(q).Scan(&n); e != nil {
		f.t.Fatal(e)
	}
	return n
}
func (f fixture) printer() {
	f.exec("INSERT INTO printers(id,name,serial_number,ip_address,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(1,'Printer','serial','localhost','code',1,1,1,0,0,0,0,0,0)")
}
func (f fixture) billedArchive() {
	f.exec("INSERT INTO print_archives(id,printer_id,filename,file_path,file_size,status,is_favorite,quantity,filament_used_grams,cost,plate_estimates) VALUES(1,1,'file.3mf','file.3mf',1,'completed',0,1,200,6,?)", `{"2":{"grams":50}}`)
	f.exec("INSERT INTO cost_centers(id,name,code,is_active,is_private,total_budget) VALUES(1,'Team','CC',1,0,50)")
	f.exec("INSERT INTO settings(key,value) VALUES('billing_enabled','true')")
}
func (f fixture) transaction(fn func(context.Context, *sql.Tx) error) {
	f.t.Helper()
	tx, e := f.db.Begin()
	if e != nil {
		f.t.Fatal(e)
	}
	defer tx.Rollback()
	if e = fn(access.With(context.Background(), access.Principal{Admin: true}), tx); e != nil {
		f.t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		f.t.Fatal(e)
	}
}
func TestWeighResetFailedPlateAndReplay(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	f.printer()
	f.billedArchive()
	f.call(p, "POST", "inventory/spools", `{"material":"PLA","label_weight":1000,"core_weight":250,"cost_per_kg":20}`, 200)
	f.call(p, "POST", "spoolbuddy/scale/update-spool-weight", `{"spool_id":1,"weight_grams":850}`, 200)
	f.call(p, "POST", "inventory/spools/1/reset-consumed-counter", "", 200)
	f.exec("INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(1,1,0,0)")
	f.exec("INSERT INTO print_log_entries(id,printer_id,archive_id,status,plate_id,cost_center_id,billing_run_id) VALUES(1,1,1,'printing',2,1,'physical-run-1')")
	f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SnapshotRun(ctx, tx, 1) })
	f.exec("UPDATE print_log_entries SET status='failed',completed_at=CURRENT_TIMESTAMP, filament_used_grams=20 WHERE id=1")
	settle := func(ctx context.Context, tx *sql.Tx) error { return SettleRun(ctx, tx, 1) }
	f.transaction(settle)
	f.transaction(settle)
	for q, want := range map[string]float64{"SELECT weight_used FROM spool": 420, "SELECT weight_used_baseline FROM spool": 400, "SELECT weight_locked FROM spool": 1, "SELECT count(*) FROM spool_usage_history": 1, "SELECT cost FROM spool_usage_history": .4, "SELECT count(*) FROM wallet_transactions": 1, "SELECT amount FROM wallet_transactions": -.6} {
		if got := f.value(q); got != want {
			t.Errorf("%s = %v, want %v", q, got, want)
		}
	}
	f.call(p, "DELETE", "finance/transactions/1", "", 200)
	f.transaction(settle)
	if f.value("SELECT count(*) FROM wallet_transactions WHERE NOT is_voided") != 0 {
		t.Fatal("replayed voided charge")
	}
}
func TestCommittedBudgetIncludesPendingCopiesAndDoesNotDoubleReserve(t *testing.T) {
	f := setup(t)
	f.printer()
	f.billedArchive()
	f.exec("INSERT INTO wallet_transactions(transaction_type,amount,is_voided,cost_center_id) VALUES('print_charge',-20,0,1)")
	for id := 1; id <= 2; id++ {
		f.exec("INSERT INTO print_queue(id,position,manual_start,require_previous_success,auto_off_after,been_jumped,gcode_injection,vibration_cali,layer_inspect,timelapse,use_ams,status,estimated_cost,cost_center_id) VALUES(?,?,0,0,0,0,0,0,0,0,1,'pending',10,1)", id, id)
	}
	f.exec("INSERT INTO budget_reservations(cost_center_id,source_type,source_id,amount,status) VALUES(1,'print_queue',1,10,'active')")
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		c, e := get(tx, "cost_centers", int64(1))
		if e != nil {
			return e
		}
		v, e := centerSummary(ctx, tx, c, true)
		if e != nil {
			return e
		}
		if num(v["budget_available"]) != 10 {
			t.Errorf("available = %v", v)
		}
		cost, e := AuthorizeQueue(ctx, tx, 0, 1, 1, 0, 2, 6)
		if e != nil || cost != 1.5 {
			t.Errorf("six plate copies %v, %v", cost, e)
		}
		if _, e = AuthorizeQueue(ctx, tx, 0, 1, 1, 0, 2, 7); e == nil {
			t.Error("overcommitted budget")
		}
		if _, e = AuthorizeQueue(access.With(ctx, access.Principal{ID: 3}), tx, 3, 1, 1, 0, 2, 1); e == nil {
			t.Error("nonmember authorized")
		}
		return nil
	})
}
func TestInventoryVisibilityAndBulkRollback(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	f.call(access.Principal{}, "GET", "inventory/spools", "", 403)
	f.call(p, "POST", "inventory/spools", `{"material":"PETG","tag_uid":"04:AB:CD"}`, 200)
	got := f.call(p, "GET", "inventory/spools/by-tag?tag_uid=04abcd", "", 200).(map[string]any)
	if got["tag_uid"] != "04ABCD" {
		t.Errorf("unexpected tag %v", got["tag_uid"])
	}
	f.call(p, "POST", "inventory/spools/bulk", `{"quantity":101,"spool":{"material":"PLA"}}`, 422)
	if f.value("SELECT count(*) FROM spool") != 1 {
		t.Fatal("partial bulk write")
	}
	f.call(p, "POST", "inventory/spools/1/archive", "", 200)
	f.call(p, "GET", "inventory/spools/by-tag?tag_uid=04abcd", "", 404)
	f.call(p, "GET", "inventory/spools/by-tag?tag_uid=04abcd&include_archived=true", "", 200)
}

func TestSpoolmanWeightsAndUsagePreserveNativeIdentity(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	f.printer()
	f.call(p, "POST", "inventory/spools", `{"material":"PLA","weight_used":46}`, 200)
	var uses int
	remaining := 800.
	remoteServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v1/spool/1":
			json.NewEncoder(w).Encode(row{"id": 1, "filament": row{"material": "PLA", "weight": 1000, "vendor": row{"empty_spool_weight": 200}}, "used_weight": 200, "remaining_weight": remaining, "price": 20})
		case r.Method == "PATCH" && r.URL.Path == "/api/v1/spool/1":
			var d row
			if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
				t.Error(err)
			}
			remaining = num(d["remaining_weight"])
			w.WriteHeader(204)
		case r.Method == "PUT" && r.URL.Path == "/api/v1/spool/1/use":
			var d row
			json.NewDecoder(r.Body).Decode(&d)
			uses++
			remaining -= num(d["use_weight"])
			w.WriteHeader(204)
		default:
			http.NotFound(w, r)
		}
	}))
	defer remoteServer.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_url',?)", remoteServer.URL)
	f.call(p, "POST", "spoolbuddy/scale/update-spool-weight", `{"spool_id":1,"weight_grams":850}`, 200)
	f.call(p, "POST", "spoolman/inventory/slot-assignments", `{"spoolman_spool_id":1,"printer_id":1,"ams_id":0,"tray_id":0}`, 200)
	f.exec("INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(1,1,0,0)")
	f.exec("INSERT INTO print_log_entries(id,printer_id,status) VALUES(1,1,'printing')")
	f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SnapshotRun(ctx, tx, 1) })
	f.exec("UPDATE print_log_entries SET status='completed',filament_used_grams=30,completed_at=CURRENT_TIMESTAMP WHERE id=1")
	for range 2 {
		f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SettleRun(ctx, tx, 1) })
	}
	svc := &Service{db: f.db}
	for range 2 {
		if err := svc.deliverUsage(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if uses != 1 || remaining != 620 {
		t.Errorf("remote weight = %v after %d use calls", remaining, uses)
	}
	if n := f.value("SELECT weight_used FROM spool WHERE id=1"); n != 46 {
		t.Errorf("native spool changed to %v", n)
	}
	if n := f.value("SELECT count(*) FROM spool_usage_history WHERE spoolman_spool_id=1 AND spool_id IS NULL AND cost=0.60"); n != 1 {
		t.Error("remote history or cost missing")
	}
	remoteServer.Close()
	f.call(p, "POST", "spoolbuddy/scale/update-spool-weight", `{"spool_id":1,"weight_grams":750}`, 503)
	if n := f.value("SELECT weight_used FROM spool WHERE id=1"); n != 46 {
		t.Errorf("remote outage changed native spool to %v", n)
	}
}

func TestStationWritePersistsAcrossHeartbeatsAndBindsOnlyCurrentOperation(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	f.call(p, "POST", "spoolbuddy/devices/register", `{"device_id":"lab","hostname":"pi","ip_address":"127.0.0.1"}`, 200)
	f.call(p, "POST", "inventory/spools", `{"material":"PLA","label_weight":750,"core_weight":210}`, 200)
	f.call(p, "POST", "spoolbuddy/nfc/write-tag", `{"device_id":"lab","spool_id":1}`, 200)
	beat := func() map[string]any {
		return f.call(p, "POST", "spoolbuddy/devices/lab/heartbeat", `{"nfc_ok":true}`, 200).(map[string]any)
	}
	pending := beat()["pending_write_payload"].(map[string]any)
	key := pending["write_id"].(string)
	if len(pending["ndef_data_hex"].(string))/2 != 129 || len(pending["ndef_v2_hex"].(string))/2 != 189 {
		t.Fatal("incorrect OpenTag lengths", pending)
	}
	if beat()["pending_command"] != "write_tag" {
		t.Fatal("heartbeat cleared an unacknowledged tag write")
	}
	f.call(p, "POST", "spoolbuddy/nfc/write-result", `{"device_id":"lab","spool_id":1,"tag_uid":"AABBCCDD","write_id":"old","success":true}`, 200)
	if f.value("SELECT count(*) FROM spool WHERE tag_uid IS NOT NULL") != 0 {
		t.Fatal("outdated operation linked a tag")
	}
	f.call(p, "POST", "spoolbuddy/nfc/write-result", `{"device_id":"lab","spool_id":1,"tag_uid":"AABBCCDD","write_id":"`+key+`","success":true}`, 200)
	if beat()["pending_command"] != nil || f.value("SELECT count(*) FROM spool WHERE tag_uid='AABBCCDD' AND encode_time IS NOT NULL") != 1 {
		t.Fatal("write completion missing")
	}
}

func TestMappedPlateFreezesMultipleSpoolsWithoutDebitingUnknownTray(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	f.printer()
	f.billedArchive()
	for i := 1; i <= 2; i++ {
		f.call(p, "POST", "inventory/spools", `{"material":"PLA","label_weight":1000}`, 200)
		f.exec("INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(?,1,0,?)", i, i-1)
	}
	f.exec("UPDATE print_archives SET extra_data=?", `{"plates":[{"index":2,"filaments":[{"id":1,"used_g":20},{"id":2,"used_g":70},{"id":3,"used_g":10}]}]}`)
	f.exec("INSERT INTO print_queue(id,position,manual_start,require_previous_success,auto_off_after,been_jumped,gcode_injection,vibration_cali,layer_inspect,timelapse,use_ams,status,estimated_cost,cost_center_id,ams_mapping,archive_id,plate_id) VALUES(1,1,0,0,0,0,0,0,0,0,1,'pending',5,1,'[1,0,3]',1,2)")
	f.exec("UPDATE cost_centers SET total_budget=2")
	f.transaction(func(ctx context.Context, tx *sql.Tx) error { return RecheckQueue(ctx, tx, p, 1) })
	if x := f.value("SELECT estimated_cost FROM print_queue"); x != 1.5 {
		t.Errorf("recheck did not replace own commitment, got %v", x)
	}
	f.exec("INSERT INTO print_log_entries(id,printer_id,queue_item_id,archive_id,status,plate_id,cost_center_id,billing_run_id) VALUES(1,1,1,1,'printing',2,1,'split')")
	f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SnapshotRun(ctx, tx, 1) })
	f.exec("UPDATE print_log_entries SET filament_used_grams=100,completed_at=CURRENT_TIMESTAMP,status='completed'")
	for range 2 {
		f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SettleRun(ctx, tx, 1) })
	}
	if f.value("SELECT weight_used FROM spool WHERE id=1") != 70 || f.value("SELECT weight_used FROM spool WHERE id=2") != 20 || f.value("SELECT sum(weight_used) FROM spool_usage_history") != 90 {
		t.Fatal("incorrect per-tray attribution")
	}
}
func TestRemoteCreateAndEditUseSelectedFilamentAndNetSpoolWeight(t *testing.T) {
	f := setup(t)
	p := access.Principal{Admin: true}
	sp := row{"id": 7, "used_weight": 0, "filament": row{"id": 42, "material": "PETG", "weight": 1000}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/spool" && r.URL.Path != "/api/v1/spool/7" {
			t.Error(r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if r.Method != "GET" {
			var d row
			if e := json.NewDecoder(r.Body).Decode(&d); e != nil {
				t.Error(e)
			}
			if r.Method == "POST" && intID(d["filament_id"]) != 42 {
				t.Error("preselected filament ignored")
			}
			for k, v := range d {
				sp[k] = v
			}
		}
		json.NewEncoder(w).Encode(sp)
	}))
	defer server.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_url',?)", server.URL)
	v := f.call(p, "POST", "spoolman/inventory/spools", `{"spoolman_filament_id":42,"label_weight":250,"core_weight":0,"weight_used":30,"cost_per_kg":20}`, 200).(map[string]any)
	if v["label_weight"] != 250. || v["weight_used"] != 30. || v["core_weight"] != 0. || v["cost_per_kg"] != 20. {
		t.Fatal("wrong stock conversion", v)
	}
	v = f.call(p, "PATCH", "spoolman/inventory/spools/7", `{"weight_used":70,"note":"Makerspace shelf"}`, 200).(map[string]any)
	if v["weight_used"] != 70. || v["note"] != "Makerspace shelf" || f.value("SELECT count(*) FROM spool") != 0 {
		t.Fatal("remote patch", v)
	}
}
