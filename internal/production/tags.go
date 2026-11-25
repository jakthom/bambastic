package production

import (
	"bambastic/internal/access"
	"database/sql"
	"io"
	"net/http"
	"os"
	"strings"
)

func readLimitedFile(path string, limit int64) ([]byte, bool, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, false, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return b[:limit], true, e
	}
	return b, false, e
}
func (s *Service) libraryTags(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	if r.Method == "GET" {
		if !p.Has("library:read_all") && !p.Has("library:read_own") {
			return nil, fail(403, "Library access required")
		}
		tags, e := records(ctx, s.db, "SELECT * FROM library_tags ORDER BY name_key")
		if e != nil {
			return nil, e
		}
		files, e := records(ctx, s.db, "SELECT f.*,t.tag_id FROM library_files f JOIN library_file_tags t ON t.file_id=f.id WHERE f.deleted_at IS NULL")
		if e != nil {
			return nil, e
		}
		counts := map[int64]int{}
		for _, f := range files {
			if s.accessible(ctx, s.db, p, "library", f, "read") {
				counts[num(f["tag_id"])]++
			}
		}
		for _, t := range tags {
			t["file_count"] = counts[num(t["id"])]
		}
		return tags, nil
	}
	if !p.Has("library:update_all") {
		return nil, fail(403, "Catalog editing requires library:update_all")
	}
	if r.Method == "DELETE" {
		res, e := s.db.ExecContext(ctx, "DELETE FROM library_tags WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return nil, sql.ErrNoRows
		}
		w.WriteHeader(204)
		return nil, nil
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	name := strings.TrimSpace(val(m["name"]))
	if name == "" || len(name) > 64 {
		return nil, fail(422, "Invalid tag name")
	}
	if r.Method == "POST" {
		k, e = insert(ctx, s.db, "library_tags", row{"name": name, "name_key": strings.ToLower(name)})
	} else {
		_, e = s.db.ExecContext(ctx, "UPDATE library_tags SET name=?,name_key=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", name, strings.ToLower(name), k)
	}
	if e != nil {
		return nil, fail(409, "Tag name already exists")
	}
	v, e := one(ctx, s.db, "SELECT t.*,count(x.file_id) file_count FROM library_tags t LEFT JOIN library_file_tags x ON x.tag_id=t.id WHERE t.id=? GROUP BY t.id", k)
	return v, e
}
func (s *Service) tagAssign(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("library:update_all") && !p.Has("library:update_own") {
		return nil, fail(403, "Library update required")
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	files, e := idsOf(m["file_ids"])
	if e != nil {
		return nil, e
	}
	tags, e := idsOf(m["tag_ids"])
	if e != nil {
		return nil, e
	}
	if len(files) == 0 {
		return nil, fail(422, "Select at least one file")
	}
	action := val(m["action"])
	if action == "" {
		action = "add"
	}
	if action != "replace" && action != "remove" && action != "add" {
		return nil, fail(422, "Invalid tag action")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	for _, t := range tags {
		if _, e = one(ctx, tx, "SELECT id FROM library_tags WHERE id=?", t); e != nil {
			return nil, e
		}
	}
	var added, removed, changed int64
	for _, f := range files {
		if _, e = s.getAsset(ctx, tx, p, "library", f, "update"); e != nil {
			return nil, e
		}
		changed++
		if action == "replace" {
			res, e := tx.ExecContext(ctx, "DELETE FROM library_file_tags WHERE file_id=?", f)
			if e != nil {
				return nil, e
			}
			n, _ := res.RowsAffected()
			removed += n
		}
		for _, t := range tags {
			var res sql.Result
			if action == "remove" {
				res, e = tx.ExecContext(ctx, "DELETE FROM library_file_tags WHERE file_id=? AND tag_id=?", f, t)
			} else {
				res, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO library_file_tags(file_id,tag_id) VALUES(?,?)", f, t)
			}
			if e != nil {
				return nil, e
			}
			n, _ := res.RowsAffected()
			if action == "remove" {
				removed += n
			} else {
				added += n
			}
		}
	}
	return row{"files_updated": changed, "associations_added": added, "associations_removed": removed}, tx.Commit()
}
