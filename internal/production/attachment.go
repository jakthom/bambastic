package production

import (
	"archive/zip"
	"database/sql"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"bambastic/internal/access"
)

// Each upload has a private immutable path. Existing readers can finish while a
// replacement is being validated. Failed transactions remove the new upload.
func (s *Service) attachment(kind, field string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		k := pathID(r)
		col := map[string]string{"source": "source_3mf_path", "f3d": "f3d_path", "photos": "photos"}[field]
		perm := "read"
		if r.Method != "GET" {
			perm = "update"
		}
		v, e := s.getAsset(ctx, s.db, p, kind, k, perm)
		if e != nil {
			return nil, e
		}
		if r.Method == "GET" {
			path := val(v[col])
			name := r.PathValue("filename")
			if field == "photos" {
				refs, e := one(ctx, s.db, "SELECT path FROM bb_photos WHERE kind=? AND owner_id=? AND filename=?", kind, k, name)
				if e != nil {
					return nil, e
				}
				if kind == "archives" && p.PrinterIDs != nil && !s.accessible(ctx, s.db, p, kind, v, "reprint") {
					return nil, sql.ErrNoRows
				}
				path = val(refs["path"])
			}
			if path == "" {
				return nil, sql.ErrNoRows
			}
			path, e = s.assetPath(path)
			if e != nil {
				return nil, e
			}
			if field != "photos" {
				name = filepath.Base(path)
			}
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.ServeFile(w, r, path)
			return nil, nil
		}
		var rel, name string
		var keep bool
		var tmpDir string
		if r.Method == "POST" {
			limit := int64(1 << 30)
			if field == "photos" {
				limit = 20 << 20
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit+(1<<20))
			if e = r.ParseMultipartForm(2 << 20); e != nil {
				return nil, fail(422, "Invalid or oversized multipart upload")
			}
			defer r.MultipartForm.RemoveAll()
			f, h, e := r.FormFile("file")
			if e != nil {
				return nil, fail(422, "File required")
			}
			defer f.Close()
			name = filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/"))
			ext := strings.ToLower(filepath.Ext(name))
			if !validFileName(name) || field == "source" && ext != ".3mf" || field == "f3d" && ext != ".f3d" {
				return nil, fail(422, "Invalid attachment name or type")
			}
			if field == "photos" {
				exts := map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".webp": "image/webp"}
				head := make([]byte, 512)
				n, _ := io.ReadFull(f, head)
				if exts[ext] == "" || http.DetectContentType(head[:n]) != exts[ext] {
					return nil, fail(422, "Invalid photo format")
				}
				if _, e = f.Seek(0, 0); e != nil {
					return nil, e
				}
				name = uuid() + ext
			}
			rel = filepath.Join("production", "attachments", kind, fmt.Sprint(k), uuid(), name)
			full := filepath.Join(s.dir, rel)
			tmpDir = filepath.Dir(full)
			if e = os.MkdirAll(tmpDir, 0700); e != nil {
				return nil, e
			}
			defer func() {
				if !keep {
					os.RemoveAll(tmpDir)
				}
			}()
			fd, e := os.OpenFile(full, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
			if e != nil {
				return nil, e
			}
			n, e := io.Copy(fd, io.LimitReader(f, limit+1))
			ce := fd.Close()
			if e != nil {
				return nil, e
			}
			if ce != nil {
				return nil, ce
			}
			if n == 0 || n > limit {
				return nil, fail(422, "Invalid attachment size")
			}
			if field != "photos" {
				z, e := zip.OpenReader(full)
				if e != nil {
					return nil, fail(422, "Invalid design container")
				}
				if len(z.File) == 0 {
					z.Close()
					return nil, fail(422, "Empty design container")
				}
				z.Close()
			}
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		v, e = s.getAsset(ctx, tx, p, kind, k, "update")
		if e != nil {
			return nil, e
		}
		old := ""
		out := row{"status": "uploaded", "filename": name}
		if field == "photos" {
			names, _ := unpack(v["photos"]).([]any)
			if names == nil {
				names = []any{}
			}
			if r.Method == "DELETE" {
				name = r.PathValue("filename")
				entry, e := one(ctx, tx, "SELECT path FROM bb_photos WHERE kind=? AND owner_id=? AND filename=?", kind, k, name)
				if e != nil {
					return nil, e
				}
				old = val(entry["path"])
				if _, e = tx.ExecContext(ctx, "DELETE FROM bb_photos WHERE kind=? AND owner_id=? AND filename=?", kind, k, name); e != nil {
					return nil, e
				}
				remaining := []any{}
				for _, n := range names {
					if val(n) != name {
						remaining = append(remaining, n)
					}
				}
				names = remaining
			} else {
				if _, e = tx.ExecContext(ctx, "INSERT INTO bb_photos VALUES(?,?,?,?)", kind, k, name, rel); e != nil {
					return nil, e
				}
				names = append(names, name)
			}
			if _, e = tx.ExecContext(ctx, "UPDATE "+sourceTable(kind)+" SET photos=? WHERE id=?", jstr(names), k); e != nil {
				return nil, e
			}
			out["photos"] = names
		} else {
			old = val(v[col])
			if old == "" && r.Method == "DELETE" {
				return nil, sql.ErrNoRows
			}
			if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET "+col+"=? WHERE id=?", nullableString(rel), k); e != nil {
				return nil, e
			}
			out[col] = rel
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		keep = true
		if old != "" && strings.HasPrefix(old, filepath.Join("production", "attachments")+string(filepath.Separator)) {
			if path, e := s.assetPath(old); e == nil {
				os.Remove(path)
				os.Remove(filepath.Dir(path))
			}
		}
		if r.Method == "DELETE" {
			out["status"] = "deleted"
		}
		return out, nil
	}
}
func nullableString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func (s *Service) revisionRoutes(rt register) {
	rt("GET", "/library/files/{id}/versions", s.revisions)
	rt("POST", "/library/files/{id}/versions", s.revisions)
	rt("POST", "/library/files/{id}/versions/{version}/restore", s.revisions)
}
func (s *Service) revisions(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	action := "read"
	if r.Method == "POST" {
		action = "update"
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	cur, e := s.getAsset(ctx, tx, p, "library", k, action)
	if e != nil {
		return nil, e
	}
	if r.Method == "GET" {
		return records(ctx, tx, "SELECT id,filename,file_size,file_hash,created_at,created_by_id FROM bb_versions WHERE file_id=? ORDER BY id DESC", k)
	}
	if flag(cur["is_external"]) {
		return nil, fail(409, "Versions require a managed file")
	}
	busy, e := one(ctx, tx, "SELECT count(*) n FROM print_queue WHERE library_file_id=? AND status IN ('pending','dispatching','printing')", k)
	if e != nil {
		return nil, e
	}
	if num(busy["n"]) > 0 {
		return nil, fail(409, "File has active or pending work")
	}
	var next row
	fields := []string{"filename", "file_path", "file_type", "file_size", "file_hash", "file_metadata"}
	if version := num(r.PathValue("version")); version != 0 {
		next, e = one(ctx, tx, "SELECT * FROM bb_versions WHERE file_id=? AND id=?", k, version)
		if e != nil {
			return nil, e
		}
	} else {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
		if e = r.ParseMultipartForm(2 << 20); e != nil {
			return nil, fail(422, "Invalid multipart upload")
		}
		defer r.MultipartForm.RemoveAll()
		f, h, e := r.FormFile("file")
		if e != nil {
			return nil, fail(422, "A file is required")
		}
		defer f.Close()
		if !validFileName(h.Filename) || !strings.EqualFold(filepath.Ext(h.Filename), filepath.Ext(val(cur["filename"]))) {
			return nil, fail(422, "Version must use the same file type")
		}
		next, e = s.prepareAsset(f, "library", h.Filename)
		if e != nil {
			return nil, e
		}
	}
	revision := row{"file_id": k, "created_by_id": id(p.ID)}
	for _, field := range fields {
		revision[field] = cur[field]
	}
	if _, e = insert(ctx, tx, "bb_versions", revision); e != nil {
		return nil, e
	}
	if e = update(ctx, tx, "library_files", k, next, fields); e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE library_files SET updated_at=CURRENT_TIMESTAMP WHERE id=?", k); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	next, e = one(ctx, s.db, "SELECT * FROM library_files WHERE id=?", k)
	if e == nil {
		decodeAsset(next)
	}
	return next, e
}
