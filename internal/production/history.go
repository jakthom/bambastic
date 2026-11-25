package production

import (
	"bambastic/internal/access"
	"database/sql"
	"net/http"
	"strings"
	"time"
)

func (s *Service) historyRoutes(rt register) {
	rt("POST", "/queue/history/clear", s.clearQueueHistory)
	rt("PATCH", "/queue/bulk", s.bulkQueue)
	rt("POST", "/queue/printer/{id}/resume", s.resumeGate)
	rt("POST", "/queue/model/{model}/resume", s.resumeGate)
	for _, m := range []string{"DELETE", "PATCH"} {
		rt(m, "/queue/batches/{id}", s.batchChange)
	}
	rt("POST", "/queue/batches/{id}/ungroup", s.batchChange)
	rt("DELETE", "/print-log/{$}", s.logChange)
	rt("DELETE", "/print-log/{id}", s.logChange)
	rt("PATCH", "/print-log/{id}", s.logChange)
	rt("GET", "/print-log/{id}/thumbnail", s.logThumbnail)
}
func (s *Service) logThumbnail(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	v, e := one(ctx, s.db, "SELECT * FROM print_log_entries WHERE id=?", pathID(r))
	if e != nil {
		return nil, e
	}
	if !scope(p, num(v["printer_id"])) || !owned(p, "archives", "read", num(v["created_by_id"])) {
		return nil, sql.ErrNoRows
	}
	if path := val(v["thumbnail_path"]); path != "" {
		name, e := s.assetPath(path)
		if e != nil {
			return nil, e
		}
		http.ServeFile(w, r, name)
		return nil, nil
	}
	r.SetPathValue("id", val(v["archive_id"]))
	r.SetPathValue("plate", val(v["plate_id"]))
	return s.assetRead("archives", "thumbnail")(w, r)
}
func (s *Service) logChange(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	perm := "delete"
	if r.Method == "PATCH" {
		perm = "update"
	}
	if !p.Has("archives:"+perm+"_own") && !p.Has("archives:"+perm+"_all") {
		return nil, fail(403, "Print log permission denied")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	k := pathID(r)
	q := "SELECT * FROM print_log_entries WHERE completed_at IS NOT NULL"
	args := []any{}
	if k != 0 {
		q += " AND id=?"
		args = append(args, k)
	}
	entries, e := records(ctx, tx, q, args...)
	if e != nil {
		return nil, e
	}
	var changed int64
	for _, v := range entries {
		if !scope(p, num(v["printer_id"])) || !owned(p, "archives", perm, num(v["created_by_id"])) {
			continue
		}
		if r.Method == "DELETE" {
			if _, e = tx.ExecContext(ctx, "INSERT INTO bb_run_sequence(singleton,last_id) VALUES(1,?) ON CONFLICT(singleton) DO UPDATE SET last_id=max(bb_run_sequence.last_id,excluded.last_id)", v["id"]); e != nil {
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, "DELETE FROM print_log_entries WHERE id=?", v["id"]); e != nil {
				return nil, e
			}
			changed++
			continue
		}
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		if status, ok := m["status"]; ok && !in(val(status), "completed", "failed", "stopped", "cancelled", "skipped") {
			return nil, fail(422, "Invalid print outcome")
		}
		if raw, ok := m["failure_reason"]; ok && !validFailure(val(raw)) {
			return nil, fail(422, "Invalid failure reason")
		}
		if e = update(ctx, tx, "print_log_entries", k, m, []string{"status", "failure_reason", "notes"}); e != nil {
			return nil, e
		}
		v, e = one(ctx, tx, "SELECT * FROM print_log_entries WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		delete(v, "confirm_token")
		return v, tx.Commit()
	}
	if k != 0 && changed == 0 {
		return nil, sql.ErrNoRows
	}
	return row{"deleted": changed}, tx.Commit()
}
func in(v string, allowed ...string) bool {
	for _, s := range allowed {
		if s == v {
			return true
		}
	}
	return false
}
func (s *Service) clearQueueHistory(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("queue:delete_all") && !p.Has("queue:delete_own") {
		return nil, fail(403, "Queue deletion permission denied")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rs, e := records(ctx, tx, "SELECT * FROM print_queue WHERE status IN ('completed','failed','cancelled','skipped') AND id NOT IN(SELECT queue_id FROM bb_queue_history_hidden)")
	if e != nil {
		return nil, e
	}
	cleared, kept := 0, 0
	qs := r.URL.Query()
	for _, v := range rs {
		if !queueVisible(p, v, "delete") {
			continue
		}
		k := num(qs.Get("printer_id"))
		if k > 0 && num(v["printer_id"]) != k || k == -1 && v["printer_id"] != nil || qs.Get("status") != "" && qs.Get("status") != v["status"] || qs.Get("location") != "" && qs.Get("location") != v["target_location"] {
			continue
		}
		if num(v["batch_id"]) > 0 {
			kept++
			continue
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO bb_queue_history_hidden VALUES(?)", v["id"]); e != nil {
			return nil, e
		}
		cleared++
	}
	return row{"cleared": cleared, "kept": kept}, tx.Commit()
}
func (s *Service) bulkQueue(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("queue:update_all") && !p.Has("queue:update_own") {
		return nil, fail(403, "Queue update permission required")
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	ids, e := idsOf(m["item_ids"])
	if e != nil {
		return nil, e
	}
	if len(ids) == 0 {
		return nil, fail(422, "Select queue items")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	count := 0
	for _, k := range ids {
		v, e := one(ctx, tx, queueProjection+" WHERE q.id=?", k)
		if e != nil {
			return nil, e
		}
		if !queueVisible(p, v, "update") {
			return nil, sql.ErrNoRows
		}
		if v["status"] != "pending" {
			continue
		}
		if e = s.editQueueFields(r, tx, p, v, m); e != nil {
			return nil, e
		}
		count++
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	return row{"updated_count": count, "skipped_count": len(ids) - count, "message": "Queue updated"}, nil
}
func (s *Service) editQueueFields(r *http.Request, tx *sql.Tx, p access.Principal, v, m row) error {
	ctx := r.Context()
	if x, ok := m["manual_start"]; ok && !flag(x) && flag(v["review_required"]) && !p.Has("queue:update_all") {
		return fail(403, "Job requires review")
	}
	if e := heatOptions(m); e != nil {
		return e
	}
	patch := row{}
	for k, x := range m {
		patch[k] = x
	}
	if x, ok := patch["scheduled_time"]; ok && val(x) != "" {
		tm, e := time.Parse(time.RFC3339, val(x))
		if e != nil {
			return fail(422, "Invalid earliest start")
		}
		patch["scheduled_time"] = tm.UTC().Format(time.RFC3339)
	}
	if raw, ok := patch["printer_id"]; ok {
		k := num(raw)
		if k <= 0 {
			if val(v["target_model"]) == "" && val(patch["target_model"]) == "" {
				return fail(422, "A target printer or model is required")
			}
			if p.KeyID != 0 && p.PrinterIDs != nil {
				return fail(403, "A pinned printer is required")
			}
		}
		if k > 0 {
			if !p.AllowsPrinter(k) {
				return sql.ErrNoRows
			}
			if _, e := one(ctx, tx, "SELECT id FROM printers WHERE id=? AND is_active", k); e != nil {
				return e
			}
		}
		patch["printer_id"] = id(k)
		if _, e := tx.ExecContext(ctx, "INSERT INTO bb_submission_target VALUES(?,?) ON CONFLICT(queue_id) DO UPDATE SET printer_id=excluded.printer_id", v["id"], id(k)); e != nil {
			return e
		}
	}
	for _, field := range []string{"bed_levelling", "flow_cali", "nozzle_offset_cali"} {
		if x, ok := patch[field]; ok && !in(val(x), "on", "off", "auto") {
			return fail(422, "Invalid calibration option")
		}
	}
	for _, key := range []string{"ams_mapping", "filament_overrides", "nozzle_mapping", "nozzle_rack_choice", "required_filament_types"} {
		if x, ok := patch[key]; ok && x != nil {
			patch[key] = jstr(x)
		}
	}
	return update(ctx, tx, "print_queue", num(v["id"]), patch, []string{"preheat_override", "preheat_chamber_target_override", "printer_id", "target_model", "target_location", "scheduled_time", "manual_start", "require_previous_success", "auto_off_after", "ams_mapping", "filament_overrides", "nozzle_mapping", "nozzle_rack_choice", "required_filament_types", "bed_levelling", "flow_cali", "nozzle_offset_cali", "vibration_cali", "layer_inspect", "timelapse", "use_ams", "confirm_outcome", "gcode_injection"})
}
func (s *Service) resumeGate(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("queue:update_all") && !p.Has("queue:update_own") {
		return nil, fail(403, "Queue update permission required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	k := pathID(r)
	model := r.PathValue("model")
	if k != 0 && !p.AllowsPrinter(k) {
		return nil, sql.ErrNoRows
	}
	rs, e := records(ctx, tx, "SELECT q.*,p.model printer_model FROM print_queue q LEFT JOIN printers p ON p.id=q.printer_id WHERE q.status IN('pending','skipped') AND q.require_previous_success")
	if e != nil {
		return nil, e
	}
	acknowledged, restored := 0, 0
	for _, v := range rs {
		if !queueVisible(p, v, "update") || k != 0 && num(v["printer_id"]) != k || model != "" && modelName(v["target_model"]) != modelName(model) && modelName(v["printer_model"]) != modelName(model) {
			continue
		}
		if v["status"] == "skipped" {
			restored++
		}
		_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='pending',gate_acknowledged=1,waiting_reason=NULL WHERE id=?", v["id"])
		if e != nil {
			return nil, e
		}
		acknowledged++
	}
	return row{"acknowledged": acknowledged, "restored": restored}, tx.Commit()
}
func (s *Service) batchChange(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	b, e := one(ctx, tx, "SELECT * FROM print_batches WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	perm := "update"
	if r.Method == "DELETE" {
		perm = "delete"
	}
	if !owned(p, "queue", perm, num(b["created_by_id"])) {
		return nil, sql.ErrNoRows
	}
	b, e = s.batchView(ctx, tx, p, b)
	if e != nil {
		return nil, e
	}
	if r.Method == "POST" {
		res, e := tx.ExecContext(ctx, "UPDATE print_queue SET batch_id=NULL WHERE batch_id=?", k)
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		if _, e = tx.ExecContext(ctx, "DELETE FROM print_batches WHERE id=?", k); e != nil {
			return nil, e
		}
		return row{"ungrouped_count": n, "message": "Batch ungrouped"}, tx.Commit()
	}
	if r.Method == "DELETE" {
		_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE batch_id=? AND status='pending'", k)
		if e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "UPDATE print_batches SET status='cancelled' WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		return row{"status": "cancelled"}, tx.Commit()
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	if name, ok := m["name"]; ok && strings.TrimSpace(val(name)) == "" {
		return nil, fail(422, "Batch name required")
	}
	if e = update(ctx, tx, "print_batches", k, m, []string{"name", "notes", "project_id", "due_date", "status"}); e != nil {
		return nil, e
	}
	if raw, ok := m["plates"]; ok {
		items, ok := raw.([]any)
		if !ok {
			return nil, fail(422, "Invalid plate demand")
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM print_batch_plates WHERE batch_id=?", k); e != nil {
			return nil, e
		}
		seen := map[int64]bool{}
		total := int64(0)
		for i, v := range items {
			pl, ok := v.(map[string]any)
			if !ok {
				return nil, fail(422, "Invalid plate target")
			}
			plate, qty := num(pl["plate_id"]), num(pl["quantity_target"])
			if seen[plate] || qty < 0 || qty > 999 {
				return nil, fail(422, "Invalid plate target")
			}
			seen[plate] = true
			total += qty
			_, e = insert(ctx, tx, "print_batch_plates", row{"batch_id": k, "plate_id": id(plate), "plate_name": pl["plate_name"], "quantity_target": qty, "sort_order": i})
			if e != nil {
				return nil, e
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE print_batches SET quantity=? WHERE id=?", total, k); e != nil {
			return nil, e
		}
	}
	b, e = one(ctx, tx, "SELECT * FROM print_batches WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	b, e = s.batchView(ctx, tx, p, b)
	if e != nil {
		return nil, e
	}
	return b, tx.Commit()
}
