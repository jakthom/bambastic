package production

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"testing"

	"bambastic/internal/management"
)

// The physical public assignment route is used mid-print, so a reconnect or
// restart before the next printer status cannot lose the layer of the replacement.
func TestRunBackupAndSameHolderSplit(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	m, e := management.New(f.s.db)
	if e != nil {
		t.Fatal(e)
	}
	m.Register(f.mux)
	for i := 1; i <= 4; i++ {
		f.req("POST", "/inventory/spools", row{"material": "PLA", "label_weight": 1000}, f.admin, "", 200)
	}
	assign := func(tray, id int) {
		t.Helper()
		f.req("POST", "/inventory/assignments", row{"printer_id": 1, "ams_id": 0, "tray_id": tray, "spool_id": id}, f.admin, "", 200)
	}
	assign(0, 1)
	assign(1, 2)
	assign(2, 4)
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	w, _ := z.Create("Metadata/plate_1.gcode")
	io.WriteString(w, "M83\nM620 S0A\nM73 L1\nG1 E10\nM73 L2\nG1 E23\nG1 E-3\nM73 L3\nG1 E30\nM73 L4\nG1 E40\n")
	w, _ = z.Create("Metadata/slice_info.config")
	io.WriteString(w, `<config><plate><metadata key="plater_id" value="1"/><filament id="1" type="PLA" used_g="100"/></plate></config>`)
	z.Close()
	aid, e := f.s.storeAsset(ctx, f.s.db, f.admin, bytes.NewReader(b.Bytes()), "archives", "roller.3mf", 0, 1, 0)
	if e != nil {
		t.Fatal(e)
	}
	f.req("POST", "/queue/", row{"printer_id": 1, "archive_id": aid, "plate_id": 1, "ams_mapping": []int{0}}, f.admin, "", 200)
	f.s.devices[1].report["ams"] = map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{map[string]any{"id": "0", "tray_type": "PLA", "tray_color": "333333FF", "remain": 85}}}}}
	q, e := f.s.claim(ctx, f.pr)
	if e != nil || q == nil {
		why, _ := one(ctx, f.s.db, "SELECT status,manual_start,waiting_reason FROM print_queue")
		t.Fatal(q, e, why)
	}
	f.s.send(ctx, q)
	report := func(state string, layer, feeding, remain int) row {
		trays := []any{}
		for i := range 3 {
			trays = append(trays, map[string]any{"id": i, "remain": remain, "tray_type": "PLA"})
		}
		return row{"subtask_id": "29", "gcode_file": f.hw.uploaded, "gcode_state": state, "layer_num": layer, "total_layer_num": 4, "ams": map[string]any{"tray_now": feeding, "ams": []any{map[string]any{"id": 0, "tray": trays}}}}
	}
	observe := func(v row) {
		t.Helper()
		if e := f.s.observe(ctx, f.pr, v); e != nil {
			t.Fatal(e)
		}
	}
	observe(report("RUNNING", 0, 0, 85))
	observe(report("RUNNING", 1, 1, 80))
	observe(report("PAUSE", 2, 1, 79))
	assign(1, 3)
	observe(report("RUNNING", 3, 1, 70))
	observe(report("FINISH", 0, 255, 60))
	observe(report("FINISH", 0, 255, 60))
	for id, want := range map[int]float64{1: 10, 2: 20, 3: 70, 4: 0} {
		var actual float64
		if e := f.s.db.QueryRow("SELECT weight_used FROM spool WHERE id=?", id).Scan(&actual); e != nil || actual != want {
			t.Errorf("spool %d: got %v want %v (%v)", id, actual, want, e)
		}
	}
	var count int
	var grams float64
	if e := f.s.db.QueryRow("SELECT count(*),sum(weight_used) FROM spool_usage_history").Scan(&count, &grams); e != nil || count != 3 || grams != 100 {
		t.Fatal(count, grams, e)
	}
}

// Keep replay of the earlier production report shape: exercise the same HTTP
// create response used by clients even when no 3MF is captured before terminal.
func TestExternalRunMaterialsFromStableTrayOnly(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	for i := 1; i <= 3; i++ {
		if _, e := f.s.db.Exec("INSERT INTO spool(id,material,label_weight,core_weight,weight_used,weight_locked) VALUES(?,'PETG',800,0,0,1)", i); e != nil {
			t.Fatal(e)
		}
		if _, e := f.s.db.Exec("INSERT INTO spool_assignment(printer_id,ams_id,tray_id,spool_id) VALUES(1,0,?,?)", i-1, i); e != nil {
			t.Fatal(e)
		}
	}
	view := func(state string, tray2tag string, pcts ...int) row {
		raw := []any{}
		for i, v := range pcts {
			slot := map[string]any{"id": i, "remain": v, "tray_type": "PETG"}
			if i == 1 {
				slot["tray_uuid"] = tray2tag
			}
			raw = append(raw, slot)
		}
		return row{"gcode_state": state, "subtask_name": "unknown job", "subtask_id": "88", "layer_num": 7, "ams_mapping": []any{0, 1}, "ams": map[string]any{"tray_now": "255", "ams": []any{map[string]any{"id": 0, "tray": raw}}}}
	}
	for _, r := range []row{view("RUNNING", "11112222", 80, 70, 88), view("FAILED", "77777777", 72, 5, 10), view("FAILED", "77777777", 72, 5, 10)} {
		if e := f.s.observe(ctx, f.pr, r); e != nil {
			t.Fatal(e)
		}
	}
	var g float64
	var n int
	if e := f.s.db.QueryRow("SELECT sum(weight_used),count(*) FROM spool_usage_history").Scan(&g, &n); e != nil || g != 64 || n != 1 {
		t.Fatal("no-slice stable mapping", g, n, e)
	}
	if e := f.s.db.QueryRow("SELECT filament_used_grams FROM print_log_entries").Scan(&g); e != nil || g != 64 {
		t.Fatal(g, e)
	}
}
