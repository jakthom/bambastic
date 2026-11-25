package production

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/textproto"
	"os"
	"strings"
	"testing"

	"bambastic/internal/access"
)

func TestFleetControlsAndSensorHistory(t *testing.T) {
	f := farmFixture(t)
	k := num(f.pr["id"])
	f.req("POST", "/printers/1/temperature/bed?target=60", nil, f.admin, "", 200)
	if got := f.hw.messages[0]["print"].(row)["param"]; got != "M140 S60" {
		t.Fatal(got)
	}
	f.req("POST", "/printers/1/temperature/nozzle?target=400", nil, f.admin, "", 422)
	f.req("POST", "/printers/1/temperature/chamber?target=30", nil, f.admin, "", 422)
	f.req("POST", "/printers/1/print-speed?mode=4", nil, f.admin, "", 200)
	f.req("POST", "/printers/1/chamber-light?on=true", nil, f.admin, "", 200)
	if len(f.hw.messages) != 4 {
		t.Fatal(f.hw.messages)
	}
	f.req("POST", "/printers/1/ams/load?tray_id=129", nil, f.admin, "", 200)
	load := f.hw.messages[4]["print"].(row)
	if load["ams_id"] != 129 || load["slot_id"] != 0 || load["target"] != 129 {
		t.Fatal(load)
	}
	f.req("POST", "/printers/1/ams/load?tray_id=19", nil, f.admin, "", 422)
	f.req("POST", "/printers/1/temperature/bed?target=20", nil, access.Principal{Admin: true, PrinterIDs: map[int64]bool{2: true}}, "", 404)
	r := row{"gcode_state": "IDLE", "nozzle_temper": 112.5, "nozzle_target_temper": 200, "bed_temper": 55, "ams": map[string]any{"ams": []any{map[string]any{"id": "0", "temp": "23.5", "humidity": "3", "humidity_raw": "13"}}}}
	f.s.devices[k].report = r
	f.s.sample(context.Background(), f.pr, r)
	if !strings.Contains(string(f.req("GET", "/printer-sensor-history/1?kinds=nozzle", nil, f.admin, "", 200)), "112.5") {
		t.Fatal("nozzle sample missing")
	}
	if !strings.Contains(string(f.req("GET", "/ams-history/1/0", nil, f.admin, "", 200)), "23.5") {
		t.Fatal("AMS sample missing")
	}
	if e := f.s.db.QueryRow("SELECT count(*) FROM printer_sensor_history WHERE sensor_kind='nozzle_2'").Scan(new(int)); e != nil {
		t.Fatal(e)
	}
}
func TestQueueWatchdogAndCooldown(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	aid := f.upload(f.admin)
	f.req("POST", "/queue/", row{"archive_id": aid, "target_model": "X1C"}, f.admin, "", 200)
	job, e := f.s.claim(ctx, f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
	if !f.s.coolDown(ctx, num(job["id"]), 1, &textproto.Error{Code: 530, Msg: "offline"}) {
		t.Fatal("cooldown missing")
	}
	if job, e = f.s.claim(ctx, f.pr); e != nil || job != nil {
		t.Fatal("device cooldown bypassed")
	}
	if _, e = f.s.db.Exec("UPDATE bb_device_cooldown SET until=0"); e != nil {
		t.Fatal(e)
	}
	job, e = f.s.claim(ctx, f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
	f.s.send(ctx, job)
	if e = f.s.observe(ctx, f.pr, row{"gcode_state": "IDLE"}); e != nil {
		t.Fatal(e)
	}
	if _, e = f.s.db.Exec("UPDATE bb_idle_watch SET first=unixepoch()-95,last=unixepoch()-2"); e != nil {
		t.Fatal(e)
	}
	if e = f.s.observe(ctx, f.pr, row{"gcode_state": "IDLE"}); e != nil {
		t.Fatal(e)
	}
	job, e = f.s.claim(ctx, f.pr)
	if e != nil || job == nil {
		t.Fatal(job, e)
	}
	f.s.send(ctx, job)
	f.s.observeRefusal(ctx, 1, row{"command": "project_file", "sequence_id": "other", "result": "fail"})
	if got := string(f.req("GET", "/queue/1", nil, f.admin, "", 200)); !strings.Contains(got, `"dispatching"`) {
		t.Fatal(got)
	}
	f.s.observeRefusal(ctx, 1, row{"command": "project_file", "sequence_id": startSequence(val(job["remote"])), "result": "fail", "reason": "file refused"})
	if got := string(f.req("GET", "/queue/1", nil, f.admin, "", 200)); !strings.Contains(got, "file refused") {
		t.Fatal(got)
	}
}

type captureDevice struct{ *mockHardware }

func (c captureDevice) Retrieve(ctx context.Context, name, path string) (string, error) {
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("Metadata/plate_1.gcode")
	io.WriteString(w, "; external simulator")
	z.Close()
	return "bracket.gcode.3mf", os.WriteFile(path, buf.Bytes(), 0600)
}
func TestExternalCaptureReusesDesignAndPreservesRuns(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	f.s.devices[1].h = captureDevice{f.hw}
	var aid int64
	for i := 0; i < 2; i++ {
		report := row{"subtask_id": fmt.Sprint(i + 1), "gcode_file": "bracket.gcode.3mf", "subtask_name": "bracket", "gcode_state": "RUNNING"}
		if e := f.s.observe(ctx, f.pr, report); e != nil {
			t.Fatal(e)
		}
		j, e := one(ctx, f.s.db, "SELECT * FROM bb_captures")
		if e != nil {
			t.Fatal(e)
		}
		f.s.capture(ctx, j)
		if i == 0 {
			aid = num(j["archive_id"])
			if _, e = f.s.db.Exec("UPDATE print_archives SET print_name='My bracket' WHERE id=?", aid); e != nil {
				t.Fatal(e)
			}
		}
		report["gcode_state"] = "FINISH"
		if e = f.s.observe(ctx, f.pr, report); e != nil {
			t.Fatal(e)
		}
	}
	v, e := one(ctx, f.s.db, "SELECT count(*) n FROM print_log_entries WHERE archive_id=? AND status='completed'", aid)
	if e != nil || num(v["n"]) != 2 {
		t.Fatal(v, e)
	}
	if got := string(f.req("GET", "/archives/2", nil, f.admin, "", 200)); !strings.Contains(got, "My bracket") {
		t.Fatal(got)
	}
	var pending int
	if e = f.s.db.QueryRow("SELECT count(*) FROM bb_captures").Scan(&pending); e != nil || pending != 0 {
		t.Fatal(pending, e)
	}
}
func TestForcedMaterialMatches(t *testing.T) {
	rp := row{"ams": map[string]any{"ams": []any{map[string]any{"id": "1", "tray": []any{map[string]any{"id": "0", "tray_type": "PLA", "tray_info_idx": "Basic", "tray_color": "FF0000FF"}, map[string]any{"id": "1", "tray_type": "PLA", "tray_info_idx": "Matte", "tray_color": "FF0000FF"}}}}}}
	pl := plate{Filaments: []row{{"id": int64(2), "type": "PLA", "color": "#ff0000", "tray_info_idx": "Matte"}}}
	_, exact := plateRequirements(pl, true)
	q := row{"filament_overrides": jstr(exact)}
	ids, reason := matchFilaments(q, rp, pl)
	if reason != "" || len(ids) != 2 || ids[0] != -1 || ids[1] != 5 {
		t.Fatal(ids, reason)
	}
	exact[0]["color"] = "#0000ff"
	q["filament_overrides"] = jstr(exact)
	if _, reason = matchFilaments(q, rp, pl); reason == "" {
		t.Fatal("forced color mismatch accepted")
	}
}
