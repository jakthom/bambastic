package production

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/management"
)

func (s *Service) queueRoutes(rt register) {
	for _, p := range []string{"/queue", "/queue/{$}"} {
		rt("GET", p, s.queueList(false))
		rt("POST", p, s.enqueue)
	}
	rt("GET", "/queue/history", s.queueList(true))
	rt("GET", "/queue/{id}", s.queueDetail)
	rt("PATCH", "/queue/{id}", s.queueEdit)
	rt("DELETE", "/queue/{id}", s.queueAction("cancel"))
	for _, a := range []string{"start", "stop", "cancel"} {
		rt("POST", "/queue/{id}/"+a, s.queueAction(a))
	}
	rt("POST", "/queue/reorder", s.reorder)
	rt("GET", "/queue/batches", s.batches)
	rt("POST", "/queue/batches", s.newBatch)
	rt("GET", "/queue/batches/{id}", s.batchDetail)
	rt("POST", "/queue/batches/{id}/dispatch", s.batchDispatch)
	for _, p := range []string{"/print-log", "/print-log/{$}"} {
		rt("GET", p, s.printLog)
	}
}

const queueProjection = `SELECT q.*,a.print_name archive_name,f.filename library_file_name,p.name printer_name,b.name batch_name,u.username created_by_username, coalesce(ar.required,0) review_required, (SELECT json_group_array(json_object('id',v.id,'library_file_id',v.library_file_id,'filename',v.filename,'position',v.position,'target_model',v.target_model,'plate_id',v.plate_id,'attempt_count',v.attempt_count)) FROM (SELECT z.*,lf.filename FROM print_queue_variants z JOIN library_files lf ON lf.id=z.library_file_id WHERE z.queue_item_id=q.id ORDER BY z.position) v) variants FROM print_queue q LEFT JOIN print_archives a ON a.id=q.archive_id LEFT JOIN library_files f ON f.id=q.library_file_id LEFT JOIN printers p ON p.id=q.printer_id LEFT JOIN print_batches b ON b.id=q.batch_id LEFT JOIN users u ON u.id=q.created_by_id LEFT JOIN bb_review ar ON ar.queue_id=q.id`

func queueVisible(p access.Principal, v row, action string) bool {
	return scope(p, num(v["printer_id"])) && owned(p, "queue", action, num(v["created_by_id"]))
}
func queueView(v row) row {
	for _, k := range []string{"require_previous_success", "auto_off_after", "manual_start", "filament_short", "skip_filament_check", "vibration_cali", "layer_inspect", "timelapse", "use_ams", "confirm_outcome", "archive_deleted", "archive_has_slicer_ams_mapping", "been_jumped", "gcode_injection", "cleanup_library_after_dispatch", "gate_acknowledged", "review_required"} {
		if value, ok := v[k]; ok && value != nil {
			v[k] = flag(value)
		}
	}
	for _, k := range []string{"required_filament_types", "ams_mapping", "filament_overrides", "nozzle_rack_choice", "nozzle_mapping", "color_substitutions"} {
		v[k] = unpack(v[k])
	}
	v["variants"] = unpack(v["variants"])
	if v["variants"] == nil {
		v["variants"] = []any{}
	}
	return v
}
func (s *Service) queueList(history bool) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		if !p.Has("queue:read_all") && !p.Has("queue:read_own") {
			return nil, fail(403, "Permission denied")
		}
		q := queueProjection + " WHERE q.id NOT IN (SELECT queue_id FROM bb_queue_history_hidden)"
		var args []any
		qs := r.URL.Query()
		if history {
			q += " AND q.status IN ('completed','failed','cancelled','skipped')"
		}
		if st := qs.Get("status"); st != "" {
			ss := strings.Split(st, ",")
			q += " AND q.status IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ss)), ",") + ")"
			for _, st := range ss {
				args = append(args, st)
			}
		}
		if pr := num(qs.Get("printer_id")); pr > 0 {
			q += " AND q.printer_id=?"
			args = append(args, pr)
		}
		if m := qs.Get("target_model"); m != "" {
			q += " AND lower(q.target_model)=lower(?)"
			args = append(args, m)
		}
		if history {
			q += " ORDER BY q.completed_at DESC,q.id DESC"
		} else {
			q += " ORDER BY q.position,q.id"
		}
		rs, e := records(ctx, s.db, q, args...)
		if e != nil {
			return nil, e
		}
		out := []row{}
		for _, v := range rs {
			if queueVisible(p, v, "read") {
				out = append(out, queueView(v))
			}
		}
		if history {
			locs := map[string]bool{}
			for _, v := range out {
				if l := val(v["target_location"]); l != "" {
					locs[l] = true
				}
			}
			return row{"items": boundPage(out, num(qs.Get("offset")), num(qs.Get("limit"))), "total": len(out), "locations": sortedNames(locs)}, nil
		}
		return out, nil
	}
}
func (s *Service) queueDetail(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	v, e := one(r.Context(), s.db, queueProjection+" WHERE q.id=?", pathID(r))
	if e != nil {
		return nil, e
	}
	if !queueVisible(p, v, "read") {
		return nil, sql.ErrNoRows
	}
	return queueView(v), nil
}
func (s *Service) enqueue(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "queue:create"); e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	return s.enqueueInput(r.Context(), access.From(r.Context()), r.Header.Get("Idempotency-Key"), m)
}
func (s *Service) enqueueInput(ctx context.Context, p access.Principal, key string, m row) (any, error) {
	if !p.Has("queue:create") {
		return nil, fail(403, "Queue permission required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if key == "" {
		key = val(m["request_key"])
	}
	if len(key) > 256 {
		return nil, fail(422, "Request key is too long")
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(jstr(m))))
	actor := p.ID
	if p.KeyID != 0 {
		actor = -p.KeyID
	}
	if key != "" {
		old, e := one(ctx, tx, "SELECT * FROM bb_requests WHERE actor=? AND request_key=?", actor, key)
		if e == nil {
			if old["fingerprint"] != hash {
				return nil, fail(409, "Request key already identifies different work")
			}
			stored, ok := unpack(old["result"]).(map[string]any)
			if !ok {
				return nil, errors.New("corrupt request result")
			}
			v, e := one(ctx, tx, queueProjection+" WHERE q.id=?", num(stored["id"]))
			if e != nil {
				return nil, e
			}
			if !queueVisible(p, v, "read") {
				return nil, sql.ErrNoRows
			}
			return stored, nil
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	ids, e := s.createCopies(ctx, tx, p, m)
	if e != nil {
		return nil, e
	}
	v, e := one(ctx, tx, queueProjection+" WHERE q.id=?", ids[0])
	if e != nil {
		return nil, e
	}
	queueView(v)
	v["queue_item_ids"] = ids
	if key != "" {
		_, e = tx.ExecContext(ctx, "INSERT INTO bb_requests VALUES(?,?,?,?)", actor, key, hash, jstr(v))
		if e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	return v, nil
}
func (s *Service) createCopies(ctx context.Context, tx *sql.Tx, p access.Principal, m row) ([]int64, error) {
	if a, ok := m["variants"].([]any); ok && len(a) > 0 {
		return s.alternativeOrder(ctx, tx, p, m)
	}
	aid, lid, pr := num(m["archive_id"]), num(m["library_file_id"]), num(m["printer_id"])
	if (aid == 0) == (lid == 0) {
		return nil, fail(422, "Choose one archive or library file")
	}
	if pr == 0 && strings.TrimSpace(val(m["target_model"])) == "" {
		return nil, fail(422, "Choose a printer or target model")
	}
	if !scope(p, pr) || pr == 0 && p.KeyID > 0 && p.PrinterIDs != nil {
		return nil, fail(403, "Printer outside permitted scope")
	}
	if pr > 0 {
		if _, e := one(ctx, tx, "SELECT id FROM printers WHERE id=? AND is_active", pr); e != nil {
			return nil, e
		}
	}
	kind := "archives"
	assetID := aid
	action := "reprint"
	if lid != 0 {
		kind = "library"
		assetID = lid
		action = "read"
	}
	asset, e := s.getAsset(ctx, tx, p, kind, assetID, action)
	if e != nil {
		return nil, e
	}
	inf, e := s.meta(kind, asset)
	if e != nil {
		return nil, e
	}
	plateID := num(m["plate_id"])
	if plateID == 0 {
		for _, pl := range inf.Plates {
			if pl.Printable {
				plateID = pl.Index
				break
			}
		}
	}
	var selected *plate
	for _, pl := range inf.Plates {
		if pl.Index == plateID && pl.Printable {
			selected = &pl
			break
		}
	}
	if selected == nil {
		return nil, fail(422, "The selected plate requires slicing before printing")
	}
	qty := int64(1)
	if m["quantity"] != nil {
		qty = num(m["quantity"])
	}
	if qty < 1 || qty > 1000 {
		return nil, fail(422, "Quantity must be 1–1000")
	}
	if bid := num(m["batch_id"]); bid != 0 {
		b, e := one(ctx, tx, "SELECT * FROM print_batches WHERE id=?", bid)
		if e != nil {
			return nil, e
		}
		if !owned(p, "queue", "update", num(b["created_by_id"])) {
			return nil, fail(403, "Cannot add work to this batch")
		}
		if ba := num(b["archive_id"]); ba != 0 && ba != aid {
			return nil, fail(422, "Archive differs from batch source")
		}
		if bl := num(b["library_file_id"]); bl != 0 && bl != lid {
			return nil, fail(422, "File differs from batch source")
		}
	}
	estimate, e := management.AuthorizeQueue(ctx, tx, p.ID, num(m["cost_center_id"]), aid, lid, plateID, int(qty))
	if e != nil {
		return nil, fail(400, e.Error())
	}
	when := any(nil)
	if st := val(m["scheduled_time"]); st != "" {
		tm, e := time.Parse(time.RFC3339, st)
		if e != nil {
			return nil, fail(422, "Scheduled time requires an RFC3339 timestamp")
		}
		when = tm.UTC().Format(time.RFC3339)
	}
	if e := heatOptions(m); e != nil {
		return nil, e
	}
	required := !(p.Has("queue:start_unreviewed") || p.Has("queue:update_all"))
	manual := required || flag(m["manual_start"])
	pos, e := one(ctx, tx, "SELECT coalesce(max(position),0)+1 pos FROM print_queue")
	if e != nil {
		return nil, e
	}
	n := num(pos["pos"])
	if flag(m["insert_at_top"]) {
		n = 1
	} else if t := num(m["insert_position"]); t > 0 && t < n {
		n = t
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET position=position+? WHERE position>=?", qty, n); e != nil {
		return nil, e
	}
	base := row{"printer_id": id(pr), "archive_id": id(aid), "library_file_id": id(lid), "plate_id": plateID, "target_model": m["target_model"], "target_location": m["target_location"], "status": "pending", "manual_start": manual, "require_previous_success": flag(m["require_previous_success"]), "auto_off_after": flag(m["auto_off_after"]), "been_jumped": false, "gcode_injection": flag(m["gcode_injection"]), "vibration_cali": true, "timelapse": flag(m["timelapse"]), "layer_inspect": flag(m["layer_inspect"]), "use_ams": true, "created_by_id": id(p.ID), "estimated_cost": estimate, "cost_center_id": id(num(m["cost_center_id"])), "project_id": id(num(m["project_id"])), "batch_id": id(num(m["batch_id"])), "print_time_seconds": selected.Seconds, "scheduled_time": when, "confirm_outcome": flag(m["confirm_outcome"]), "waiting_reason": "Waiting for an eligible printer"}
	if manual {
		base["waiting_reason"] = "Waiting for manual release"
	}
	for _, k := range []string{"preheat_override", "preheat_chamber_target_override", "use_ams", "vibration_cali", "skip_filament_check", "bed_levelling", "flow_cali", "nozzle_offset_cali"} {
		if x := m[k]; x != nil {
			base[k] = x
		}
	}
	types, exact := plateRequirements(*selected, flag(m["force_color_match"]))
	if m["required_filament_types"] == nil && len(types) > 0 {
		base["required_filament_types"] = jstr(types)
	}
	if len(exact) > 0 && m["filament_overrides"] == nil {
		base["filament_overrides"] = jstr(exact)
	}
	for _, k := range []string{"required_filament_types", "ams_mapping", "filament_overrides", "nozzle_rack_choice", "nozzle_mapping", "nozzles_info"} {
		if x := m[k]; x != nil {
			base[k] = jstr(x)
		}
	}
	ids := []int64{}
	for i := int64(0); i < qty; i++ {
		base["position"] = n + i
		k, e := insert(ctx, tx, "print_queue", base)
		if e != nil {
			return nil, e
		}
		if p.KeyID != 0 {
			if _, e = tx.ExecContext(ctx, "INSERT INTO bb_queue_actor VALUES(?,?)", k, p.KeyID); e != nil {
				return nil, e
			}
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO bb_submission_target VALUES(?,?)", k, id(pr)); e != nil {
			return nil, e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO bb_review VALUES(?,?)", k, required); e != nil {
			return nil, e
		}
		ids = append(ids, k)
	}
	return ids, nil
}
func (s *Service) queueAction(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		k := pathID(r)
		var account access.Principal
		if action == "start" {
			before, e := one(ctx, s.db, queueProjection+" WHERE q.id=?", k)
			if e != nil {
				return nil, e
			}
			uid := num(before["created_by_id"])
			if uid == 0 {
				uid = p.ID
			}
			var key int64
			err := s.db.QueryRowContext(ctx, "SELECT key_id FROM bb_queue_actor WHERE queue_id=?", k).Scan(&key)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if key != 0 {
				account, e = s.durableActor(ctx, uid, key)
			} else if uid == p.ID {
				account = p
			} else {
				account, e = s.owner(ctx, uid)
			}
			if e != nil {
				return nil, fail(403, "Queue owner is unavailable")
			}

		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		v, e := one(ctx, tx, queueProjection+" WHERE q.id=?", k)
		if e != nil {
			return nil, e
		}
		perm := "update"
		if action == "cancel" {
			perm = "delete"
		}
		claimable := action == "start" && num(v["created_by_id"]) == 0 && p.Has("queue:update_own") && p.Has("queue:start_unreviewed") && scope(p, num(v["printer_id"]))
		if !queueVisible(p, v, perm) && !claimable {
			return nil, fail(403, "Cannot change this queue item")
		}
		st := val(v["status"])
		switch action {
		case "start":
			if num(v["printer_id"]) == 0 && p.KeyID > 0 && p.PrinterIDs != nil {
				return nil, fail(403, "Select a printer in this key’s scope")
			}
			if !p.Has("queue:update_all") && (!p.Has("queue:start_unreviewed") || flag(v["review_required"])) {
				return nil, fail(403, "Job requires an authorized reviewer")
			}
			if st != "pending" {
				return nil, fail(409, "Only pending jobs can be started")
			}
			_, e = tx.ExecContext(ctx, "UPDATE print_queue SET manual_start=0,created_by_id=coalesce(created_by_id,?),skip_filament_check=CASE WHEN ? THEN TRUE ELSE skip_filament_check END,scheduled_time=NULL,waiting_reason=NULL WHERE id=? AND status='pending'", id(p.ID), flag(r.URL.Query().Get("skip_filament_check")), k)
			if e == nil && v["created_by_id"] == nil && p.KeyID != 0 {
				_, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO bb_queue_actor VALUES(?,?)", k, p.KeyID)
			}
			if e == nil {
				if e = management.RecheckQueue(ctx, tx, account, k); e != nil {
					return nil, fail(409, e.Error())
				}
				_, e = tx.ExecContext(ctx, "UPDATE bb_review SET required=0 WHERE queue_id=?", k)
			}
		case "cancel":
			if st != "pending" && st != "dispatching" {
				return nil, fail(409, "Only pending or uploading work can be cancelled")
			}
			var phase string
			_ = tx.QueryRowContext(ctx, "SELECT phase FROM bb_attempts WHERE queue_id=?", k).Scan(&phase)
			if phase == "sent" || phase == "running" {
				return nil, fail(409, "Start has been sent. Wait for printer confirmation")
			}
			_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='cancelled',completed_at=CURRENT_TIMESTAMP,waiting_reason=NULL WHERE id=?", k)
			if e == nil {
				_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='cancelled' WHERE queue_id=?", k)
			}
		case "stop":
			if st != "printing" || !p.Has("printers:control") {
				return nil, fail(409, "Job must be running and controllable")
			}
			if e = tx.Commit(); e != nil {
				return nil, e
			}
			return s.deviceCommand(ctx, num(v["printer_id"]), "stop")
		}
		if e != nil {
			return nil, e
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		s.kick()
		return row{"success": true, "queue_item_id": k}, nil
	}
}
func (s *Service) queueEdit(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	v, e := one(ctx, tx, queueProjection+" WHERE q.id=?", pathID(r))
	if e != nil {
		return nil, e
	}
	p := access.From(ctx)
	if !queueVisible(p, v, "update") {
		return nil, fail(403, "Cannot edit job")
	}
	if v["status"] != "pending" {
		return nil, fail(409, "Job already claimed")
	}
	if e = s.editQueueFields(r, tx, p, v, m); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	return s.queueDetail(w, r)
}
func (s *Service) reorder(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "queue:reorder"); e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	ids, _ := m["item_ids"].([]any)
	if len(ids) == 0 {
		return nil, fail(422, "item_ids is required")
	}
	ctx := r.Context()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	seen := map[int64]bool{}
	slots := []int{}
	for _, x := range ids {
		k := num(x)
		if seen[k] {
			return nil, fail(422, "Duplicate queue item")
		}
		seen[k] = true
		v, e := one(ctx, tx, "SELECT * FROM print_queue WHERE id=? AND status='pending'", k)
		if e != nil {
			return nil, e
		}
		if !queueVisible(access.From(ctx), v, "read") {
			return nil, sql.ErrNoRows
		}
		slots = append(slots, int(num(v["position"])))
	}
	sort.Ints(slots)
	for i, x := range ids {
		if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET position=? WHERE id=?", slots[i], num(x)); e != nil {
			return nil, e
		}
	}
	return row{"success": true}, tx.Commit()
}
func (s *Service) printLog(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("archives:read_all") && !p.Has("archives:read_own") {
		return nil, fail(403, "Permission denied")
	}
	rs, e := records(ctx, s.db, "SELECT * FROM print_log_entries WHERE completed_at IS NOT NULL ORDER BY completed_at DESC,id DESC")
	if e != nil {
		return nil, e
	}
	out := []row{}
	for _, v := range rs {
		if scope(p, num(v["printer_id"])) && owned(p, "archives", "read", num(v["created_by_id"])) {
			delete(v, "confirm_token")
			delete(v, "timelapse_baseline")
			out = append(out, v)
		}
	}
	total := len(out)
	off := num(r.URL.Query().Get("offset"))
	if off < 0 {
		off = 0
	}
	if off > int64(total) {
		off = int64(total)
	}
	out = out[off:]
	limit := num(r.URL.Query().Get("limit"))
	if limit > 0 && limit < int64(len(out)) {
		out = out[:limit]
	}
	return row{"items": out, "total": total, "deleted_archive_ids": []int{}}, nil
}
