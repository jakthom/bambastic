package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAMSBackgroundAssignmentAndRemoval(t *testing.T) {
	f := setup(t)
	f.printer()
	f.exec("INSERT INTO settings(key,value) VALUES('auto_add_unknown_rfid','false')")
	f.exec("INSERT INTO spool(material,subtype,brand,rgba,label_weight,core_weight,weight_used,weight_locked) VALUES('PLA',NULL,NULL,'112233FF',1000,0,0,0),('PLA','Basic','Bambu Lab','112233FF',1000,0,0,0),('PLA','Basic','Other','112233FF',1000,0,0,0)")
	tray := row{"id": "0", "tray_type": "PLA", "tray_sub_brands": "PLA Basic", "tray_color": "112233FF", "remain": 85., "tag_uid": "12345678", "tray_info_idx": "GFL01"}
	unit := row{"id": "0", "tray": []any{tray}}
	ams := map[string]any{"ams": []any{unit}, "tray_exist_bits": "1"}
	report := map[string]any{"ams": ams, "gcode_state": "IDLE"}
	s := &Service{db: f.db, MaterialSlots: func(id int64) (map[string]any, bool) { return report, id == 1 }}
	var prompts []row
	s.SetEventSink(func(v map[string]any) {
		if v["type"] == "unknown_tag" {
			prompts = append(prompts, row(v))
		}
	})
	seen := map[slotKey]trayObservation{}
	clock := time.Unix(1500000000, 0)
	step := func(seconds int) {
		t.Helper()
		clock = clock.Add(time.Duration(seconds) * time.Second)
		if e := s.observeAMS(context.Background(), clock, seen); e != nil {
			t.Fatal(e)
		}
	}
	step(0)
	if f.value("SELECT spool_id FROM spool_assignment") != 2 || f.value("SELECT weight_used FROM spool WHERE id=2") != 150 || f.value("SELECT count(*) FROM slot_preset_mappings WHERE preset_id='GFSL01'") != 1 {
		t.Fatal("RFID did not match best untagged inventory, weight or preset")
	}
	f.exec("UPDATE spool SET weight_locked=1,weight_used=25 WHERE id=2")
	tray["remain"] = 60
	step(5)
	if f.value("SELECT weight_used FROM spool WHERE id=2") != 25 {
		t.Fatal("overrode locked scale measurement")
	}
	report["gcode_state"] = "RUNNING"
	ams["tray_exist_bits"] = "0"
	step(5)
	step(125)
	if f.value("SELECT count(*) FROM spool_assignment") != 1 {
		t.Fatal("running spool lost")
	}
	report["gcode_state"] = "IDLE"
	step(5)
	step(100)
	ams["tray_exist_bits"] = "1"
	step(5)
	ams["tray_exist_bits"] = "0"
	step(5)
	step(90)
	unit["tray"] = []any{row{"id": "0"}}
	delete(ams, "tray_exist_bits")
	step(60)
	if f.value("SELECT count(*) FROM spool_assignment") != 1 {
		t.Fatal("transient or partial report unlinked an occupied spool")
	}
	unit["tray"] = []any{tray}
	ams["tray_exist_bits"] = "0"
	step(1)
	step(121)
	if f.value("SELECT count(*) FROM spool_assignment") != 0 {
		t.Fatal("sustained empty left assignment")
	}
	ams["tray_exist_bits"] = "1"
	tray["tray_type"] = "PETG"
	tray["tray_sub_brands"] = "PETG HF"
	tray["tag_uid"] = "ABCDEF22"
	step(5)
	step(65)
	if len(prompts) != 1 || num(prompts[0]["tray_count"]) != 1 || prompts[0]["tag_uid"] != "ABCDEF22" {
		t.Fatal(prompts)
	}
	ams["tray_exist_bits"] = "0"
	step(5)
	ams["tray_exist_bits"] = "1"
	step(5)
	if len(prompts) != 2 {
		t.Fatal("reinserted unknown tag did not re-prompt")
	}
	f.exec("UPDATE settings SET value='true' WHERE key='auto_add_unknown_rfid'")
	step(5)
	step(65)
	if f.value("SELECT count(*) FROM spool WHERE tag_uid='ABCDEF22'") != 1 || f.value("SELECT count(*) FROM spool_assignment") != 1 {
		t.Fatal("failed to create exactly one new roll")
	}
	f.exec("UPDATE spool SET archived_at=CURRENT_TIMESTAMP WHERE tag_uid='ABCDEF22'")
	f.exec("DELETE FROM spool_assignment")
	step(65)
	if f.value("SELECT count(*) FROM spool WHERE tag_uid='ABCDEF22'") != 1 || f.value("SELECT count(*) FROM spool_assignment") != 0 || len(prompts) != 2 {
		t.Fatal("adopted or re-prompted archived roll", prompts)
	}
}

func TestAMSSpoolmanManualAndAutomaticBind(t *testing.T) {
	f := setup(t)
	f.printer()
	f.db.SetMaxOpenConns(1)
	reads, writes := 0, 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			writes++
			http.Error(w, "unexpected write", 405)
			return
		}
		// A remote inventory read must leave the writer available for the open or close of a run.
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		var one int
		if e := f.db.QueryRowContext(ctx, "SELECT 1").Scan(&one); e != nil {
			t.Error(e)
			w.WriteHeader(503)
			return
		}
		reads++
		json.NewEncoder(w).Encode([]row{{"id": 12, "extra": row{"tag": `"12345678"`}, "filament": row{"name": "PLA Basic"}}, {"id": 13, "archived": true, "extra": row{"tag": `"99999999"`}}})
	}))
	defer remote.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_sync_mode','manual'),('spoolman_url',?)", remote.URL)
	tray := row{"id": "0", "tray_type": "PLA", "tray_color": "AABBCCFF", "tag_uid": "1234567800000100", "tray_info_idx": "GFL01", "remain": 20.}
	other := row{"id": "1", "tray_type": "PLA", "tag_uid": "99999999", "tray_info_idx": "GFL01"}
	report := map[string]any{"ams": []any{row{"id": "0", "tray": []any{tray, other}}}, "gcode_state": "IDLE"}
	s := &Service{db: f.db, MaterialSlots: func(int64) (map[string]any, bool) { return report, true }}
	now := time.Now()
	seen := map[slotKey]trayObservation{}
	step := func(seconds int) {
		t.Helper()
		now = now.Add(time.Duration(seconds) * time.Second)
		if e := s.observeAMS(context.Background(), now, seen); e != nil {
			t.Fatal(e)
		}
	}
	step(0)
	if reads != 0 {
		t.Fatal("polling in manual mode", reads)
	}
	f.exec("UPDATE settings SET value='auto' WHERE key='spoolman_sync_mode'")
	step(5)
	step(5)
	if reads != 1 || writes != 0 || f.value("SELECT count(*) FROM spoolman_slot_assignments") != 1 || f.value("SELECT spoolman_spool_id FROM spoolman_slot_assignments") != 12 || f.value("SELECT count(*) FROM spool") != 0 {
		t.Fatal("automatic Spoolman reconciliation", reads, writes)
	}
	f.exec("INSERT INTO print_log_entries(id,printer_id,status) VALUES(9,1,'printing')")
	tray["exists"] = false
	step(130)
	if f.value("SELECT count(*) FROM spoolman_slot_assignments") != 1 {
		t.Fatal("unlinked open run on IDLE report")
	}
	f.exec("DELETE FROM print_log_entries")
	step(5)
	step(121)
	if f.value("SELECT count(*) FROM spoolman_slot_assignments") != 0 || writes != 0 {
		t.Fatal("empty remote holder did not settle")
	}
}

func TestModelSpecificTrayAddresses(t *testing.T) {
	s := &Service{MaterialSlots: func(int64) (map[string]any, bool) {
		return map[string]any{"gcode_state": "IDLE", "ams": map[string]any{"tray_exist_bits": "1010000", "ams": []any{row{"id": "16", "tray": []any{row{"id": "0", "tray_type": "PLA"}}}, row{"id": "128", "tray": []any{row{"id": "0", "tray_type": "PLA"}}}, row{"id": "32", "tray": []any{row{"id": "1", "tray_type": "ABS"}}}}}, "vt_tray": []any{row{"id": "251", "tray_type": "ABS"}, row{"id": "255", "tray_type": "PETG"}}}, true
	}}
	slots, _, _ := s.slots(4)
	if slots[slotKey{4, 6, 0}]["exists"] != true || slots[slotKey{4, 128, 0}]["exists"] != true || slots[slotKey{4, 32, 1}]["exists"] != false || slots[slotKey{4, 255, 6}]["tray_type"] != "ABS" || slots[slotKey{4, 255, 1}]["tray_type"] != "PETG" || slotAddress(32, 7) != 35 {
		t.Fatal(slots)
	}
}

func TestRemoteRFIDAutoCreateCarriesTagOnOriginalWrite(t *testing.T) {
	f := setup(t)
	f.printer()
	var spools []row
	adds := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/spool":
			json.NewEncoder(w).Encode(spools)
		case "GET /api/v1/filament":
			json.NewEncoder(w).Encode([]row{{"id": 3, "name": "PLA", "material": "PLA", "vendor": row{"name": "Bambu Lab"}, "color_hex": "112233"}})
		case "GET /api/v1/field/spool":
			json.NewEncoder(w).Encode([]row{{"key": "tag"}, {"key": "bambu_slicer_filament"}})
		case "POST /api/v1/spool":
			var v row
			json.NewDecoder(r.Body).Decode(&v)
			if extraText(sub(v["extra"]), "tag") != "1234ABCDEFFF8765" {
				t.Errorf("tag missing from create: %v", v)
			}
			v["id"] = 42
			spools = append(spools, v)
			adds++
			json.NewEncoder(w).Encode(v)
		default:
			t.Error(r.Method, r.URL)
			w.WriteHeader(400)
		}
	}))
	defer srv.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_url',?)", srv.URL)
	report := map[string]any{"gcode_state": "IDLE", "ams": []any{row{"id": 0, "tray": []any{row{"id": 0, "tray_type": "PLA", "tray_color": "112233FF", "tray_info_idx": "GFL01", "tag_uid": "1234ABCDEFFF8765"}}}}}
	s := &Service{db: f.db, MaterialSlots: func(int64) (map[string]any, bool) { return report, true }}
	ctx := context.Background()
	if e := s.observeAMS(ctx, time.Now(), map[slotKey]trayObservation{}); e != nil {
		t.Fatal(e)
	}
	if e := s.observeAMS(ctx, time.Now(), map[slotKey]trayObservation{}); e != nil {
		t.Fatal(e)
	}
	if adds != 1 || f.value("SELECT spoolman_spool_id FROM spoolman_slot_assignments") != 42 {
		t.Fatal("duplicate auto-registration", adds)
	}
}
