package production

import (
	"bambastic/internal/access"
	"bambastic/internal/management"
	"context"
	"encoding/json"
	"errors"
	"testing"
)

func TestNativeAssignmentPublishesAfterCommitAndReplaysOnInsertion(t *testing.T) {
	f := farmFixture(t)
	m, e := management.New(f.s.db)
	if e != nil {
		t.Fatal(e)
	}
	m.ConfigureNativeAssignment = f.s.ConfigureNativeAssignment
	m.Register(f.mux)
	sp := f.req("POST", "/inventory/spools", row{"material": "PLA", "brand": "Acme", "subtype": "Silk", "rgba": "CC99DDFF", "label_weight": 1000}, f.admin, "", 200)
	var clean row
	json.Unmarshal(sp, &clean)
	spool := num(clean["id"])
	d := f.s.devices[1]
	slot := map[string]any{"id": "0", "state": 9, "tray_type": ""}
	raw := map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{slot}}}, "tray_exist_bits": "0"}
	d.kReport = row{"gcode_state": "IDLE", "ams": raw, "nozzle_diameter": "0.4"}
	post := func() row {
		var v row
		if e := json.Unmarshal(f.req("POST", "/inventory/assignments", row{"spool_id": spool, "printer_id": 1, "ams_id": 0, "tray_id": 0}, f.admin, "", 200), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	first := post()
	if first["configured"] != false || first["pending_config"] != true || len(f.hw.messages) != 0 {
		t.Fatal(first, f.hw.messages)
	}
	for _, stmt := range []string{"INSERT INTO spool_filament_preset(spool_id,printer_model,nozzle_diameter,slicer_filament,slicer_filament_name) VALUES(1,'X1C','0.4','GFSB00','Acme Red @BBL X1C')", "INSERT INTO spool_k_profile(spool_id,printer_id,extruder,nozzle_diameter,k_value,cali_idx) VALUES(1,1,0,'0.4',0.05,17)"} {
		if _, e := f.s.db.Exec(stmt); e != nil {
			t.Fatal(e)
		}
	}
	raw["tray_exist_bits"] = "1" // On AMS HT firmware the presence bit can return with state still 9.
	f.s.replayAssignments(t.Context(), 1)
	if len(f.hw.messages) != 3 {
		t.Fatal(f.hw.messages)
	}
	cfg := f.hw.messages[0]["print"].(row)
	cal := f.hw.messages[1]["print"].(row)
	if cfg["tray_sub_brands"] != "Acme Red" || cfg["tray_info_idx"] != "GFB00" || cfg["tray_color"] != "CC99DDFF" || num(cfg["nozzle_temp_min"]) != 190 || num(cal["cali_idx"]) != 17 || cal["filament_id"] != "GFB00" {
		t.Fatal(cfg, cal)
	}
	slot["tray_type"] = "PLA"
	slot["tray_color"] = "CC99DDFF"
	slot["tray_info_idx"] = "GFB00"
	f.s.replayAssignments(t.Context(), 1)
	if len(f.hw.messages) != 3 {
		t.Fatal("duplicate configuration")
	}
	var fingerprint string
	if e = f.s.db.QueryRow("SELECT fingerprint_type FROM spool_assignment WHERE id=?", first["id"]).Scan(&fingerprint); e != nil || fingerprint != "PLA" {
		t.Fatal(fingerprint, e)
	}
	slot["state"] = 3
	slot["tray_type"] = ""
	delete(raw, "tray_exist_bits") // The user's placement may be the only evidence on older firmware.
	d.h = nil
	offline := post()
	if offline["configured"] != false || offline["pending_config"] != true {
		t.Fatal(offline)
	}
	d.h = f.hw
	live := post()
	if live["configured"] != true || live["pending_config"] != false || len(f.hw.messages) != 6 {
		t.Fatal(live, f.hw.messages)
	}
	// An older generation, a changed physical identity or turned-on login cannot actuate.
	if f.s.ConfigureNativeAssignment(t.Context(), num(live["id"]), "stale") {
		t.Fatal("stale generation")
	}
	if _, e = f.s.db.Exec("UPDATE spool SET tag_uid='AA22' WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	slot["tag_uid"] = "BB33"
	if v := post(); v["configured"] != false {
		t.Fatal(v)
	}
	slot["tag_uid"] = "AA22"
	slot["state"] = 11
	tx, e := f.s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	if e = access.Put(tx, "auth_enabled", "true"); e != nil {
		t.Fatal(e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	f.s.replayAssignments(t.Context(), 1)
	if len(f.hw.messages) != 6 {
		t.Fatal("configuration after authority expired")
	}
}

type failCalibration struct{ *mockHardware }

func (h failCalibration) Publish(ctx context.Context, m row) error {
	if v, ok := m["print"].(row); ok && v["command"] == "extrusion_cali_sel" {
		return errors.New("fixture unavailable")
	}
	return h.mockHardware.Publish(ctx, m)
}
func TestPartialAssignmentDeliveryRetainsCurrentOwnerForRetry(t *testing.T) {
	f := farmFixture(t)
	for _, q := range []string{"INSERT INTO users(id,username,role,auth_source,is_active) VALUES(8,'staff','user','local',1)", "INSERT INTO spool(id,material,label_weight,core_weight,weight_used,weight_locked) VALUES(1,'ABS',1000,200,0,0)"} {
		if _, e := f.s.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	actor := access.Principal{ID: 8, Permissions: map[string]bool{"inventory:update": true}, PrinterIDs: map[int64]bool{1: true}}
	current := actor
	f.s.SetUserResolver(func(context.Context, int64) (access.Principal, error) { return current, nil })
	m, e := management.New(f.s.db)
	if e != nil {
		t.Fatal(e)
	}
	m.ConfigureNativeAssignment = f.s.ConfigureNativeAssignment
	m.Register(f.mux)
	d := f.s.devices[1]
	tray := map[string]any{"id": "0", "state": 11, "tray_type": "ABS"}
	d.kReport = row{"gcode_state": "IDLE", "ams": map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{tray}}}}}
	d.h = failCalibration{f.hw}
	var res row
	json.Unmarshal(f.req("POST", "/inventory/assignments", row{"printer_id": 1, "ams_id": 0, "tray_id": 0, "spool_id": 1}, actor, "", 200), &res)
	if res["pending_config"] != true || res["configured"] != false || len(f.hw.messages) != 1 {
		t.Fatal(res, f.hw.messages)
	}
	if _, e := f.s.db.Exec("UPDATE bb_slot_config SET due=0"); e != nil {
		t.Fatal(e)
	}
	d.h = f.hw
	current.PrinterIDs = map[int64]bool{}
	f.s.replayAssignments(t.Context(), 1)
	if len(f.hw.messages) != 1 {
		t.Fatal("revoked authorization")
	}
	current = actor
	f.s.replayAssignments(t.Context(), 1)
	if len(f.hw.messages) != 4 || num(f.hw.messages[2]["print"].(row)["cali_idx"]) != -1 {
		t.Fatal(f.hw.messages)
	}
	s, e := resolveSlot(row{"model": "H3C"}, row{"vir_slot": []any{map[string]any{"id": 253, "tray_type": "ASA"}}}, 255, 8)
	if e != nil || s.wire != 253 || s.global != 253 || s.local != 0 || s.extruder != 0 || s.current["tray_type"] != "ASA" {
		t.Fatal(s, e)
	}
}
