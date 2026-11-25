package production

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"bambastic/internal/access"
)

func (s *Service) confirmationLinks(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	a, e := s.getAsset(ctx, s.db, p, "archives", pathID(r), "read")
	if e != nil {
		return nil, e
	}
	run, e := one(ctx, s.db, "SELECT * FROM print_log_entries WHERE archive_id=? AND id=? AND confirm_token IS NOT NULL", a["id"], num(r.PathValue("run")))
	if e != nil {
		return nil, e
	}
	if !scope(p, num(run["printer_id"])) || !owned(p, "archives", "update", num(run["created_by_id"])) {
		return nil, sql.ErrNoRows
	}
	w.Header().Set("Cache-Control", "private, no-store")
	base := "/confirm/" + val(run["confirm_token"]) + "/"
	return row{"good_url": base + "good", "reject_url": base + "reject", "answered": run["confirm_token_used_at"] != nil}, nil
}
func (s *Service) confirmOutcome(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	token := r.PathValue("token")
	verdict := r.PathValue("verdict")
	if !in(verdict, "good", "reject") {
		return nil, fail(400, "Choose good or reject")
	}
	if token == "" || len(token) > 64 {
		return nil, sql.ErrNoRows
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	run, e := one(ctx, tx, "SELECT l.* FROM print_log_entries l JOIN print_archives a ON a.id=l.archive_id WHERE l.confirm_token=? AND l.completed_at IS NOT NULL AND a.deleted_at IS NULL", token)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	var archive row
	if run == nil {
		archive, e = one(ctx, tx, "SELECT * FROM print_archives WHERE confirm_token=? AND deleted_at IS NULL", token)
		if e != nil {
			return nil, e
		}
		run, e = latestQualityRun(ctx, tx, num(archive["id"]))
		if e != nil {
			return nil, e
		}
	} else {
		archive, e = one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", run["archive_id"])
		if e != nil {
			return nil, e
		}
	}
	holder := run
	if holder == nil || val(run["confirm_token"]) != token {
		holder = archive
	}
	if r.Method == "GET" && strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/confirm/"+token+"/"+verdict, http.StatusSeeOther)
		return nil, nil
	}
	if r.Method == "POST" && holder["confirm_token_used_at"] == nil && holder["user_verdict"] == nil {
		if e = recordOutcome(ctx, tx, archive, run, row{"user_verdict": verdict}, "link"); e != nil {
			return nil, e
		}
		holder["user_verdict"] = verdict
		holder["confirm_token_used_at"] = true
		if e = tx.Commit(); e != nil {
			return nil, e
		}
	}
	return row{"print_name": archive["print_name"], "plate_id": holder["plate_id"], "verdict": verdict, "recorded_verdict": holder["user_verdict"], "answered": holder["confirm_token_used_at"] != nil || holder["user_verdict"] != nil}, nil
}
func latestQualityRun(ctx context.Context, tx *sql.Tx, archiveID int64) (row, error) {
	run, e := one(ctx, tx, "SELECT * FROM print_log_entries WHERE archive_id=? ORDER BY started_at DESC,id DESC LIMIT 1", archiveID)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	return run, e
}
func recordOutcome(ctx context.Context, tx *sql.Tx, archive, run, patch row, source string) error {
	latest, e := latestQualityRun(ctx, tx, num(archive["id"]))
	if e != nil {
		return e
	}
	if v, ok := patch["user_verdict"]; ok {
		if v != nil && !in(val(v), "good", "reject") {
			return fail(422, "Invalid print verdict")
		}
		if v == nil {
			patch["user_verdict_source"] = nil
			patch["user_verdict_at"] = nil
		} else {
			patch["user_verdict_source"] = source
			patch["user_verdict_at"] = time.Now().UTC().Format(time.RFC3339)
		}
	}
	if v, ok := patch["status"]; ok && !in(val(v), "completed", "failed", "aborted", "cancelled") {
		return fail(422, "Invalid print outcome")
	}
	if v, ok := patch["failure_reason"]; ok && !validFailure(val(v)) {
		return fail(422, "Invalid failure reason")
	}
	write := func(table string, v row) error {
		if err := update(ctx, tx, table, num(v["id"]), patch, []string{"user_verdict", "user_verdict_source", "user_verdict_at", "status", "failure_reason"}); err != nil {
			return err
		}
		if patch["user_verdict"] != nil && v["confirm_token"] != nil && v["confirm_token_used_at"] == nil {
			_, err := tx.ExecContext(ctx, "UPDATE "+table+" SET confirm_token_used_at=CURRENT_TIMESTAMP WHERE id=? AND confirm_token_used_at IS NULL", v["id"])
			return err
		}
		return nil
	}
	if run != nil {
		if run["completed_at"] == nil {
			return fail(409, "Print is in progress")
		}
		if e = write("print_log_entries", run); e != nil {
			return e
		}
	}
	if latest == nil || run != nil && num(run["id"]) == num(latest["id"]) {
		return write("print_archives", archive)
	}
	return nil
}
func validFailure(reason string) bool {
	return in(reason, "", "adhesionFailure", "spaghettiDetached", "layerShift", "cloggedNozzle", "filamentRunout", "warping", "stringing", "underExtrusion", "powerFailure", "userCancelled", "noStatusUpdate", "killSwitch", "other")
}
func (s *Service) editArchive(ctx context.Context, p access.Principal, k int64, m row) (any, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	perm := "update"
	if num(m["run_id"]) != 0 {
		perm = "read"
	}
	a, e := s.getAsset(ctx, tx, p, "archives", k, perm)
	if e != nil {
		return nil, e
	}
	k = num(a["id"])
	if !owned(p, "archives", "update", num(a["created_by_id"])) {
		return nil, sql.ErrNoRows
	}
	if src, ok := m["user_verdict_source"]; ok && !in(val(src), "", "dialog", "printer_card", "api") {
		return nil, fail(422, "Invalid verdict source")
	}
	runID := num(m["run_id"])
	outcome := row{}
	for _, key := range []string{"user_verdict", "status", "failure_reason"} {
		if v, ok := m[key]; ok {
			outcome[key] = v
		}
	}
	if runID > 0 {
		for key := range m {
			if !in(key, "run_id", "user_verdict", "user_verdict_source", "status", "failure_reason") {
				return nil, fail(400, "A run edit accepts outcome fields only")
			}
		}
	}
	if len(outcome) > 0 {
		var run row
		if runID > 0 {
			run, e = one(ctx, tx, "SELECT * FROM print_log_entries WHERE archive_id=? AND id=?", k, runID)
		} else {
			run, e = latestQualityRun(ctx, tx, k)
		}
		if e != nil {
			return nil, e
		}
		if run != nil && (!scope(p, num(run["printer_id"])) || !owned(p, "archives", "update", num(run["created_by_id"]))) {
			return nil, sql.ErrNoRows
		}
		src := val(m["user_verdict_source"])
		if src == "" {
			src = "api"
		}
		if e = recordOutcome(ctx, tx, a, run, outcome, src); e != nil {
			return nil, e
		}
	}
	if q, ok := m["quantity"]; ok && (num(q) < 0 || num(q) > 10000) {
		return nil, fail(422, "Invalid part quantity")
	}
	if k := num(m["project_id"]); k != 0 {
		if !p.Has("projects:read") {
			return nil, fail(403, "Project access required")
		}
		if _, e = one(ctx, tx, "SELECT id FROM projects WHERE id=?", k); e != nil {
			return nil, e
		}
	}
	if e = update(ctx, tx, "print_archives", k, m, []string{"notes", "external_url", "project_id", "print_name", "tags", "is_favorite", "quantity", "cost"}); e != nil {
		return nil, e
	}
	v, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	if e = s.presentAsset(ctx, tx, p, "archives", v); e != nil {
		return nil, e
	}
	decodeAsset(v)
	return v, tx.Commit()
}
