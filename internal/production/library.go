package production

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"path/filepath"
	"strings"

	"bambastic/internal/access"
)

func (s *Service) libraryRoutes(rt register) {
	for _, a := range []string{"move", "copy", "add-to-queue"} {
		rt("POST", "/library/files/"+a, s.bulkFiles(a))
	}
	rt("POST", "/library/bulk-delete", s.bulkFiles("delete"))
	rt("PUT", "/library/files/{id}", s.assetEdit("library"))
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		rt(m, "/library/folders/{id}", s.folderDetail)
	}
	rt("GET", "/library/folders/{id}/{sub}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		switch r.PathValue("id") {
		case "by-project", "by-archive":
			field := strings.TrimPrefix(r.PathValue("id"), "by-")
			r.SetPathValue("id", r.PathValue("sub"))
			return s.linkedFolders(field)(w, r)
		}
		if r.PathValue("sub") == "readme" {
			return s.folderReadme(w, r)
		}
		return nil, sql.ErrNoRows
	})
	rt("GET", "/library/trash", s.trash)
	rt("POST", "/library/trash/{id}/restore", s.trash)
	rt("GET", "/library/stats", s.libraryStats)
	for _, p := range []string{"/library/tags", "/library/tags/{$}"} {
		rt("GET", p, s.libraryTags)
		rt("POST", p, s.libraryTags)
	}
	rt("PATCH", "/library/tags/{id}", s.libraryTags)
	rt("DELETE", "/library/tags/{id}", s.libraryTags)
	rt("POST", "/library/tags/bulk-assign", s.tagAssign)
}

func idsOf(v any) ([]int64, error) {
	a, ok := v.([]any)
	if v == nil {
		return []int64{}, nil
	}
	if !ok || len(a) > 1000 {
		return nil, fail(422, "Expected up to 1000 identifiers")
	}
	ids := []int64{}
	seen := map[int64]bool{}
	for _, item := range a {
		k := num(item)
		if k <= 0 {
			return nil, fail(422, "Invalid identifier")
		}
		if !seen[k] {
			ids = append(ids, k)
			seen[k] = true
		}
	}
	return ids, nil
}
func folderWrite(p access.Principal, f row) bool {
	return !flag(f["external_readonly"]) && (owned(p, "library", "update", num(f["created_by_id"])) || p.Has("library:upload") && (num(f["created_by_id"]) == p.ID || flag(f["shared"])))
}
func (s *Service) targetFolder(ctx context.Context, d database, p access.Principal, k int64) error {
	return s.checkLibraryDestination(ctx, d, p, k, false)
}
func (s *Service) checkLibraryDestination(ctx context.Context, d database, p access.Principal, k int64, external bool) error {
	if k == 0 {
		return nil
	}
	v, e := one(ctx, d, "SELECT * FROM library_folders WHERE id=?", k)
	if e != nil {
		return e
	}
	if !folderWrite(p, v) {
		return fail(403, "Destination folder is not writable")
	}
	if flag(v["is_external"]) && !external {
		return fail(409, "A managed destination is required")
	}
	return nil
}
func (s *Service) bulkFiles(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		perm := "update"
		switch action {
		case "delete":
			perm = "delete"
		case "add-to-queue", "copy":
			perm = "read"
		}
		if !p.Has("library:"+perm+"_own") && !p.Has("library:"+perm+"_all") {
			return nil, fail(403, "Library permission required")
		}
		if action == "add-to-queue" && !p.Has("queue:create") || action == "copy" && !p.Has("library:upload") {
			return nil, fail(403, "Permission denied")
		}
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		files, e := idsOf(m["file_ids"])
		if e != nil {
			return nil, e
		}
		dirs, e := idsOf(m["folder_ids"])
		if e != nil {
			return nil, e
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		var edits externalEdits
		defer edits.finish(s)
		defer tx.Rollback()
		dest := num(m["folder_id"])
		var target row
		if action == "move" || action == "copy" {
			if e = s.checkLibraryDestination(ctx, tx, p, dest, true); e != nil {
				return nil, e
			}
			target, e = s.externalFolder(ctx, tx, dest)
			if e != nil {
				return nil, e
			}
		}
		failed := []row{}
		added := []row{}
		changed := 0
		for _, k := range files {
			f, e := s.getAsset(ctx, tx, p, "library", k, perm)
			if e == nil {
				switch action {
				case "move", "copy":
					if action == "move" && num(f["folder_id"]) == dest {
						break
					}
					var source row
					if flag(f["is_external"]) {
						source, e = s.externalFolder(ctx, tx, num(f["folder_id"]))
						if e != nil {
							break
						}
						if source == nil {
							e = fail(409, "External source folder missing")
							break
						}
					}
					if e = edits.transfer(ctx, s, tx, f, source, target, val(f["filename"]), action == "move"); e != nil {
						break
					}
					if action == "move" {
						_, e = tx.ExecContext(ctx, "UPDATE library_files SET folder_id=?,is_external=?,fs_modified_at=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", id(dest), target != nil, f["fs_modified_at"], k)
						break
					}
					c := row{"folder_id": id(dest), "created_by_id": id(p.ID), "print_count": 0, "is_external": target != nil, "fs_modified_at": f["fs_modified_at"]}
					for _, field := range []string{"filename", "file_path", "file_type", "file_size", "file_hash", "file_metadata", "notes", "external_url", "thumbnail_path"} {
						c[field] = f[field]
					}
					var newID int64
					newID, e = insert(ctx, tx, "library_files", c)
					if e == nil {
						_, e = tx.ExecContext(ctx, "INSERT INTO library_file_tags(file_id,tag_id) SELECT ?,tag_id FROM library_file_tags WHERE file_id=?", newID, k)
					}
				case "delete":
					e = s.trashManaged(ctx, tx, p, k)
				case "add-to-queue":
					options := row{"library_file_id": k, "printer_id": m["printer_id"], "target_model": m["target_model"], "manual_start": true, "cost_center_id": m["cost_center_id"]}
					if num(options["printer_id"]) == 0 && val(options["target_model"]) == "" {
						var info plateInfo
						info, e = s.meta("library", f)
						if e == nil {
							options["target_model"] = info.Model
						}
					}
					if e == nil {
						var ids []int64
						ids, e = s.createCopies(ctx, tx, p, options)
						if e == nil {
							added = append(added, row{"file_id": k, "filename": f["filename"], "queue_item_id": ids[0]})
						}
					}
				}
			}
			if e != nil {
				if !errors.Is(e, sql.ErrNoRows) {
					var problem failure
					if !errors.As(e, &problem) {
						return nil, e
					}
				}
				failed = append(failed, row{"file_id": k, "filename": f["filename"], "error": e.Error(), "reason": e.Error(), "code": transferCode(e)})
				continue
			}
			changed++
		}
		deletedDirs := 0
		if action == "delete" {
			for _, k := range dirs {
				if e = s.removeFolder(ctx, tx, p, k); e != nil {
					return nil, e
				}
				deletedDirs++
			}
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		switch action {
		case "add-to-queue":
			s.kick()
			return row{"added": added, "errors": failed}, nil
		case "delete":
			return row{"deleted_files": changed, "deleted_folders": deletedDirs, "skipped": len(failed)}, nil
		}
		return row{"moved": changed, "copied": changed, "skipped": len(failed), "skipped_details": failed, "skipped_reasons": failed, "status": "success"}, nil
	}
}
func (s *Service) trashManaged(ctx context.Context, tx *sql.Tx, p access.Principal, k int64) error {
	f, e := s.getAsset(ctx, tx, p, "library", k, "delete")
	if e != nil {
		return e
	}
	busy, e := one(ctx, tx, "SELECT count(*) n FROM print_queue WHERE library_file_id=? AND status IN ('dispatching','pending','printing')", k)
	if e != nil {
		return e
	}
	if num(busy["n"]) > 0 {
		return fail(409, "File is in use by the queue")
	}
	if flag(f["is_external"]) {
		return s.forgetExternal(ctx, tx, k)
	}
	_, e = tx.ExecContext(ctx, "UPDATE library_files SET deleted_at=CURRENT_TIMESTAMP WHERE id=?", k)
	return e
}
func (s *Service) removeFolder(ctx context.Context, tx *sql.Tx, p access.Principal, k int64) error {
	ds, e := records(ctx, tx, "WITH RECURSIVE tree(id) AS (SELECT id FROM library_folders WHERE id=? UNION SELECT f.id FROM library_folders f JOIN tree ON f.parent_id=tree.id) SELECT * FROM library_folders WHERE id IN tree", k)
	if e != nil {
		return e
	}
	if len(ds) == 0 {
		return sql.ErrNoRows
	}
	for _, d := range ds {
		if !owned(p, "library", "delete", num(d["created_by_id"])) {
			return fail(403, "Cannot delete this folder")
		}
		fs, e := records(ctx, tx, "SELECT id FROM library_files WHERE folder_id=? AND deleted_at IS NULL", d["id"])
		if e != nil {
			return e
		}
		for _, f := range fs {
			if e = s.trashManaged(ctx, tx, p, num(f["id"])); e != nil {
				return e
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE library_files SET folder_id=NULL WHERE folder_id=?", d["id"]); e != nil {
			return e
		}
	}
	_, e = tx.ExecContext(ctx, "DELETE FROM library_folders WHERE id=?", k)
	return e
}
func (s *Service) folderDetail(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	f, e := one(ctx, tx, "SELECT * FROM library_folders WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	if !folderAccess(p, f) {
		return nil, sql.ErrNoRows
	}
	if r.Method == "GET" {
		return f, nil
	}
	if r.Method == "DELETE" {
		if e = s.removeFolder(ctx, tx, p, k); e != nil {
			return nil, e
		}
		return row{"deleted": true}, tx.Commit()
	}
	if !owned(p, "library", "update", num(f["created_by_id"])) {
		return nil, fail(403, "Cannot edit this folder")
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	if _, ok := m["shared"]; ok && !p.Has("library:update_all") {
		return nil, fail(403, "Cannot change shared folders")
	}
	if n, ok := m["name"]; ok && (strings.TrimSpace(val(n)) == "" || len(val(n)) > 255) {
		return nil, fail(422, "Invalid folder name")
	}
	if x, ok := m["parent_id"]; ok {
		dest := num(x)
		if dest != num(f["parent_id"]) {
			if flag(f["is_external"]) {
				return nil, fail(400, "External folders cannot be moved")
			}
			if e = s.targetFolder(ctx, tx, p, dest); e != nil {
				return nil, e
			}
		}
		count, e := one(ctx, tx, "WITH RECURSIVE ancestors(id) AS (SELECT ? UNION SELECT f.parent_id FROM library_folders f JOIN ancestors ON f.id=ancestors.id WHERE f.parent_id IS NOT NULL) SELECT count(*) n FROM ancestors WHERE id=?", dest, k)
		if e != nil {
			return nil, e
		}
		if num(count["n"]) != 0 {
			return nil, fail(409, "Folder cannot contain its ancestor")
		}
	}
	for _, field := range []string{"project_id", "parent_id", "archive_id"} {
		if x, ok := m[field]; ok {
			m[field] = id(num(x))
		}
	}
	if aid := num(m["archive_id"]); aid != 0 {
		if _, e = s.getAsset(ctx, tx, p, "archives", aid, "update"); e != nil {
			return nil, e
		}
	}
	if e = update(ctx, tx, "library_folders", k, m, []string{"name", "shared", "parent_id", "project_id", "archive_id"}); e != nil {
		return nil, e
	}
	f, e = one(ctx, tx, "SELECT * FROM library_folders WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	return f, tx.Commit()
}
func (s *Service) linkedFolders(field string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		p := access.From(r.Context())
		if !p.Has("library:read_all") && !p.Has("library:read_own") {
			return nil, fail(403, "Library access required")
		}
		all, e := records(r.Context(), s.db, "SELECT * FROM library_folders WHERE "+field+"_id=? ORDER BY name", pathID(r))
		if e != nil {
			return nil, e
		}
		out := []row{}
		for _, f := range all {
			if folderAccess(p, f) {
				out = append(out, f)
			}
		}
		return out, nil
	}
}
func (s *Service) folderReadme(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	f, e := one(ctx, s.db, "SELECT * FROM library_folders WHERE id=?", pathID(r))
	if e != nil || !folderAccess(p, f) {
		return nil, sql.ErrNoRows
	}
	rs, e := records(ctx, s.db, "SELECT * FROM library_files WHERE folder_id=? AND deleted_at IS NULL AND lower(filename) IN ('readme.md','readme.txt','readme') ORDER BY id", pathID(r))
	if e != nil {
		return nil, e
	}
	for _, v := range rs {
		if s.accessible(ctx, s.db, p, "library", v, "read") {
			path, e := s.assetPath(v["file_path"])
			if e != nil {
				return nil, e
			}
			b, cut, e := readLimitedFile(path, 64<<10)
			if e != nil {
				return nil, e
			}
			return row{"filename": v["filename"], "content": string(b), "truncated": cut}, nil
		}
	}
	return nil, sql.ErrNoRows
}
func (s *Service) trash(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if r.Method == "POST" {
		f, e := one(ctx, s.db, "SELECT * FROM library_files WHERE id=? AND deleted_at IS NOT NULL", pathID(r))
		if e != nil {
			return nil, e
		}
		if !s.accessible(ctx, s.db, p, "library", f, "delete") {
			return nil, sql.ErrNoRows
		}
		_, e = s.db.ExecContext(ctx, "UPDATE library_files SET deleted_at=NULL WHERE id=?", pathID(r))
		return row{"restored": true}, e
	}
	if !p.Has("library:delete_all") && !p.Has("library:delete_own") {
		return nil, fail(403, "Trash access required")
	}
	retention := s.trashPolicy(ctx).Retention
	rs, e := records(ctx, s.db, "SELECT f.*,l.name folder_name,datetime(f.deleted_at,'+'||?||' days') auto_purge_at FROM library_files f LEFT JOIN library_folders l ON l.id=f.folder_id WHERE f.deleted_at IS NOT NULL ORDER BY f.deleted_at DESC", retention)
	if e != nil {
		return nil, e
	}
	out := []row{}
	for _, v := range rs {
		if s.accessible(ctx, s.db, p, "library", v, "delete") {
			delete(v, "file_path")
			out = append(out, v)
		}
	}
	total := len(out)
	start := min(total, max(0, int(num(r.URL.Query().Get("offset")))))
	limit := int(num(r.URL.Query().Get("limit")))
	if limit < 1 {
		limit = 100
	}
	out = out[start:min(total, start+min(500, limit))]
	return row{"items": out, "total": total, "retention_days": retention}, nil
}
func (s *Service) libraryStats(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !p.Has("library:read_all") && !p.Has("library:read_own") {
		return nil, fail(403, "Library access required")
	}
	rs, e := records(r.Context(), s.db, "SELECT * FROM library_files WHERE deleted_at IS NULL")
	if e != nil {
		return nil, e
	}
	byType := map[string]int64{}
	var count, size, prints, folders int64
	for _, v := range rs {
		if s.accessible(r.Context(), s.db, p, "library", v, "read") {
			count++
			size += num(v["file_size"])
			prints += num(v["print_count"])
			byType[val(v["file_type"])]++
		}
	}
	rs, e = records(r.Context(), s.db, "SELECT * FROM library_folders")
	if e != nil {
		return nil, e
	}
	for _, v := range rs {
		if folderAccess(p, v) {
			folders++
		}
	}
	return row{"total_files": count, "total_folders": folders, "total_size_bytes": size, "total_size": size, "files_by_type": byType, "total_prints": prints}, nil
}
func validFileName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 255 && filepath.Base(name) == name && !strings.ContainsAny(name, "\x00\r\n\\/")
}
