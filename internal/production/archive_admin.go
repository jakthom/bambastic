package production

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

func (s *Service) archiveAdminRoutes(rt register) {
	rt("GET", "/archives/{id}/delete-impact", s.deleteImpact)
	rt("POST", "/archives/recalculate-costs", s.reprice)
	rt("POST", "/archives/backfill-hashes", s.backfillArchives)
	rt("POST", "/archives/search/rebuild-index", s.reindexArchives)
	rt("POST", "/archives/upload-bulk", s.bulkArchiveUploads)
	rt("POST", "/archives/upload-source", s.archiveSourceByName)
	rt("GET", "/archives/purge/preview", s.archivePurge)
	rt("POST", "/archives/purge", s.archivePurge)
	for _, method := range []string{"GET", "PUT"} {
		rt(method, "/archives/purge/settings", s.archiveRetention)
	}
	for _, mode := range []string{"merge-preview", "merge-count", "merge"} {
		m := "GET"
		if mode == "merge" {
			m = "POST"
		}
		rt(m, "/archives/"+mode, s.mergeArchives(mode))
	}
}
func (s *Service) deleteImpact(w http.ResponseWriter, r *http.Request) (any, error) {
	f, e := s.getAsset(r.Context(), s.db, access.From(r.Context()), "archives", pathID(r), "read")
	if e != nil {
		return nil, e
	}
	items, e := records(r.Context(), s.db, "SELECT id,printer_id,status FROM print_queue WHERE archive_id=?", f["id"])
	if e != nil {
		return nil, e
	}
	total := 0
	active := false
	for _, q := range items {
		if scope(access.From(r.Context()), num(q["printer_id"])) {
			total++
			active = active || in(val(q["status"]), "printing", "dispatching")
		}
	}
	return row{"related_queue_items": total, "currently_printing": active}, nil
}
func (s *Service) backfillArchives(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:update_all"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := access.From(ctx)
	files, e := records(ctx, s.db, "SELECT * FROM print_archives WHERE deleted_at IS NULL AND (content_hash IS NULL OR content_hash='')")
	if e != nil {
		return nil, e
	}
	updated, bad := 0, 0
	for _, f := range files {
		if !s.accessible(ctx, s.db, p, "archives", f, "update") {
			continue
		}
		full, e := s.assetPath(f["file_path"])
		if e != nil {
			bad++
			continue
		}
		src, e := os.Open(full)
		if e != nil {
			bad++
			continue
		}
		h := sha256.New()
		_, e = io.Copy(h, src)
		src.Close()
		if e != nil {
			bad++
			continue
		}
		digest := hex.EncodeToString(h.Sum(nil))
		res, e := s.db.ExecContext(ctx, "UPDATE print_archives SET content_hash=?,gcode_hash=COALESCE(gcode_hash,?) WHERE id=? AND file_path=? AND COALESCE(content_hash,'')=''", digest, plateDigest(full, val(f["filename"]), digest), f["id"], f["file_path"])
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		updated += int(n)
	}
	return row{"updated": updated, "errors": bad}, nil
}
func (s *Service) reindexArchives(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:update_all"); e != nil {
		return nil, e
	}
	if access.From(r.Context()).PrinterIDs != nil {
		return nil, fail(403, "Installation-wide archive access required")
	}
	ctx := r.Context()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec("INSERT INTO archive_fts(archive_fts) VALUES('rebuild')"); e != nil {
		return nil, e
	}
	var n int
	if e = tx.QueryRow("SELECT COUNT(*) FROM print_archives").Scan(&n); e != nil {
		return nil, e
	}
	return row{"status": "success", "indexed": n, "count": n}, tx.Commit()
}
func (s *Service) archiveSourceByName(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:update_all"); e != nil {
		return nil, e
	}
	r.Body = http.MaxBytesReader(w, r.Body, (1<<30)+(1<<20))
	if e := r.ParseMultipartForm(2 << 20); e != nil {
		return nil, fail(422, "Invalid source upload")
	}
	defer r.MultipartForm.RemoveAll()
	f, h, e := r.FormFile("file")
	if e != nil {
		return nil, fail(422, "A source file is required")
	}
	f.Close()
	name := strings.TrimSpace(r.URL.Query().Get("print_name"))
	if name == "" {
		name = strings.TrimSuffix(strings.TrimSuffix(filepath.Base(h.Filename), filepath.Ext(h.Filename)), "_source")
	}
	if name == "" {
		return nil, fail(422, "A print name is required")
	}
	ctx := r.Context()
	files, e := records(ctx, s.db, "SELECT * FROM print_archives WHERE deleted_at IS NULL AND (instr(lower(print_name),lower(?))>0 OR instr(lower(filename),lower(?))>0) ORDER BY lower(print_name)=lower(?) DESC,id DESC", name, name, name)
	if e != nil {
		return nil, e
	}
	for _, f := range files {
		if s.accessible(ctx, s.db, access.From(ctx), "archives", f, "update") {
			r.SetPathValue("id", val(f["id"]))
			return s.attachment("archives", "source")(w, r)
		}
	}
	return nil, sql.ErrNoRows
}
func (s *Service) bulkArchiveUploads(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:create"); e != nil {
		return nil, e
	}
	p := access.From(r.Context())
	printer := num(r.URL.Query().Get("printer_id"))
	if !scope(p, printer) {
		return nil, sql.ErrNoRows
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if e := r.ParseMultipartForm(8 << 20); e != nil {
		return nil, fail(422, "Invalid or oversized upload")
	}
	defer r.MultipartForm.RemoveAll()
	files := r.MultipartForm.File["files"]
	if len(files) == 0 || len(files) > 1000 {
		return nil, fail(422, "Choose files to upload")
	}
	ok, bad := []row{}, []row{}
	for i, h := range files {
		name := filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/"))
		v := row{"index": i, "filename": name}
		if !validFileName(name) || !in(strings.ToLower(filepath.Ext(name)), ".3mf", ".gcode") {
			v["error"] = "Unsupported archive file"
			bad = append(bad, v)
			continue
		}
		f, e := h.Open()
		if e != nil {
			return nil, e
		}
		id, e := s.storeAsset(r.Context(), s.db, p, f, "archives", name, 0, printer, 0)
		f.Close()
		if e != nil {
			v["error"] = "Could not import print file"
			bad = append(bad, v)
			continue
		}
		v["id"] = id
		v["archive_id"] = id
		ok = append(ok, v)
		s.emit(row{"type": "archive_created", "data": row{"id": id, "printer_id": printer}})
	}
	return row{"uploaded": len(ok), "failed": len(bad), "results": ok, "errors": bad}, nil
}
func (s *Service) archivePolicy(ctx context.Context) row {
	v, _ := access.Setting(ctx, s.db, "archive_auto_purge_days", "365")
	days, e := bounded(v, 7, 3650)
	if e != nil {
		days = 365
	}
	a, _ := access.Setting(ctx, s.db, "archive_auto_purge_enabled", "false")
	b, _ := access.Setting(ctx, s.db, "archive_auto_purge_stats", "false")
	return row{"enabled": flag(a), "days": days, "purge_stats": flag(b)}
}
func (s *Service) archiveRetention(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:purge"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	if r.Method == "GET" {
		return s.archivePolicy(ctx), nil
	}
	if access.From(ctx).PrinterIDs != nil {
		return nil, fail(403, "Installation-wide archive access required")
	}
	v, e := input(w, r)
	if e != nil {
		return nil, e
	}
	days := 365
	if v["days"] != nil {
		days, e = bounded(val(v["days"]), 7, 3650)
		if e != nil {
			return nil, e
		}
	}
	out := row{"enabled": false, "days": days, "purge_stats": false}
	for _, b := range []string{"enabled", "purge_stats"} {
		if x, ok := v[b]; ok {
			value, ok := x.(bool)
			if !ok {
				return nil, fail(422, "Invalid "+b)
			}
			out[b] = value
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	for key, val := range map[string]any{"archive_auto_purge_days": days, "archive_auto_purge_enabled": out["enabled"], "archive_auto_purge_stats": out["purge_stats"]} {
		if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, fmt.Sprint(val)); e != nil {
			return nil, e
		}
	}
	return out, tx.Commit()
}
func (s *Service) archivePurge(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:purge"); e != nil {
		return nil, e
	}
	v := row{"older_than_days": r.URL.Query().Get("older_than_days"), "purge_stats": r.URL.Query().Get("purge_stats")}
	var e error
	if r.Method == "POST" {
		v, e = input(w, r)
		if e != nil {
			return nil, e
		}
	}
	days, e := bounded(val(v["older_than_days"]), 1, 3650)
	if e != nil {
		return nil, e
	}
	stats := false
	if raw := val(v["purge_stats"]); raw != "" {
		stats, e = strconv.ParseBool(raw)
		if e != nil {
			return nil, fail(422, "Invalid purge_stats")
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	out, e := s.purgeArchives(r.Context(), tx, access.From(r.Context()), days, stats, r.Method == "POST")
	if e != nil {
		return nil, e
	}
	return out, tx.Commit()
}
func (s *Service) purgeArchives(ctx context.Context, tx *sql.Tx, p access.Principal, days int, stats, apply bool) (row, error) {
	rows, e := records(ctx, tx, `SELECT * FROM print_archives a WHERE (deleted_at IS NULL OR ?) AND julianday(COALESCE(completed_at,started_at,created_at))<julianday('now',?) AND status!='printing' AND NOT EXISTS(SELECT 1 FROM print_queue q WHERE q.archive_id=a.id AND q.status IN ('pending','printing','dispatching','awaiting_review')) AND NOT EXISTS(SELECT 1 FROM print_log_entries l WHERE l.archive_id=a.id AND l.completed_at IS NULL AND l.status IN ('printing','running','paused')) AND NOT EXISTS(SELECT 1 FROM archive_aliases x WHERE x.merged_archive_id=a.id) ORDER BY id`, stats, fmt.Sprintf("-%d days", days))
	if e != nil {
		return nil, e
	}
	samples := []string{}
	var size int64
	total := 0
	for _, f := range rows {
		if p.PrinterIDs != nil && !s.archiveFullScope(ctx, tx, p, f) {
			continue
		}
		if len(samples) < 10 {
			samples = append(samples, val(f["filename"]))
		}
		size += num(f["file_size"])
		total++
		if apply {
			if e = purgeArchiveRow(ctx, tx, num(f["id"]), stats); e != nil {
				return nil, e
			}
		}
	}
	if apply {
		return row{"deleted": total, "purge_stats": stats}, nil
	}
	return row{"count": total, "older_than_days": days, "total_bytes": size, "sample_filenames": samples}, nil
}
func (s *Service) archiveFullScope(ctx context.Context, d database, p access.Principal, f row) bool {
	if !scope(p, num(f["printer_id"])) {
		return false
	}
	runs, e := records(ctx, d, "SELECT DISTINCT printer_id FROM print_log_entries WHERE archive_id=?", f["id"])
	if e != nil {
		return false
	}
	for _, v := range runs {
		if !scope(p, num(v["printer_id"])) {
			return false
		}
	}
	return true
}
func purgeArchiveRow(ctx context.Context, tx *sql.Tx, k int64, stats bool) error {
	if _, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bb_reclaim(path) SELECT file_path FROM print_archives WHERE id=? AND file_path!=''", k); e != nil {
		return e
	}
	if stats {
		if _, e := tx.ExecContext(ctx, "DELETE FROM print_log_entries WHERE archive_id=?", k); e != nil {
			return e
		}
		_, e := tx.ExecContext(ctx, "DELETE FROM print_archives WHERE id=?", k)
		return e
	}
	_, e := tx.ExecContext(ctx, "UPDATE print_archives SET file_path='',deleted_at=CURRENT_TIMESTAMP WHERE id=?", k)
	return e
}
func (s *Service) archiveSweeper(ctx context.Context) error {
	cfg := s.archivePolicy(ctx)
	if !flag(cfg["enabled"]) {
		return nil
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var due bool
	if e = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT julianday(value)<=julianday('now','-1 day') FROM settings WHERE key='archive_auto_purge_last_run'),1)").Scan(&due); e != nil || !due {
		return e
	}
	if _, e = s.purgeArchives(ctx, tx, access.Principal{Admin: true}, int(num(cfg["days"])), flag(cfg["purge_stats"]), true); e != nil {
		return e
	}
	if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES('archive_auto_purge_last_run',CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=CURRENT_TIMESTAMP"); e != nil {
		return e
	}
	return tx.Commit()
}
