package production

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

func (s *Service) libraryAdminRoutes(rt register) {
	for _, method := range []string{"GET", "PUT"} {
		rt(method, "/library/trash/settings", s.trashSettings)
	}
	rt("GET", "/library/purge/preview", s.libraryPurge)
	rt("POST", "/library/purge", s.libraryPurge)
	rt("DELETE", "/library/trash/{id}", s.emptyTrash)
	rt("DELETE", "/library/trash", s.emptyTrash)
	rt("POST", "/library/files/{id}/print", func(_ http.ResponseWriter, r *http.Request) (any, error) {
		if e := permitted(r, "queue:create"); e != nil {
			return nil, e
		}
		return nil, fail(410, "Create a queue item with POST /api/v1/queue/")
	})
}

type trashPolicy struct {
	Retention int  `json:"retention_days"`
	Auto      bool `json:"auto_purge_enabled"`
	Days      int  `json:"auto_purge_days"`
	Never     bool `json:"auto_purge_include_never_printed"`
}

func (s *Service) trashPolicy(ctx context.Context) trashPolicy {
	get := func(k, d string) string { v, _ := access.Setting(ctx, s.db, k, d); return v }
	ret, _ := strconv.Atoi(get("library_trash_retention_days", "30"))
	days, _ := strconv.Atoi(get("library_auto_purge_days", "90"))
	if ret < 1 || ret > 365 {
		ret = 30
	}
	if days < 7 || days > 3650 {
		days = 90
	}
	return trashPolicy{ret, flag(get("library_auto_purge_enabled", "false")), days, flag(get("library_auto_purge_include_never_printed", "true"))}
}
func (s *Service) trashSettings(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:purge"); e != nil {
		return nil, e
	}
	p := s.trashPolicy(r.Context())
	if r.Method == "GET" {
		return p, nil
	}
	v, e := input(w, r)
	if e != nil {
		return nil, e
	}
	if p.Retention, e = bounded(val(v["retention_days"]), 1, 365); e != nil {
		return nil, e
	}
	if y, ok := v["auto_purge_days"]; ok {
		if p.Days, e = bounded(val(y), 7, 3650); e != nil {
			return nil, e
		}
	}
	for k, b := range map[string]*bool{"auto_purge_enabled": &p.Auto, "auto_purge_include_never_printed": &p.Never} {
		if val, ok := v[k]; ok {
			n, valid := val.(bool)
			if !valid {
				return nil, fail(422, "Invalid "+k)
			}
			*b = n
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	for key, val := range map[string]any{"library_trash_retention_days": p.Retention, "library_auto_purge_enabled": p.Auto, "library_auto_purge_days": p.Days, "library_auto_purge_include_never_printed": p.Never} {
		if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, fmt.Sprint(val)); e != nil {
			return nil, e
		}
	}
	return p, tx.Commit()
}

const eligibleFiles = ` deleted_at IS NULL AND NOT is_external AND
	(last_printed_at IS NOT NULL OR ?) AND julianday(COALESCE(last_printed_at,created_at)) < julianday('now',?)
	AND NOT EXISTS(SELECT 1 FROM print_queue q WHERE q.library_file_id=library_files.id AND q.status IN ('pending','dispatching','printing','awaiting_review'))`

func (s *Service) libraryPurge(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:purge"); e != nil {
		return nil, e
	}
	v := row{"older_than_days": r.URL.Query().Get("older_than_days"), "include_never_printed": r.URL.Query().Get("include_never_printed")}
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
	include := true
	if raw := val(v["include_never_printed"]); raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fail(422, "Invalid include_never_printed")
		}
		include = b
	}
	age := fmt.Sprintf("-%d days", days)
	if r.Method == "POST" {
		res, e := s.db.ExecContext(r.Context(), "UPDATE library_files SET deleted_at=CURRENT_TIMESTAMP WHERE "+eligibleFiles, include, age)
		if e != nil {
			return nil, e
		}
		count, e := res.RowsAffected()
		return row{"moved_to_trash": count}, e
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	summary, e := one(r.Context(), tx, "SELECT COUNT(*) count,COALESCE(SUM(file_size),0) total_bytes FROM library_files WHERE "+eligibleFiles, include, age)
	if e != nil {
		return nil, e
	}
	samples, e := records(r.Context(), tx, "SELECT filename FROM library_files WHERE "+eligibleFiles+" ORDER BY id LIMIT 10", include, age)
	if e != nil {
		return nil, e
	}
	names := []string{}
	for _, v := range samples {
		names = append(names, val(v["filename"]))
	}
	summary["sample_filenames"] = names
	summary["older_than_days"] = days
	summary["include_never_printed"] = include
	return summary, nil
}

func removeTrashed(ctx context.Context, tx *sql.Tx, f row) error {
	k := num(f["id"])
	// The same immutable blob may back an archive, another user's file or an old version.
	// Reclaim bytes at quiescent startup, after checking all live references.
	if _, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bb_reclaim(path) SELECT file_path FROM library_files WHERE id=? UNION SELECT file_path FROM bb_versions WHERE file_id=? UNION SELECT path FROM bb_photos WHERE kind='library' AND owner_id=?`, k, k, k); e != nil {
		return e
	}
	if _, e := tx.ExecContext(ctx, "DELETE FROM bb_photos WHERE kind='library' AND owner_id=?", k); e != nil {
		return e
	}
	_, e := tx.ExecContext(ctx, "DELETE FROM library_files WHERE id=? AND deleted_at IS NOT NULL AND NOT is_external", k)
	return e
}
func (s *Service) emptyTrash(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !p.Has("library:delete_all") && !p.Has("library:delete_own") {
		return nil, fail(403, "Trash access required")
	}
	q := "SELECT * FROM library_files WHERE deleted_at IS NOT NULL AND NOT is_external"
	args := []any{}
	oneFile := r.PathValue("id") != ""
	if oneFile {
		q += " AND id=?"
		args = append(args, pathID(r))
	}
	ctx := r.Context()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	files, e := records(ctx, tx, q, args...)
	if e != nil {
		return nil, e
	}
	count := 0
	for _, f := range files {
		if owned(p, "library", "delete", num(f["created_by_id"])) {
			if e := removeTrashed(ctx, tx, f); e != nil {
				return nil, e
			}
			count++
		}
	}
	if oneFile && count != 1 {
		return nil, sql.ErrNoRows
	}
	return row{"status": "success", "deleted": count}, tx.Commit()
}
func (s *Service) reclaimLibrary(ctx context.Context) error {
	rs, e := records(ctx, s.db, `SELECT path FROM bb_reclaim WHERE path NOT IN (SELECT thumbnail_path FROM library_files WHERE thumbnail_path IS NOT NULL UNION SELECT file_path FROM library_files UNION SELECT file_path FROM print_archives UNION SELECT file_path FROM bb_versions UNION SELECT file_path FROM pending_uploads UNION SELECT path FROM bb_photos UNION SELECT source_3mf_path FROM print_archives WHERE source_3mf_path IS NOT NULL UNION SELECT f3d_path FROM print_archives WHERE f3d_path IS NOT NULL)`)
	if e != nil {
		return e
	}
	for _, v := range rs {
		rel := val(v["path"])
		clean := filepath.ToSlash(filepath.Clean(rel))
		if !filepath.IsLocal(rel) || !strings.HasPrefix(clean, "production/objects/") {
			continue
		}
		e = os.Remove(filepath.Join(s.dir, rel))
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if _, e = s.db.ExecContext(ctx, "DELETE FROM bb_reclaim WHERE path=?", rel); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) sweepLibrary(ctx context.Context) error {
	p := s.trashPolicy(ctx)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if p.Auto {
		var due bool
		e = tx.QueryRowContext(ctx, "SELECT COALESCE((SELECT julianday(value)<=julianday('now','-1 day') FROM settings WHERE key='library_auto_purge_last_run'),1)").Scan(&due)
		if e != nil {
			return e
		}
		if due {
			if _, e = tx.ExecContext(ctx, "UPDATE library_files SET deleted_at=CURRENT_TIMESTAMP WHERE "+eligibleFiles, p.Never, fmt.Sprintf("-%d days", p.Days)); e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('library_auto_purge_last_run',CURRENT_TIMESTAMP) ON CONFLICT(key) DO UPDATE SET value=CURRENT_TIMESTAMP"); e != nil {
				return e
			}
		}
	}
	files, e := records(ctx, tx, "SELECT id FROM library_files WHERE NOT is_external AND deleted_at IS NOT NULL AND julianday(deleted_at)<julianday('now',?)", fmt.Sprintf("-%d days", p.Retention))
	if e != nil {
		return e
	}
	for _, f := range files {
		if e = removeTrashed(ctx, tx, f); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Service) librarySweepLoop(ctx context.Context) {
	t := time.NewTicker(15 * time.Minute)
	defer t.Stop()
	for {
		if e := s.archiveSweeper(ctx); e != nil && ctx.Err() == nil {
			log.Printf("archive retention: %v", e)
		}
		if e := s.sweepLibrary(ctx); e != nil && ctx.Err() == nil {
			log.Printf("library retention: %v", e)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
