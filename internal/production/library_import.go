package production

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

func (s *Service) libraryImportRoutes(rt register) {
	rt("POST", "/library/files/extract-zip", s.extractLibraryZip)
	rt("POST", "/library/folders/external", s.mountLibrary)
	rt("POST", "/library/folders/{id}/scan", s.scanLibrary)
}
func libraryDir(ctx context.Context, tx *sql.Tx, p access.Principal, parent int64, name, external string, hidden bool) (int64, error) {
	q := "SELECT id FROM library_folders WHERE parent_id IS ? AND name=? AND is_external=? AND external_path IS ? AND created_by_id IS ?"
	var ext any
	if external != "" {
		ext = external
	}
	var k int64
	args := []any{id(parent), name, external != "", ext, id(p.ID)}
	if external != "" {
		q = "SELECT id FROM library_folders WHERE parent_id IS ? AND is_external AND external_path=?"
		args = []any{id(parent), external}
	}
	e := tx.QueryRowContext(ctx, q, args...).Scan(&k)
	if e == nil {
		return k, nil
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return 0, e
	}
	return insert(ctx, tx, "library_folders", row{"name": name, "parent_id": id(parent), "is_external": external != "", "external_readonly": external != "", "external_show_hidden": hidden, "external_path": ext, "shared": external != "", "created_by_id": id(p.ID)})
}
func queryBool(r *http.Request, k string, def bool) (bool, error) {
	if v := r.URL.Query().Get(k); v != "" {
		n, e := strconv.ParseBool(v)
		if e != nil {
			return false, fail(422, "Invalid "+k)
		}
		return n, nil
	}
	return def, nil
}
func (s *Service) extractLibraryZip(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:upload"); e != nil {
		return nil, e
	}
	structure, e := queryBool(r, "preserve_structure", true)
	if e != nil {
		return nil, e
	}
	wrap, e := queryBool(r, "create_folder_from_zip", false)
	if e != nil {
		return nil, e
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if e = r.ParseMultipartForm(8 << 20); e != nil {
		return nil, fail(400, "Invalid ZIP upload")
	}
	defer r.MultipartForm.RemoveAll()
	src, h, e := r.FormFile("file")
	if e != nil {
		return nil, fail(422, "A ZIP file is required")
	}
	defer src.Close()
	z, e := zip.NewReader(src, h.Size)
	if e != nil || len(z.File) > 10000 {
		return nil, fail(422, "Invalid or oversized ZIP")
	}
	var total uint64
	for _, entry := range z.File {
		n := strings.TrimSuffix(entry.Name, "/")
		if !fs.ValidPath(n) || strings.ContainsAny(n, "\\:\x00") || len(n) > 2048 || entry.Mode()&os.ModeSymlink != 0 || entry.UncompressedSize64 > 1<<30 {
			return nil, fail(422, "Unsafe ZIP entry")
		}
		total += entry.UncompressedSize64
		if total > 1<<30 {
			return nil, fail(422, "Expanded ZIP exceeds 1 GiB")
		}
	}
	ctx := r.Context()
	p := access.From(ctx)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	root := num(r.URL.Query().Get("folder_id"))
	if e = s.targetFolder(ctx, tx, p, root); e != nil {
		return nil, e
	}
	created := 0
	if wrap {
		name := strings.TrimSuffix(filepath.Base(h.Filename), filepath.Ext(h.Filename))
		if !validFileName(name) {
			return nil, fail(422, "Invalid ZIP filename")
		}
		root, e = libraryDir(ctx, tx, p, root, name, "", false)
		if e != nil {
			return nil, e
		}
		created++
	}
	folders := map[string]int64{".": root}
	files := []row{}
	issues := []row{}
	for _, entry := range z.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		name := path.Base(entry.Name)
		if !validFileName(name) || strings.HasPrefix(entry.Name, "__MACOSX/") || strings.HasPrefix(name, ".") {
			continue
		}
		folder := root
		if structure && path.Dir(entry.Name) != "." {
			prefix := ""
			for _, part := range strings.Split(path.Dir(entry.Name), "/") {
				if !validFileName(part) {
					return nil, fail(422, "Invalid ZIP folder name")
				}
				prefix = path.Join(prefix, part)
				if v, ok := folders[prefix]; ok {
					folder = v
					continue
				}
				folder, e = libraryDir(ctx, tx, p, folder, part, "", false)
				if e != nil {
					return nil, e
				}
				created++
				folders[prefix] = folder
			}
		}
		f, e := entry.Open()
		if e != nil {
			return nil, fail(422, "Cannot read ZIP entry")
		}
		k, e := s.storeAsset(ctx, tx, p, io.LimitReader(f, int64(entry.UncompressedSize64)+1), "library", name, folder, 0, 0)
		f.Close()
		if e != nil {
			var bad failure
			if errors.As(e, &bad) {
				issues = append(issues, row{"filename": entry.Name, "error": bad.message})
				continue
			}
			return nil, e
		}
		files = append(files, row{"filename": entry.Name, "file_id": k, "folder_id": id(folder)})
	}
	return row{"files": files, "extracted": len(files), "folders_created": created, "errors": issues}, tx.Commit()
}
func within(root, target string) bool {
	rel, e := filepath.Rel(root, target)
	return e == nil && (rel == "." || filepath.IsLocal(rel))
}
func (s *Service) externalRoot(raw string) (string, error) {
	if !filepath.IsAbs(raw) {
		return "", fail(422, "An absolute external folder path is required")
	}
	target, e := filepath.EvalSymlinks(raw)
	if e != nil {
		return "", fail(400, "External folder is unavailable")
	}
	app, e := filepath.EvalSymlinks(s.dir)
	if e != nil {
		return "", e
	}
	if within(app, target) || within(target, app) {
		return "", fail(400, "Select a folder outside installation storage")
	}
	roots := os.Getenv("BAMBASTIC_EXTERNAL_ROOTS")
	if roots == "" {
		roots = os.Getenv("BAMBUDDY_EXTERNAL_ROOTS")
	}
	for _, root := range filepath.SplitList(roots) {
		if !filepath.IsAbs(root) {
			continue
		}
		base, e := filepath.EvalSymlinks(root)
		if e == nil && within(base, target) {
			stat, e := os.Stat(target)
			if e == nil && stat.IsDir() {
				return target, nil
			}
		}
	}
	return "", fail(400, "Select a directory within BAMBASTIC_EXTERNAL_ROOTS")
}
func (s *Service) mountLibrary(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "settings:update"); e != nil {
		return nil, e
	}
	v, e := input(w, r)
	if e != nil {
		return nil, e
	}
	name := strings.TrimSpace(val(v["name"]))
	if !validFileName(name) {
		return nil, fail(422, "Invalid folder name")
	}
	root, e := s.externalRoot(val(v["external_path"]))
	if e != nil {
		return nil, e
	}
	readonly := true
	if v["readonly"] != nil {
		var ok bool
		readonly, ok = v["readonly"].(bool)
		if !ok {
			return nil, fail(422, "readonly must be a boolean")
		}
	}
	ctx := r.Context()
	p := access.From(ctx)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	parent := num(v["parent_id"])
	if e = s.targetFolder(ctx, tx, p, parent); e != nil {
		return nil, e
	}
	var exists int
	if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM library_folders WHERE external_path=?", root).Scan(&exists); e != nil {
		return nil, e
	}
	if exists != 0 {
		return nil, fail(409, "External folder already linked")
	}
	k, e := libraryDir(ctx, tx, p, parent, name, root, flag(v["show_hidden"]))
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE library_folders SET external_readonly=? WHERE id=?", readonly, k); e != nil {
		return nil, e
	}
	d, e := one(ctx, tx, "SELECT *,0 file_count FROM library_folders WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	return d, tx.Commit()
}
func (s *Service) scanLibrary(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:upload"); e != nil {
		return nil, e
	}
	s.retryExternal()
	ctx := r.Context()
	actor := access.From(ctx)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	k := pathID(r)
	folder, e := one(ctx, tx, "SELECT * FROM library_folders WHERE id=? AND is_external", k)
	if e != nil || !folderAccess(actor, folder) {
		return nil, sql.ErrNoRows
	}
	base, e := s.externalRoot(val(folder["external_path"]))
	if e != nil {
		return nil, e
	}
	root, e := s.openMount(base)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	hidden := flag(folder["external_show_hidden"])
	dirs := map[string]int64{".": k}
	owner := access.Principal{ID: num(folder["created_by_id"])}
	known, e := records(ctx, tx, `WITH RECURSIVE f(id) AS (SELECT ? UNION SELECT l.id FROM library_folders l JOIN f ON l.parent_id=f.id WHERE l.is_external) SELECT id,folder_id,filename,file_size,CAST(fs_modified_at AS TEXT) stamp FROM library_files WHERE is_external AND folder_id IN f`, k)
	if e != nil {
		return nil, e
	}
	type fileKey struct {
		folder int64
		name   string
	}
	old := map[fileKey]row{}
	for _, f := range known {
		old[fileKey{num(f["folder_id"]), val(f["filename"])}] = f
	}
	added, updated, removed := 0, 0, 0
	e = fs.WalkDir(root.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if rel == "." {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".bambastic-") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if !hidden && strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		parent := dirs[path.Dir(rel)]
		if d.IsDir() {
			id, e := libraryDir(ctx, tx, owner, parent, d.Name(), filepath.Join(base, rel), hidden)
			if e == nil {
				_, e = tx.ExecContext(ctx, "UPDATE library_folders SET external_readonly=?,external_show_hidden=? WHERE id=?", folder["external_readonly"], hidden, id)
			}
			dirs[rel] = id
			return e
		}
		switch strings.ToLower(path.Ext(rel)) {
		case ".3mf", ".gcode", ".stl", ".obj", ".step", ".stp", ".png", ".jpg", ".jpeg", ".gif", ".webp", ".svg", ".md", ".pdf", ".csv", ".xlsx", ".ods":
		default:
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() || info.Size() > 1<<30 || info.Size() == 0 {
			return nil
		}
		key := fileKey{parent, d.Name()}
		prev := old[key]
		delete(old, key)
		if prev != nil && num(prev["file_size"]) == info.Size() && val(prev["stamp"]) == info.ModTime().UTC().Format(time.RFC3339Nano) {
			return nil
		}
		reader, e := root.Open(rel)
		if e != nil {
			return e
		}
		v, e := s.prepareAsset(io.LimitReader(reader, 1<<30), "library", d.Name())
		reader.Close()
		if e != nil {
			return e
		}
		v["fs_modified_at"] = info.ModTime().UTC().Format(time.RFC3339Nano)
		v["is_external"] = true
		v["folder_id"] = parent
		v["created_by_id"] = id(owner.ID)
		if prev == nil {
			_, e = insert(ctx, tx, "library_files", v)
			added++
		} else {
			e = update(ctx, tx, "library_files", num(prev["id"]), v, []string{"file_path", "file_hash", "file_metadata", "file_size", "fs_modified_at"})
			updated++
		}
		return e
	})
	if e != nil {
		return nil, e
	}
	ids := []int64{}
	for _, f := range old {
		ids = append(ids, num(f["id"]))
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	for _, n := range ids {
		_, e = tx.ExecContext(ctx, "DELETE FROM library_files WHERE id=? AND is_external AND NOT EXISTS(SELECT 1 FROM print_queue WHERE library_file_id=? AND status IN ('pending','dispatching','printing','awaiting_review'))", n, n)
		if e != nil {
			return nil, e
		}
		removed++
	}
	return row{"status": "completed", "added": added, "updated": updated, "removed": removed}, tx.Commit()
}
