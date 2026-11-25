package slicing

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/management"
	"bambastic/internal/production"
)

func TestPipelineSliceOnceFanOutCancelAndRetry(t *testing.T) {
	s, h := testSlicer(t)
	s.db.SetMaxOpenConns(1)
	if _, e := management.New(s.db); e != nil {
		t.Fatal(e)
	}
	farm, e := production.New(s.db, s.dir)
	if e != nil {
		t.Fatal(e)
	}
	farm.Register(h)
	s.AttachFarm(farm)
	admin := access.Principal{Admin: true}
	// Seed a registered, offline printer. Production registration verifies the LAN device.
	_, e = s.db.Exec("INSERT INTO printers(name,serial_number,ip_address,access_code,model,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES('X1','SIM','127.0.0.1','fixture','X1C',1,1,0,0,0,0,0,0,0)")
	if e != nil {
		t.Fatal(e)
	}
	var code int
	var v object
	if e = os.WriteFile(filepath.Join(s.dir, "model.stl"), []byte("source"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("INSERT INTO library_files(filename,file_path,file_type,file_size,print_count,is_external) VALUES('part.stl','model.stl','stl',6,0,0)"); e != nil {
		t.Fatal(e)
	}
	var output bytes.Buffer
	z := zip.NewWriter(&output)
	g, _ := z.Create("Metadata/plate_1.gcode")
	g.Write([]byte("G1 X55\n"))
	z.Close()
	var calls atomic.Int32
	var block atomic.Bool
	release := make(chan struct{})
	entered := make(chan struct{})
	side := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/slice" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if e := r.ParseMultipartForm(1 << 20); e != nil {
			t.Error(e)
			return
		}
		defer r.MultipartForm.RemoveAll()
		if block.Load() {
			close(entered)
			<-release
		}
		w.Write(output.Bytes())
	}))
	defer side.Close()
	if _, e = s.db.Exec("INSERT INTO settings(key,value) VALUES('bambu_studio_api_url',?)", side.URL); e != nil {
		t.Fatal(e)
	}
	const base = "/api/v1/"
	code, v = call(t, h, admin, "POST", base+"slicer-pipelines/", `{"name":"Fixture","printer_preset":{"source":"standard","id":"Bambu Lab X1 Carbon 0.4 nozzle"},"process_preset":{"source":"standard","id":"Standard"},"filament_presets":[{"source":"standard","id":"PLA"}],"target_kind":"specific_printer","target_printer_id":1}`)
	if code != 201 {
		t.Fatal(code, v)
	}
	code, v = call(t, h, admin, "POST", base+"slicer-pipelines/1/run", `{"copies":4,"source_library_file_id":1}`)
	if code != 409 {
		t.Fatal(code, v)
	}
	code, v = call(t, h, admin, "POST", base+"slicer-pipelines/1/run", `{"copies":4,"force":true,"source_library_file_id":1}`)
	if code != 202 {
		t.Fatal(code, v)
	}
	poll := func(path, want string) object {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		var o object
		for time.Now().Before(deadline) {
			_, o = call(t, h, admin, "GET", base+path, "")
			if o["status"] == want {
				return o
			}
			if o["status"] == "failed" {
				break
			}
			time.Sleep(8 * time.Millisecond)
		}
		t.Fatalf("expected %s: %#v", want, o)
		return nil
	}
	poll("pipeline-runs/1", "in_progress")
	var copies, files int
	if e = s.db.QueryRow("SELECT COUNT(*),COUNT(DISTINCT library_file_id) FROM print_queue").Scan(&copies, &files); e != nil || copies != 4 || files != 1 || calls.Load() != 1 {
		t.Fatal(copies, files, calls.Load(), e)
	}
	if _, e = s.db.Exec("UPDATE print_queue SET status=CASE WHEN id<3 THEN 'failed' ELSE 'completed' END"); e != nil {
		t.Fatal(e)
	}
	v = poll("pipeline-runs/1", "partial_failure")
	if number(v["copies_failed"]) != 2 {
		t.Fatal(v)
	}
	code, v = call(t, h, admin, "POST", base+"pipeline-runs/1/retry-failed", `{"force":true}`)
	if code != 202 || number(v["parent_run_id"]) != 1 || number(v["copies"]) != 2 {
		t.Fatal(code, v)
	}
	poll("pipeline-runs/2", "in_progress")
	block.Store(true)
	code, v = call(t, h, admin, "POST", base+"slicer-pipelines/1/run", `{"source_library_file_id":1,"force":true}`)
	if code != 202 {
		t.Fatal(code, v)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("slice request did not arrive")
	}
	code, v = call(t, h, admin, "POST", base+"pipeline-runs/3/cancel", "")
	close(release)
	if code != 200 || v["status"] != "cancelled" {
		t.Fatal(code, v)
	}
	// Wait for publication and the cancellation check before the service is torn down.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		var count int
		_ = s.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM library_files WHERE source_type='sliced'").Scan(&count)
		if count == 3 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = s.db.QueryRow("SELECT COUNT(*) FROM print_queue").Scan(&copies); e != nil || copies != 6 {
		t.Fatal("cancelled run enqueued work", copies, e)
	}
	code, v = call(t, h, admin, "GET", base+"pipeline-runs/3", "")
	raw, _ := json.Marshal(v)
	if code != 200 || v["status"] != "cancelled" {
		t.Fatal(code, string(raw))
	}
}
