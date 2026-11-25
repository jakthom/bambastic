package production

import (
	"context"
	"database/sql"
	"testing"
	"time"
)

func driftFixture(t *testing.T) (farmTest, *liveDevice, map[string]any) {
	t.Helper()
	f := farmFixture(t)
	for _, stmt := range []string{
		"INSERT INTO spool(id,material,label_weight,core_weight,weight_used,weight_locked,rgba,tag_uid,slicer_filament) VALUES(1,'PLA',1000,250,0,0,'112233FF','AC001122334455','GFA00')",
		"INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(1,1,0,1)",
		"INSERT INTO spool_k_profile(spool_id,printer_id,extruder,nozzle_diameter,k_value,name,cali_idx) VALUES(1,1,0,'0.4',0.03,'Measured',19)",
		"INSERT INTO spool_filament_preset(spool_id,printer_model,nozzle_diameter,slicer_filament) VALUES(1,'X1C','0.4','GFSB00')",
	} {
		if _, e := f.s.db.Exec(stmt); e != nil {
			t.Fatal(e)
		}
	}
	d := f.s.devices[1]
	tray := map[string]any{"id": "1", "tag_uid": "AC00112233445500", "tray_type": "PLA", "tray_color": "112233FF", "tray_info_idx": "GFA00", "cali_idx": -1}
	r := row{"gcode_state": "IDLE", "nozzle_diameter": "0.4", "ams": map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{tray}}}}}
	d.kReport = r
	d.report = r
	return f, d, tray
}
func TestResetKProfileRestoresOnceAndHonorsManualDefault(t *testing.T) {
	f, d, tray := driftFixture(t)
	now := time.Now()
	apply := func(delay time.Duration, job int64, used map[int64]bool, want int) {
		t.Helper()
		if n := f.s.reapplyK(t.Context(), 1, d, used, job, now.Add(delay)); n != want {
			t.Fatalf("reapply at %v job %d: got %d want %d", delay, job, n, want)
		}
	}
	apply(0, 0, nil, 1)
	p := f.hw.messages[0]["print"].(row)
	if p["command"] != "extrusion_cali_sel" || num(p["cali_idx"]) != 19 || p["filament_id"] != "GFB00" || num(p["tray_id"]) != 1 || p["nozzle_diameter"] != "0.4" || p["setting_id"] != nil {
		t.Fatal(p)
	}
	apply(time.Second, 0, nil, 0)
	apply(time.Second, 55, nil, 0)
	apply(30*time.Second, 0, nil, 1)
	apply(60*time.Second, 0, nil, 1)
	apply(5*time.Minute, 0, nil, 0)
	apply(5*time.Minute, 56, map[int64]bool{2: true}, 0)
	apply(5*time.Minute, 56, map[int64]bool{1: true}, 1)
	apply(6*time.Minute, 56, nil, 0)
	tray["cali_idx"] = 11
	apply(6*time.Minute, 0, nil, 0)
	f.req("POST", "/printers/1/slots/0/1/configure?tray_type=PLA&tray_color=112233FF&nozzle_temp_min=190&nozzle_temp_max=220&cali_idx=-1", nil, f.admin, "", 200)
	// The first push after a manual Default can still contain the old index.
	apply(7*time.Minute, 0, nil, 0)
	tray["cali_idx"] = -1
	apply(7*time.Minute, 57, nil, 0)
	delete(tray, "tray_type")
	apply(8*time.Minute, 0, nil, 0)
	tray["tray_type"] = "PLA"
	apply(9*time.Minute, 0, nil, 0)
	tray["tray_type"] = ""
	apply(10*time.Minute, 0, nil, 0)
	tray["tray_type"] = "PLA"
	tray["tag_uid"] = "ACDEFFFFFFFFFF00"
	apply(11*time.Minute, 58, nil, 0)
	tray["tag_uid"] = "AC00112233445500"
	tray["cali_idx"] = "invalid"
	apply(12*time.Minute, 0, nil, 0)
	delete(tray, "cali_idx")
	d.report["gcode_state"] = "PREPARE"
	apply(13*time.Minute, 0, nil, 0)
	d.report["gcode_state"] = "RUNNING"
	apply(14*time.Minute, 60, nil, 0)
	d.report["gcode_state"] = "IDLE"
	apply(15*time.Minute, 0, nil, 1)
}
func TestPendingPrintRestoresUsedCalibrationBeforeStart(t *testing.T) {
	f, d, _ := driftFixture(t)
	aid := f.upload(f.admin)
	f.req("POST", "/queue/", row{"archive_id": aid, "printer_id": 1, "plate_id": 2}, f.admin, "K-before-job", 200)
	job, e := f.s.claim(t.Context(), f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
	job["ams_mapping"] = "[-1,1]"
	// No-profile negative cache from a previous scan must not delay this job.
	d.drift.watches = map[trayKey]kWatch{}
	key := trayKey{0, 1}
	d.drift.watches[key] = kWatch{signature: kCandidates(d.printer, d.report)[key].signature, next: time.Now().Add(6 * time.Minute)}
	f.s.send(t.Context(), job)
	if len(f.hw.messages) != 2 || f.hw.messages[0]["print"].(row)["command"] != "extrusion_cali_sel" || f.hw.messages[1]["print"].(row)["command"] != "project_file" {
		t.Fatal(f.hw.messages)
	}
}
func TestRemoteSpoolAndNozzleKDriftGuards(t *testing.T) {
	f, d, tray := driftFixture(t)
	for _, stmt := range []string{
		"INSERT INTO settings(key,value) VALUES('spoolman_enabled','true') ON CONFLICT(key) DO UPDATE SET value='true'",
		"INSERT INTO spoolman_slot_assignments(printer_id,ams_id,tray_id,spoolman_spool_id) VALUES(1,0,1,78)",
		"INSERT INTO spoolman_k_profile(printer_id,spoolman_spool_id,extruder,nozzle_diameter,nozzle_type,k_value,cali_idx) VALUES(1,78,1,'0.6','HH00',0.06,50)",
	} {
		if _, e := f.s.db.Exec(stmt); e != nil {
			t.Fatal(e)
		}
	}
	d.printer["nozzle_count"] = 2
	d.report["device"] = map[string]any{"fila_switch": map[string]any{}, "nozzle": map[string]any{"info": []any{map[string]any{"id": 0, "diameter": "0.4", "type": "HS01"}, map[string]any{"id": 1, "diameter": "0.6", "type": "HH01"}}}}
	unit := d.report["ams"].(map[string]any)["ams"].([]any)[0].(map[string]any)
	now := time.Now()
	if f.s.reapplyK(t.Context(), 1, d, nil, 1, now) != 0 {
		t.Fatal("assumed a hotend without routing")
	}
	unit["info"] = "01000e01" // switch inlet A/left extruder
	var changed bool
	f.s.SpoolmanMaterialIdentity = func(_ context.Context, id int64) (map[string]any, error) {
		if id != 78 {
			t.Fatal(id)
		}
		if changed {
			_, e := f.s.db.Exec("UPDATE spoolman_slot_assignments SET spoolman_spool_id=79 WHERE printer_id=1")
			if e != nil {
				t.Fatal(e)
			}
		}
		return map[string]any{"tag_uid": "AC001122334455"}, nil
	}
	if n := f.s.reapplyK(t.Context(), 1, d, nil, 2, now); n != 1 {
		t.Fatal("matched routed pair", n)
	}
	msg := f.hw.messages[0]["print"].(row)
	if num(msg["cali_idx"]) != 50 || msg["nozzle_diameter"] != "0.6" {
		t.Fatal(msg)
	}
	unit["info"] = "00000e01"
	if f.s.reapplyK(t.Context(), 1, d, nil, 3, now.Add(time.Minute)) != 0 {
		t.Fatal("borrowed opposite nozzle's profile")
	}
	unit["info"] = "01000e01"
	changed = true
	if f.s.reapplyK(t.Context(), 1, d, nil, 4, now.Add(2*time.Minute)) != 0 {
		t.Fatal("sent after assignment changed")
	}
	tray["tag_uid"] = "0000000000000000"
	if f.s.reapplyK(t.Context(), 1, d, nil, 5, now.Add(3*time.Minute)) != 0 {
		t.Fatal("inactive native inventory shadowed Spoolman")
	}
}

type kTelemetryFixture struct {
	mockHardware
	writes chan row
}

func (h *kTelemetryFixture) Publish(ctx context.Context, cmd row) error {
	select {
	case h.writes <- cmd:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestKDriftOnUnchangedRFIDAndPartialMQTTStatus(t *testing.T) {
	f, _, _ := driftFixture(t)
	// Keep a dashboard read snapshot open throughout status reconciliation.
	// A non-WAL SQLite fixture made the sampler's concurrent COMMIT return
	// SQLITE_BUSY and left the pooled connection inside a transaction.
	reader, err := f.s.db.BeginTx(t.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Rollback()
	var printers int
	if err = reader.QueryRow("SELECT count(*) FROM printers").Scan(&printers); err != nil {
		t.Fatal(err)
	}
	hardwareSim := &kTelemetryFixture{writes: make(chan row, 4)}
	arrived := make(chan func(row), 1)
	f.s.open = func(_ context.Context, pr row, pin string, receive func(row)) (hardware, error) {
		arrived <- receive
		return hardwareSim, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); f.s.watch(ctx, f.pr) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("printer observer did not shut down")
		}
	}()
	var report func(row)
	select {
	case report = <-arrived:
	case <-time.After(3 * time.Second):
		t.Fatal("no simulated MQTT connection")
	}
	a := func(tray map[string]any) map[string]any {
		return map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{tray}}}}
	}
	report(row{"gcode_state": "IDLE", "nozzle_diameter": "0.4", "ams": a(map[string]any{"id": "1", "tag_uid": "AC00112233445500", "tray_type": "PLA", "tray_color": "112233FF", "tray_info_idx": "GFA00", "cali_idx": 19})})
	// Firmware sends the lost index separately without repeating RFID, colour,
	// nozzle, or gcode_state. The watch must act on the current connection.
	report(row{"ams": a(map[string]any{"id": "1", "cali_idx": -1})})
	select {
	case packet := <-hardwareSim.writes:
		cmd, _ := packet["print"].(row)
		if cmd["command"] != "extrusion_cali_sel" || num(cmd["cali_idx"]) != 19 || num(cmd["tray_id"]) != 1 {
			t.Fatal(packet)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no calibration restore after partial telemetry")
	}
}
