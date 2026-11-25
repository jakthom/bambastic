package production

import (
	"bambastic/internal/access"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRepriceSpoolmanHistoryAndPerRunCoverage(t *testing.T) {
	f := farmFixture(t)
	f.s.db.SetMaxOpenConns(1)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.s.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("INSERT INTO printers(id,name,ip_address,serial_number,access_code,model,is_active,auto_archive,nozzle_count,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(2,'Hidden','127.0.0.1','another','fixture','X1C',1,1,1,0,0,0,0,0,0)")
	exec("INSERT INTO print_archives(id,filename,file_path,file_size,status,is_favorite,quantity,cost,filament_used_grams,filament_type) VALUES(1,'jig.3mf','jig',0,'completed',0,1,3,100,'PLA'),(2,'private.3mf','hidden',0,'completed',0,1,80,100,'PLA'),(3,'legacy.3mf','legacy',0,'completed',0,1,7,100,'PETG')")
	exec("INSERT INTO print_log_entries(id,archive_id,printer_id,status,started_at,completed_at,cost,filament_used_grams) VALUES(1,1,1,'completed','2025-01-01 00:00:00','2025-01-01 01:00:00',3,100),(2,1,1,'failed','2025-01-02 00:00:00','2025-01-02 01:00:00',3,50),(3,2,2,'completed','2025-01-01 00:00:00','2025-01-01 01:00:00',80,100)")
	exec("INSERT INTO spool_usage_history(archive_id,printer_id,spoolman_spool_id,status,weight_used,percent_used,cost,created_at) VALUES(1,1,10,'completed',50,5,0.50,'2025-01-01 01:00:00'),(1,1,11,'failed',20,2,0.40,'2025-01-02 01:03:00'),(1,1,12,'failed',5,1,0.25,'2025-01-02 01:03:00'),(1,1,13,'failed',5,1,0.25,'2025-01-02 01:04:00'),(2,2,10,'completed',100,10,30,'2025-01-01 01:00:00')")
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The pricing request must not hold a database connection during HTTP I/O.
		ctx, stop := context.WithTimeout(r.Context(), time.Second)
		defer stop()
		var n int
		if e := f.s.db.QueryRowContext(ctx, "SELECT count(*) FROM printers").Scan(&n); e != nil {
			t.Error(e)
		}
		switch r.URL.Path {
		case "/api/v1/health":
			fmt.Fprint(w, "{}")
		case "/api/v1/spool":
			if r.URL.Query().Get("allow_archived") != "true" {
				t.Error("archived pricing omitted")
			}
			fmt.Fprint(w, `[{"id":10,"price":20,"initial_weight":250,"filament":{"price":40,"weight":1000}},{"id":11,"price":0,"initial_weight":250,"filament":{"price":40,"weight":1000}},{"id":12,"price":0,"filament":{}}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer service.Close()
	exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_url',?),('default_filament_cost','25')", service.URL)
	p := access.Principal{Admin: true, PrinterIDs: map[int64]bool{1: true}}
	recalc := func() row {
		t.Helper()
		var v row
		if e := json.Unmarshal(f.req("POST", "/archives/recalculate-costs", nil, p, "", 200), &v); e != nil {
			t.Fatal(e)
		}
		return v
	}
	number := func(q string, want float64) {
		t.Helper()
		var got float64
		if e := f.s.db.QueryRow(q).Scan(&got); e != nil || got != want {
			t.Fatal(q, got, want, e)
		}
	}
	v := recalc()
	if num(v["usage_repriced"]) != 3 || num(v["updated"]) != 1 || num(v["preserved"]) != 1 || num(v["spoolman_unreachable"]) != 0 {
		t.Fatal(v)
	}
	number("SELECT cost FROM spool_usage_history WHERE id=1", 4)
	number("SELECT cost FROM spool_usage_history WHERE id=2", 0.80)
	number("SELECT count(*) FROM spool_usage_history WHERE id=3 AND cost IS NULL", 1)
	number("SELECT cost FROM spool_usage_history WHERE id=4", 0.25)
	number("SELECT cost FROM print_log_entries WHERE id=1", 6.5)
	number("SELECT cost FROM print_log_entries WHERE id=2", 3.27)
	number("SELECT cost FROM print_log_entries WHERE id=3", 80)
	number("SELECT cost FROM print_archives WHERE id=1", 4.62)
	if v = recalc(); num(v["usage_repriced"]) != 0 || num(v["updated"]) != 0 {
		t.Fatal("repricing replay changed values", v)
	}
	service.Close()
	if v = recalc(); num(v["spoolman_unreachable"]) != 4 || num(v["usage_repriced"]) != 0 {
		t.Fatal("expected 4 reachable-scope rows to keep recorded prices", v)
	}
	number("SELECT cost FROM spool_usage_history WHERE id=5", 30)
}
