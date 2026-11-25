package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPartialPerToolLastValidLayerAndNoDuplicateFallback(t *testing.T) {
	f := setup(t)
	f.printer()
	f.exec("INSERT INTO spool(material,label_weight,core_weight,weight_used,weight_locked) VALUES('PLA',1000,0,0,0),('PLA',1000,0,0,0),('PLA',1000,0,0,0)")
	f.exec("INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(1,1,0,0),(2,1,0,1),(3,1,0,2)")
	f.exec("INSERT INTO print_log_entries(id,printer_id,status) VALUES(1,1,'printing')")
	view := func(layer int, pct int, remain int) map[string]any {
		return map[string]any{"layer_num": layer, "total_layer_num": 4, "mc_percent": pct, "ams_mapping": []any{0, 1}, "ams": []any{row{"id": 0, "tray": []any{row{"id": 0, "remain": remain, "tray_type": "PLA"}, row{"id": 1, "remain": remain, "tray_type": "PLA"}, row{"id": 2, "remain": remain, "tray_type": "PLA"}}}}}
	}
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		if e := SnapshotRun(ctx, tx, 1); e != nil {
			return e
		}
		return ObserveMaterial(ctx, tx, 1, 1, view(0, 0, 90), true, false)
	})
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		return ObserveMaterial(ctx, tx, 1, 1, view(2, 40, 88), false, false)
	})
	x, e := parseExtrusion(context.Background(), strings.NewReader("M83\nM620 S0A\nM73 L1\nG1 E74\nG1 E-4\nM73 L2\nM620 S1A\nG1 E5\nM73 L3\nG1 E15\nM73 L4\nM620 S0A\nG1 E30\n"))
	if e != nil {
		t.Fatal(e)
	}
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		if e := ObserveMaterial(ctx, tx, 1, 1, view(0, 0, 30), false, true); e != nil {
			return e
		}
		_, e := AttributeMaterial(ctx, tx, 1, false, 100, map[int]float64{0: 20, 1: 80}, MaterialCloseInputs{RunID: 1, layers: x})
		if e != nil {
			return e
		}
		grams, e := AttributeMaterial(ctx, tx, 1, false, 100, map[int]float64{0: 20, 1: 80}, MaterialCloseInputs{RunID: 1, layers: x})
		if e != nil {
			return e
		}
		if grams == nil || *grams != 34 {
			t.Fatalf("last known layer 70/100 of 20g and 5/20 of 80g: %v", grams)
		}
		_, e = tx.Exec("UPDATE print_log_entries SET completed_at=CURRENT_TIMESTAMP,status='failed',filament_used_grams=? WHERE id=1", *grams)
		if e != nil {
			return e
		}
		return SettleRun(ctx, tx, 1)
	})
	f.transaction(func(ctx context.Context, tx *sql.Tx) error { return SettleRun(ctx, tx, 1) })
	if f.value("SELECT weight_used FROM spool WHERE id=1") != 14 || f.value("SELECT weight_used FROM spool WHERE id=2") != 20 || f.value("SELECT weight_used FROM spool WHERE id=3") != 0 || f.value("SELECT sum(weight_used) FROM spool_usage_history") != 34 {
		t.Fatal("sliced trays doubled, partial ratios incorrect, or unused roll charged")
	}
}
func TestRemoteRemainUsesSpoolWeightAndPreservesUncertainDelivery(t *testing.T) {
	f := setup(t)
	f.printer()
	f.db.SetMaxOpenConns(1)
	applied := 0.
	responses := 0
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		var n int
		if e := f.db.QueryRowContext(ctx, "SELECT count(*) FROM printers").Scan(&n); e != nil {
			t.Error(e)
			w.WriteHeader(503)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/spool/9":
			json.NewEncoder(w).Encode(row{"id": 9, "initial_weight": 500, "filament": row{"weight": 1100}})
		case "PUT /api/v1/spool/9/use":
			var d row
			json.NewDecoder(r.Body).Decode(&d)
			applied += num(d["use_weight"])
			responses++
			w.WriteHeader(500) // upstream may already have applied the increment
		default:
			t.Error(r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	defer remote.Close()
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('spoolman_url',?)", remote.URL)
	f.exec("INSERT INTO spoolman_slot_assignments(printer_id,ams_id,tray_id,spoolman_spool_id) VALUES(1,0,1,9)")
	f.exec("INSERT INTO print_log_entries(id,printer_id,status) VALUES(2,1,'printing')")
	view := func(remain any) map[string]any {
		return map[string]any{"ams": map[string]any{"tray_now": 1, "ams": []any{row{"id": "0", "tray": []any{row{"id": 1, "tray_type": "PETG", "tray_weight": 3000, "remain": remain}}}}}}
	}
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		if e := SnapshotRun(ctx, tx, 2); e != nil {
			return e
		}
		return ObserveMaterial(ctx, tx, 2, 1, view(90), true, false)
	})
	inputs := PrepareMaterialClose(context.Background(), f.db, 2, 1, 0, "", true, view(80))
	f.transaction(func(ctx context.Context, tx *sql.Tx) error {
		if e := ObserveMaterial(ctx, tx, 2, 1, view(80), false, true); e != nil {
			return e
		}
		g, e := AttributeMaterial(ctx, tx, 2, true, 0, nil, inputs)
		if e != nil {
			return e
		}
		if g == nil || *g != 50 {
			t.Fatalf("remote reference weight not used: %v", g)
		}
		if _, e := tx.Exec("UPDATE print_log_entries SET completed_at=CURRENT_TIMESTAMP,status='completed',filament_used_grams=? WHERE id=2", *g); e != nil {
			return e
		}
		return SettleRun(ctx, tx, 2)
	})
	s := &Service{db: f.db}
	if e := s.deliverUsage(context.Background()); e == nil {
		t.Fatal("ambiguous remote write accepted")
	}
	if e := s.deliverUsage(context.Background()); e != nil {
		t.Fatal(e)
	}
	if responses != 1 || applied != 50 || f.value("SELECT count(*) FROM bambastic_material_allocations WHERE state='review'") != 1 {
		t.Fatal(responses, applied)
	}
}
