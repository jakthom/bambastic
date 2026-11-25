package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"crypto/md5"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlateSnippetsPreserveOtherPlatesAndVerifyDigest(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "source.3mf")
	f, e := os.Create(file)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	gcode := "; HEADER_BLOCK_START\n; max z height [mm]: 12\n; HEADER_BLOCK_END\n; startup\n; MACHINE_START_GCODE_END\n; job\n; EXECUTABLE_BLOCK_END\n"
	for name, raw := range map[string]string{"Metadata/plate_2.gcode": gcode, "Metadata/plate_2.gcode.md5": "OLD", "Metadata/plate_7.gcode": "other plate"} {
		w, e := z.Create(name)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(w, raw); e != nil {
			t.Fatal(e)
		}
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	f.Close()
	out, e := rewritePlate(t.Context(), dir, file, 2, snippets{"; start {max_layer_z}", "; finish"})
	if e != nil {
		t.Fatal(e)
	}
	archive, e := zip.OpenReader(out)
	if e != nil {
		t.Fatal(e)
	}
	defer archive.Close()
	expected := "; HEADER_BLOCK_START\n; max z height [mm]: 12\n; HEADER_BLOCK_END\n; startup\n; start 12\n; MACHINE_START_GCODE_END\n; job\n; finish\n; EXECUTABLE_BLOCK_END\n"
	for name, want := range map[string]string{"Metadata/plate_2.gcode": expected, "Metadata/plate_7.gcode": "other plate", "Metadata/plate_2.gcode.md5": fmt.Sprintf("%X", md5.Sum([]byte(expected)))} {
		b, e := readMember(archive, name, 1024)
		if e != nil || string(b) != want {
			t.Fatal(name, string(b), e)
		}
	}
	original, e := zip.OpenReader(file)
	if e != nil {
		t.Fatal(e)
	}
	defer original.Close()
	b, e := readMember(original, "Metadata/plate_2.gcode", 1024)
	if e != nil || string(b) != gcode {
		t.Fatal("source changed", e)
	}
}
func TestSensorRecheckedAfterUpload(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	id := f.upload(f.admin)
	f.req("POST", "/queue/", row{"archive_id": id, "printer_id": 1, "plate_id": 2}, f.admin, "", 200)
	job, e := f.s.claim(ctx, f.pr)
	if job == nil || e != nil {
		t.Fatal(job, e)
	}
	f.hw.during = func() {
		if _, e = f.s.db.Exec("INSERT INTO printer_ha_sensors(printer_id,name,entity_id,kind,block_print,last_checked,last_state) VALUES(1,'enclosure','binary_sensor.farm','binary',1,CURRENT_TIMESTAMP,'on')"); e != nil {
			t.Fatal(e)
		}
	}
	f.s.send(ctx, job)
	if len(f.hw.messages) != 0 {
		t.Fatal("device start sent while sensor blocking")
	}
	v, e := one(ctx, f.s.db, "SELECT status,waiting_reason FROM print_queue WHERE id=?", job["id"])
	if e != nil || v["status"] != "pending" || v["waiting_reason"] != "Waiting for Home Assistant sensor clearance" {
		t.Fatal(v, e)
	}
}

func TestOwnedPreheatReleaseAndKeepWarmExpiry(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	aid := f.upload(f.admin)
	if _, e := f.s.db.Exec("UPDATE print_archives SET bed_temperature=60 WHERE id=?", aid); e != nil {
		t.Fatal(e)
	}
	tx, e := f.s.db.Begin()
	if e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"preheat_soak_seconds": "0", "preheat_enabled": "true", "queue_keep_bed_warm": "true", "require_plate_clear": "true"} {
		if e = access.Put(tx, k, v); e != nil {
			t.Fatal(e)
		}
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
	f.req("POST", "/queue/", row{"archive_id": aid, "plate_id": 2, "printer_id": 1, "preheat_override": "on", "preheat_chamber_target_override": 66}, f.admin, "", 422)
	f.req("POST", "/queue/", row{"archive_id": aid, "plate_id": 2, "printer_id": 1, "preheat_override": "on", "preheat_chamber_target_override": 0}, f.admin, "", 200)
	r := f.s.devices[1].report
	r["bed_temper"] = 60
	r["bed_target_temper"] = 60
	j, e := f.s.claim(ctx, f.pr)
	if e != nil || j == nil {
		t.Fatal(j, e)
	}
	f.hw.during = func() { f.req("POST", "/queue/1/cancel", nil, f.admin, "", 200) }
	f.s.send(ctx, j)
	f.s.releaseHeaters(ctx)
	messages := jstr(f.hw.messages)
	if !strings.Contains(messages, "M140 S60") || !strings.Contains(messages, "M140 S0") || strings.Contains(messages, "project_file") {
		t.Fatal(messages)
	}
	f.req("POST", "/queue/", row{"archive_id": aid, "plate_id": 2, "printer_id": 1, "preheat_override": "on", "preheat_chamber_target_override": 45}, f.admin, "", 200)
	r["gcode_state"] = "FINISH"
	r["bed_target_temper"] = 0
	if _, e = f.s.db.Exec("UPDATE printers SET awaiting_plate_clear=1 WHERE id=1"); e != nil {
		t.Fatal(e)
	}
	f.s.keepWarm(ctx)
	if msg := jstr(f.hw.messages[len(f.hw.messages)-1]); !strings.Contains(msg, "M140 S90") {
		t.Fatal(msg)
	}
	r["bed_target_temper"] = 90
	if _, e = f.s.db.Exec("UPDATE bb_keep_warm SET expires=0"); e != nil {
		t.Fatal(e)
	}
	f.s.keepWarm(ctx)
	n := len(f.hw.messages)
	if !strings.Contains(jstr(f.hw.messages[n-1]), "M140 S0") {
		t.Fatal(f.hw.messages)
	}
	f.s.keepWarm(ctx)
	if len(f.hw.messages) != n {
		t.Fatal("Expired hold rearmed")
	}
}
