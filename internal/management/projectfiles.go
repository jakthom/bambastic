package management

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type ProjectFiles interface {
	ImportProjectFile(context.Context, *sql.Tx, access.Principal, io.Reader, string, int64) (int64, error)
	OpenProjectFile(context.Context, *sql.Tx, access.Principal, int64) (io.ReadCloser, error)
}

const projectFields = "name description color status target_count target_parts_count target_sets notes tags due_date priority budget"
const exportedBOM = "name quantity_needed quantity_acquired unit_price sourcing_url stl_filename remarks"

func safeName(v string) bool {
	return v != "" && v != "." && v != ".." && filepath.Base(v) == v && len(v) < 256 && !strings.ContainsAny(v, "/\\\x00\r\n:")
}
func selected(v row, fields string) row {
	out := row{}
	for _, k := range strings.Fields(fields) {
		if x, ok := v[k]; ok {
			out[k] = x
		}
	}
	return out
}
func (s *Service) SetProjectFiles(data string, files ProjectFiles) {
	s.data = data
	s.projectFiles = files
}
func (s *Service) projectExtras(m *http.ServeMux) {
	s.route(m, "GET", projectPath+"/timeline", "projects:read", s.timeline)
	s.route(m, "POST", "projects/import", "projects:create", func(r request) (any, error) {
		var d row
		if e := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&d); e != nil {
			return nil, invalid("Invalid project document")
		}
		return s.importProject(r, d, nil)
	})
	s.route(m, "POST", "projects/import/file", "projects:create", s.projectZipImport)
	m.HandleFunc("GET /api/v1/"+projectPath+"/export", s.exportProject)
	m.HandleFunc("GET /api/v1/"+projectPath+"/cover-image", s.projectCover)
	m.HandleFunc("DELETE /api/v1/"+projectPath+"/cover-image", s.projectCover)
}
func (s *Service) timeline(r request) (any, error) {
	p, e := get(r.tx, "projects", r.id())
	if e != nil {
		return nil, e
	}
	entries := []row{{"event_type": "project_created", "timestamp": p["created_at"], "title": "Project created", "description": p["name"], "metadata": row{"project_id": r.id()}}}
	for _, t := range []struct{ table, kind, field string }{{"print_archives", "archives", "print_name"}, {"print_queue", "queue", "status"}} {
		rows, e := list(r.tx, "SELECT * FROM "+t.table+" WHERE project_id=?", r.id())
		if e != nil {
			return nil, e
		}
		for _, v := range rows {
			if !visibleMember(r, t.kind, v, false) {
				continue
			}
			entries = append(entries, row{"event_type": map[string]string{"archives": "archive_added", "queue": "queue_added"}[t.kind], "timestamp": v["created_at"], "title": v[t.field], "metadata": row{"id": v["id"], "printer_id": v["printer_id"], "status": v["status"]}})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool { return fmt.Sprint(entries[i]["timestamp"]) > fmt.Sprint(entries[j]["timestamp"]) })
	return entries, nil
}
func (s *Service) coverDir(id int64) string {
	return filepath.Join(s.data, "projects", fmt.Sprint(id), "attachments")
}
func (s *Service) coverUpload(r request) (any, error) {
	if _, e := get(r.tx, "projects", r.id()); e != nil {
		return nil, e
	}
	if s.data == "" {
		return nil, bad("Project storage unavailable")
	}
	r.Body = http.MaxBytesReader(nil, r.Body, (8<<20)+(64<<10))
	if e := r.ParseMultipartForm(8 << 20); e != nil {
		return nil, invalid("Expected a cover image")
	}
	defer r.MultipartForm.RemoveAll()
	file, _, e := r.FormFile("file")
	if e != nil {
		return nil, invalid("Cover image required")
	}
	defer file.Close()
	data, e := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	if e != nil || len(data) > 8<<20 {
		return nil, invalid("Cover exceeds 8 MiB")
	}
	ext := map[string]string{"image/jpeg": ".jpg", "image/png": ".png", "image/webp": ".webp", "image/gif": ".gif"}[http.DetectContentType(data)]
	if ext == "" {
		return nil, invalid("Use a JPEG, PNG, WebP or GIF image")
	}
	dir := s.coverDir(r.id())
	if e = os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	var token [16]byte
	rand.Read(token[:])
	name := "cover_" + hex.EncodeToString(token[:]) + ext
	path := filepath.Join(dir, name)
	if e = os.WriteFile(path, data, 0600); e != nil {
		return nil, e
	}
	if _, e = r.tx.Exec("UPDATE projects SET cover_image_filename=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", name, r.id()); e != nil {
		os.Remove(path)
		return nil, e
	}
	return row{"filename": name, "cover_image_filename": name, "message": "Cover image saved"}, nil
}
func (s *Service) projectCover(w http.ResponseWriter, r *http.Request) {
	perm := "projects:read"
	if r.Method == "DELETE" {
		perm = "projects:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	var id int64
	var name string
	e := s.db.QueryRow("SELECT id,coalesce(cover_image_filename,'') FROM projects WHERE id=?", r.PathValue("id")).Scan(&id, &name)
	if e != nil || !safeName(name) {
		httpx.Error(w, 404, "Cover image unavailable")
		return
	}
	path := filepath.Join(s.coverDir(id), name)
	if r.Method == "DELETE" {
		res, e := s.db.Exec("UPDATE projects SET cover_image_filename=NULL WHERE id=? AND cover_image_filename=?", id, name)
		if e != nil {
			httpx.Error(w, 500, "Cannot remove cover")
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			os.Remove(path)
		}
		httpx.JSON(w, 200, row{"message": "Cover removed"})
		return
	}
	http.ServeFile(w, r, path)
}

type exportFile struct {
	ID   int64
	Name string
}

func (s *Service) exportProject(w http.ResponseWriter, req *http.Request) {
	if !access.Require(w, req, "projects:read") {
		return
	}
	tx, e := s.db.BeginTx(req.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Database unavailable")
		return
	}
	defer tx.Rollback()
	r := request{Request: req, tx: tx}
	id := r.id()
	p, e := get(tx, "projects", id)
	if e != nil {
		httpx.Error(w, 404, "Unknown project")
		return
	}
	data := selected(p, projectFields)
	bom, e := projectBOM(r, id)
	if e != nil {
		httpx.Error(w, 500, "BOM unavailable")
		return
	}
	outBOM := []row{}
	for _, v := range bom {
		outBOM = append(outBOM, selected(v, exportedBOM))
	}
	data["bom_items"] = outBOM
	files := []exportFile{}
	folders := []row{}
	actor := access.From(req.Context())
	if actor.Has("library:read_all") || actor.Has("library:read_own") {
		dirs, e := list(tx, "SELECT * FROM library_folders WHERE project_id=? ORDER BY name", id)
		if e != nil {
			httpx.Error(w, 500, "Project folders unavailable")
			return
		}
		for _, f := range dirs {
			if !actor.Has("library:read_all") && actor.ID != intID(f["created_by_id"]) && !httpx.Bool(f["shared"]) {
				continue
			}
			folder := str(f["name"])
			if !safeName(folder) {
				httpx.Error(w, 422, "Folder cannot be represented in a ZIP")
				return
			}
			records, e := list(tx, "SELECT id,filename,file_type,notes,created_by_id FROM library_files WHERE folder_id=? AND deleted_at IS NULL ORDER BY filename", f["id"])
			if e != nil {
				httpx.Error(w, 500, "Project files unavailable")
				return
			}
			meta := []row{}
			for _, v := range records {
				if !actor.Has("library:read_all") && actor.ID != intID(v["created_by_id"]) {
					continue
				}
				n := str(v["filename"])
				if !safeName(n) {
					httpx.Error(w, 422, "Invalid library filename")
					return
				}
				meta = append(meta, selected(v, "filename file_type notes"))
				files = append(files, exportFile{intID(v["id"]), "files/" + folder + "/" + n})
			}
			folders = append(folders, row{"name": folder, "files": meta})
		}
	}
	data["linked_folders"] = folders
	if req.URL.Query().Get("format") == "json" {
		httpx.JSON(w, 200, data)
		return
	}
	f, e := os.CreateTemp("", "bambastic-project-*.zip")
	if e != nil {
		httpx.Error(w, 500, "Cannot stage project export")
		return
	}
	defer os.Remove(f.Name())
	defer f.Close()
	z := zip.NewWriter(f)
	writer, e := z.Create("project.json")
	if e == nil {
		e = json.NewEncoder(writer).Encode(data)
	}
	paths := map[string]bool{}
	for _, file := range files {
		if e != nil {
			break
		}
		if paths[file.Name] {
			e = errors.New("duplicate archive path")
			break
		}
		paths[file.Name] = true
		var in io.ReadCloser
		in, e = s.projectFiles.OpenProjectFile(req.Context(), tx, actor, file.ID)
		if e != nil {
			break
		}
		writer, e = z.Create(file.Name)
		if e == nil {
			_, e = io.Copy(writer, in)
		}
		in.Close()
	}
	closeErr := z.Close()
	if e != nil || closeErr != nil {
		httpx.Error(w, 409, "An exported file could not be read")
		return
	}
	tx.Rollback()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("project-%d.zip", id)))
	http.ServeFile(w, req, f.Name())
}
func (s *Service) projectZipImport(r request) (any, error) {
	r.Body = http.MaxBytesReader(nil, r.Body, (2<<30)+(1<<20))
	if e := r.ParseMultipartForm(8 << 20); e != nil {
		return nil, invalid("Expected a project file")
	}
	defer r.MultipartForm.RemoveAll()
	file, header, e := r.FormFile("file")
	if e != nil {
		return nil, invalid("Missing project file")
	}
	defer file.Close()
	if header.Size > 2<<30 {
		return nil, invalid("Project archive is too large")
	}
	var meta io.Reader = file
	entries := map[string]*zip.File{}
	if strings.EqualFold(filepath.Ext(header.Filename), ".zip") {
		z, e := zip.NewReader(file, header.Size)
		if e != nil || len(z.File) > 50000 {
			return nil, invalid("Invalid project archive")
		}
		var total uint64
		for _, entry := range z.File {
			if entry.FileInfo().IsDir() {
				continue
			}
			if entries[entry.Name] != nil || entry.Mode()&os.ModeSymlink != 0 {
				return nil, invalid("Duplicate or unsupported ZIP entry")
			}
			if entry.UncompressedSize64 > 2<<30-total {
				return nil, invalid("Expanded project exceeds size limit")
			}
			total += entry.UncompressedSize64
			entries[entry.Name] = entry
		}
		manifest := entries["project.json"]
		if manifest == nil {
			return nil, invalid("Missing project.json")
		}
		m, e := manifest.Open()
		if e != nil {
			return nil, e
		}
		defer m.Close()
		meta = m
	}
	b, e := io.ReadAll(io.LimitReader(meta, (2<<20)+1))
	var doc row
	if e != nil || len(b) > 2<<20 || json.Unmarshal(b, &doc) != nil {
		return nil, invalid("Invalid project manifest")
	}
	return s.importProject(r, doc, entries)
}
func (s *Service) importProject(r request, doc row, entries map[string]*zip.File) (any, error) {
	input := selected(doc, projectFields)
	original := r.Body
	b, _ := json.Marshal(input)
	r.Body = io.NopCloser(bytes.NewReader(b))
	result, e := saveProject(r, 0)
	r.Body = original
	if e != nil {
		return nil, e
	}
	p := result.(row)
	id := intID(p["id"])
	if status := str(doc["status"]); status == "completed" || status == "archived" || status == "active" {
		_, e = r.tx.Exec("UPDATE projects SET status=? WHERE id=?", status, id)
		if e != nil {
			return nil, e
		}
	}
	decode := func(v any) ([]row, error) {
		raw, e := json.Marshal(v)
		if e != nil {
			return nil, e
		}
		var list []row
		e = json.Unmarshal(raw, &list)
		return list, e
	}
	items, e := decode(doc["bom_items"])
	if e != nil {
		return nil, invalid("Invalid bill of materials")
	}
	for i, item := range items {
		data := selected(item, exportedBOM)
		if strings.TrimSpace(str(data["name"])) == "" {
			return nil, invalid("BOM item name required")
		}
		data["project_id"] = id
		data["sort_order"] = i
		if data["quantity_needed"] == nil {
			data["quantity_needed"] = 1
		}
		if data["quantity_acquired"] == nil {
			data["quantity_acquired"] = 0
		}
		if _, e = insert(r.tx, "project_bom_items", data); e != nil {
			return nil, e
		}
	}
	dirs, e := decode(doc["linked_folders"])
	if e != nil {
		return nil, invalid("Invalid folder list")
	}
	actor := access.From(r.Context())
	for _, dir := range dirs {
		name := str(dir["name"])
		if !safeName(name) {
			return nil, invalid("Invalid folder name")
		}
		if !actor.Has("library:upload") && !actor.Has("library:create_folder") {
			return nil, denied()
		}
		var owner any
		if actor.ID != 0 {
			owner = actor.ID
		}
		folder, e := insert(r.tx, "library_folders", row{"name": name, "project_id": id, "created_by_id": owner, "is_external": false, "external_readonly": false, "external_show_hidden": false})
		if e != nil {
			return nil, e
		}
		files, e := decode(dir["files"])
		if e != nil {
			return nil, invalid("Invalid file list")
		}
		for _, v := range files {
			fn := str(v["filename"])
			if !safeName(fn) {
				return nil, invalid("Invalid library filename")
			}
			if len(entries) == 0 {
				continue
			}
			entry := entries["files/"+name+"/"+fn]
			if entry == nil {
				return nil, invalid("Missing file in project ZIP")
			}
			in, e := entry.Open()
			if e != nil {
				return nil, e
			}
			fileID, e := s.projectFiles.ImportProjectFile(r.Context(), r.tx, actor, in, fn, folder)
			in.Close()
			if e != nil {
				return nil, e
			}
			if _, e = r.tx.Exec("UPDATE library_files SET notes=? WHERE id=?", v["notes"], fileID); e != nil {
				return nil, e
			}
		}
	}
	return projectView(r, id)
}
