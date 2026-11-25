package production

import (
	"bambastic/internal/access"
	"context"
	"database/sql"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
)

func (s *Service) StorageRoot() string { return s.dir }

// Intake stores a verified slicer upload in the same transaction as queue admission or review approval.
func (s *Service) Intake(ctx context.Context, tx *sql.Tx, p access.Principal, src io.Reader, name string, printer, project int64, model string, enqueue, manual bool, options map[string]any) (int64, []int64, error) {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if !strings.EqualFold(filepath.Ext(name), ".3mf") {
		return 0, nil, fail(422, "Expected a 3MF file")
	}
	if !p.Has("archives:create") || !scope(p, printer) {
		return 0, nil, fail(403, "Archive creation denied")
	}
	if project != 0 {
		if !p.Has("projects:read") {
			return 0, nil, fail(403, "Project access required")
		}
		var exists int64
		if err := tx.QueryRowContext(ctx, "SELECT id FROM projects WHERE id=?", project).Scan(&exists); err != nil {
			return 0, nil, err
		}
	}
	v, e := s.prepareAsset(src, "archives", name)
	if e != nil {
		return 0, nil, e
	}
	v["created_by_id"] = id(p.ID)
	v["printer_id"] = id(printer)
	v["project_id"] = id(project)
	raw, _ := access.Setting(ctx, tx, "virtual_printer_archive_name_source", "metadata")
	path, e := s.assetPath(v["file_path"])
	if e != nil {
		return 0, nil, e
	}
	v["print_name"] = IntakeDisplayName(path, name, raw == "filename")
	var aid int64
	if project == 0 && val(v["gcode_hash"]) != "" {
		old, e := one(ctx, tx, `SELECT a.* FROM print_archives a WHERE a.deleted_at IS NULL AND a.project_id IS NULL AND a.library_file_id IS NULL AND a.gcode_hash=? AND a.created_by_id IS ? AND a.printer_id IS ? AND NOT EXISTS(SELECT 1 FROM print_queue q WHERE q.archive_id=a.id AND q.status IN ('pending','dispatching','printing')) AND NOT EXISTS(SELECT 1 FROM print_log_entries l WHERE l.archive_id=a.id AND l.completed_at IS NULL) ORDER BY a.id LIMIT 1`, v["gcode_hash"], id(p.ID), id(printer))
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return 0, nil, e
		}
		if old != nil && s.accessible(ctx, tx, p, "archives", old, "update") {
			aid = num(old["id"])
			if old["file_path"] != v["file_path"] {
				if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO bb_reclaim(path) VALUES(?)", old["file_path"]); e != nil {
					return 0, nil, e
				}
			}
			if e = update(ctx, tx, "print_archives", aid, v, []string{"filename", "file_path", "file_size", "content_hash", "gcode_hash", "extra_data", "plate_estimates", "sliced_for_model", "print_time_seconds", "filament_used_grams"}); e != nil {
				return 0, nil, e
			}
		}
	}
	if aid == 0 {
		aid, e = insert(ctx, tx, "print_archives", v)
	}
	if e != nil || !enqueue {
		return aid, nil, e
	}
	if !p.Has("queue:create") {
		return 0, nil, fail(403, "Queue permission required")
	}
	a, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", aid)
	if e != nil {
		return 0, nil, e
	}
	inf, e := s.meta("archives", a)
	if e != nil {
		return 0, nil, e
	}
	ids := []int64{}
	for _, pl := range inf.Plates {
		if !pl.Printable {
			continue
		}
		if want := num(options["plate_id"]); want != 0 && want != pl.Index {
			continue
		}
		m := row{"archive_id": aid, "printer_id": id(printer), "target_model": model, "plate_id": pl.Index, "manual_start": manual}
		for _, field := range []string{"ams_mapping", "timelapse", "use_ams", "bed_levelling", "flow_cali", "layer_inspect", "vibration_cali", "nozzle_offset_cali", "gcode_injection", "force_color_match"} {
			if x := options[field]; x != nil {
				m[field] = x
			}
		}
		items, e := s.createCopies(ctx, tx, p, m)
		if e != nil {
			return 0, nil, e
		}
		if p.ID == 0 {
			for _, k := range items {
				if _, e = tx.ExecContext(ctx, "INSERT INTO bb_claimed_intake(queue_id) VALUES(?)", k); e != nil {
					return 0, nil, e
				}
			}
		}
		ids = append(ids, items...)
	}
	if len(ids) == 0 {
		return 0, nil, fail(422, "No printable plate in upload")
	}
	return aid, ids, nil
}
func (s *Service) bulkStatus(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "printers:read"); e != nil {
		return nil, e
	}
	out := []row{}
	for _, tok := range strings.Split(r.URL.Query().Get("ids"), ",") {
		id := num(tok)
		if !access.From(r.Context()).AllowsPrinter(id) {
			continue
		}
		v, e := one(r.Context(), s.db, "SELECT * FROM printers WHERE id=?", id)
		if e == nil {
			out = append(out, s.state(id, v))
		}
	}
	return out, nil
}

// CreateCopies admits work inside the caller's transaction so pipelines and batch
// submissions can commit their tracking records together with the actual demand.
func (s *Service) CreateCopies(ctx context.Context, tx *sql.Tx, p access.Principal, options map[string]any) ([]int64, error) {
	if !p.Has("queue:create") {
		return nil, fail(403, "Queue permission required")
	}
	return s.createCopies(access.With(ctx, p), tx, p, options)
}
