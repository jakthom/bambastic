package production

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"bambastic/internal/access"
)

func (s *Service) batchView(ctx context.Context, d database, p access.Principal, b row) (row, error) {
	if !owned(p, "queue", "read", num(b["created_by_id"])) {
		return nil, sql.ErrNoRows
	}
	jobs, e := records(ctx, d, "SELECT * FROM print_queue WHERE batch_id=? ORDER BY id", b["id"])
	if e != nil {
		return nil, e
	}
	plates, e := records(ctx, d, "SELECT * FROM print_batch_plates WHERE batch_id=? ORDER BY sort_order,id", b["id"])
	if e != nil {
		return nil, e
	}
	for _, v := range jobs {
		if !queueVisible(p, v, "read") {
			return nil, sql.ErrNoRows
		}
	}
	progress := map[int64]row{}
	for _, v := range plates {
		progress[num(v["plate_id"])] = v
	}
	for _, j := range jobs {
		k := num(j["plate_id"])
		v := progress[k]
		if v == nil {
			v = row{"plate_id": id(k), "quantity_target": 0}
			progress[k] = v
			plates = append(plates, v)
		}
		st := val(j["status"])
		if st == "dispatching" {
			st = "printing"
		}
		key := st + "_count"
		v[key] = num(v[key]) + 1
		b[key] = num(b[key]) + 1
		v["dispatched"] = num(v["dispatched"]) + 1
	}
	var want, remaining, dispatchable int64
	for _, v := range plates {
		target := num(v["quantity_target"])
		left := max(int64(0), target-num(v["completed_count"]))
		owed := max(int64(0), left-num(v["printing_count"])-num(v["pending_count"]))
		want += target
		remaining += left
		dispatchable += owed
		v["remaining"] = left
		v["can_dispatch"] = owed > 0 && num(v["dispatched"]) > 0
		v["dispatchable_count"] = owed
	}
	b["has_targets"] = want > 0
	b["target_count"] = want
	b["remaining_count"] = remaining
	b["dispatchable_count"] = dispatchable
	b["plates"] = plates
	if want > 0 && remaining == 0 {
		b["status"] = "completed"
	}
	return b, nil
}
func (s *Service) batches(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("queue:read_all") && !p.Has("queue:read_own") {
		return nil, fail(403, "Permission denied")
	}
	rs, e := records(ctx, s.db, "SELECT * FROM print_batches ORDER BY id DESC")
	if e != nil {
		return nil, e
	}
	out := []row{}
	for _, b := range rs {
		for _, k := range []string{"external_source", "external_ref"} {
			if x := r.URL.Query().Get(k); x != "" && val(b[k]) != x {
				b = nil
				break
			}
		}
		if b == nil {
			continue
		}
		v, e := s.batchView(ctx, s.db, p, b)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) batchDetail(w http.ResponseWriter, r *http.Request) (any, error) {
	b, e := one(r.Context(), s.db, "SELECT * FROM print_batches WHERE id=?", pathID(r))
	if e != nil {
		return nil, e
	}
	return s.batchView(r.Context(), s.db, access.From(r.Context()), b)
}

var externalName = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

func (s *Service) newBatch(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "queue:create"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := access.From(ctx)
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	name := strings.TrimSpace(val(m["name"]))
	if name == "" {
		return nil, fail(422, "Batch name is required")
	}
	src, ref := val(m["external_source"]), val(m["external_ref"])
	if (src == "") != (ref == "") || src != "" && (!externalName.MatchString(src) || len(ref) > 255) {
		return nil, fail(422, "Invalid external order reference")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	for kind, field := range map[string]string{"archives": "archive_id", "library": "library_file_id"} {
		if k := num(m[field]); k != 0 {
			if _, e = s.getAsset(ctx, tx, p, kind, k, "read"); e != nil {
				return nil, e
			}
		}
	}
	if src != "" {
		if _, e = one(ctx, tx, "SELECT id FROM print_batches WHERE external_source=? AND external_ref=?", src, ref); e == nil {
			return nil, fail(409, "External order already exists")
		} else if !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
	}
	k, e := insert(ctx, tx, "print_batches", row{"name": name, "archive_id": m["archive_id"], "library_file_id": m["library_file_id"], "project_id": m["project_id"], "due_date": m["due_date"], "notes": m["notes"], "external_source": m["external_source"], "external_ref": m["external_ref"], "quantity": 0, "status": "pending", "created_by_id": id(p.ID)})
	if e != nil {
		return nil, e
	}
	ps, _ := m["plates"].([]any)
	seen := map[int64]bool{}
	var total int64
	for i, node := range ps {
		v, ok := node.(map[string]any)
		if !ok {
			return nil, fail(422, "Invalid plate target")
		}
		n := num(v["plate_id"])
		qty := int64(1)
		if v["quantity_target"] != nil {
			qty = num(v["quantity_target"])
		}
		if seen[n] || qty < 0 || qty > 999 {
			return nil, fail(422, "Invalid or duplicate plate target")
		}
		seen[n] = true
		total += qty
		if _, e = insert(ctx, tx, "print_batch_plates", row{"batch_id": k, "plate_id": id(n), "plate_name": v["plate_name"], "quantity_target": qty, "sort_order": i}); e != nil {
			return nil, e
		}
	}
	ids, _ := m["item_ids"].([]any)
	for _, x := range ids {
		q, e := one(ctx, tx, "SELECT * FROM print_queue WHERE id=? AND status='pending' AND batch_id IS NULL", num(x))
		if e != nil {
			return nil, e
		}
		if !queueVisible(p, q, "update") {
			return nil, fail(403, "Cannot group this job")
		}
		if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET batch_id=? WHERE id=?", k, num(x)); e != nil {
			return nil, e
		}
	}
	if total == 0 {
		total = int64(len(ids))
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_batches SET quantity=? WHERE id=?", total, k); e != nil {
		return nil, e
	}
	b, e := one(ctx, tx, "SELECT * FROM print_batches WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	b, e = s.batchView(ctx, tx, p, b)
	if e != nil {
		return nil, e
	}
	return b, tx.Commit()
}
func (s *Service) batchDispatch(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "queue:create"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := access.From(ctx)
	in := row{}
	if r.ContentLength != 0 {
		var e error
		in, e = input(w, r)
		if e != nil {
			return nil, e
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	b, e := one(ctx, tx, "SELECT * FROM print_batches WHERE id=?", pathID(r))
	if e != nil {
		return nil, e
	}
	if !owned(p, "queue", "update", num(b["created_by_id"])) {
		return nil, fail(403, "Cannot dispatch this batch")
	}
	b, e = s.batchView(ctx, tx, p, b)
	if e != nil {
		return nil, e
	}
	limit := num(in["limit"])
	ids := []int64{}
	for _, v := range b["plates"].([]row) {
		pl := num(v["plate_id"])
		if flag(in["only_plate"]) && pl != num(in["plate_id"]) {
			continue
		}
		n := num(v["dispatchable_count"])
		if limit > 0 {
			n = min(n, limit-int64(len(ids)))
		}
		if n <= 0 {
			continue
		}
		seed, e := one(ctx, tx, "SELECT * FROM print_queue WHERE batch_id=? AND plate_id IS ? ORDER BY id DESC LIMIT 1", pathID(r), id(pl))
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, e
		}
		for _, k := range []string{"ams_mapping", "filament_overrides", "nozzle_rack_choice", "required_filament_types"} {
			seed[k] = unpack(seed[k])
		}
		seed["quantity"] = n
		seed["scheduled_time"] = nil
		newIDs, e := s.createCopies(ctx, tx, p, seed)
		if e != nil {
			return nil, e
		}
		ids = append(ids, newIDs...)
	}
	if len(ids) == 0 {
		return nil, fail(409, "No remaining plates with a dispatch template")
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	return row{"dispatched_count": len(ids), "queue_item_ids": ids}, nil
}
