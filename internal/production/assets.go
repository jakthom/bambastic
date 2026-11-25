package production

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

type plate struct {
	Index       int64    `json:"index"`
	Name        string   `json:"name"`
	Thumbnail   bool     `json:"has_thumbnail"`
	Printable   bool     `json:"has_gcode"`
	Seconds     int64    `json:"print_time_seconds"`
	Grams       float64  `json:"filament_used_grams"`
	Filaments   []row    `json:"filaments"`
	Objects     []string `json:"objects"`
	ObjectCount int      `json:"object_count"`
}
type plateInfo struct {
	Plates    []plate `json:"plates"`
	Printable bool    `json:"has_gcode"`
	Model     string  `json:"sliced_for_model,omitempty"`
}

var plateEntry = regexp.MustCompile(`^Metadata/plate_([1-9][0-9]*)\.(gcode|json|png)$`)

type slicerXML struct {
	Plates []struct {
		Meta []struct {
			Key   string `xml:"key,attr"`
			Value string `xml:"value,attr"`
		} `xml:"metadata"`
		Filament []struct {
			ID     string  `xml:"id,attr"`
			Preset string  `xml:"tray_info_idx,attr"`
			Type   string  `xml:"type,attr"`
			Color  string  `xml:"color,attr"`
			Used   float64 `xml:"used_g,attr"`
		} `xml:"filament"`
		Objects []struct {
			Name    string `xml:"name,attr"`
			ID      string `xml:"identify_id,attr"`
			Skipped string `xml:"skipped,attr"`
		} `xml:"object"`
	} `xml:"plate"`
}

func inspect(path, filename string) (plateInfo, error) {
	out := plateInfo{Plates: []plate{}}
	ext := strings.ToLower(filepath.Ext(filename))
	if ext == ".gcode" {
		out.Printable = true
		out.Plates = []plate{{Index: 1, Name: filename, Printable: true, Filaments: []row{}, Objects: []string{}}}
		return out, nil
	}
	if ext != ".3mf" {
		return out, nil
	}
	z, e := zip.OpenReader(path)
	if e != nil {
		return out, fail(422, "Invalid 3MF container")
	}
	defer z.Close()
	ps := map[int64]*plate{}
	get := func(n int64) *plate {
		p := ps[n]
		if p == nil {
			p = &plate{Index: n, Name: fmt.Sprintf("Plate %d", n), Filaments: []row{}, Objects: []string{}}
			ps[n] = p
		}
		return p
	}
	if len(z.File) > 50000 {
		return out, fail(422, "Too many entries in 3MF")
	}
	for _, f := range z.File {
		if m := plateEntry.FindStringSubmatch(f.Name); m != nil {
			p := get(num(m[1]))
			if m[2] == "gcode" && f.UncompressedSize64 > 0 {
				p.Printable = true
				out.Printable = true
			}
			if m[2] == "png" {
				p.Thumbnail = true
			}
		}
		if f.Name != "Metadata/slice_info.config" && f.Name != "Metadata/project_settings.config" && f.Name != "Metadata/model_settings.config" {
			continue
		}
		if f.UncompressedSize64 > 8<<20 {
			continue
		}
		rd, e := f.Open()
		if e != nil {
			continue
		}
		buf, e := io.ReadAll(io.LimitReader(rd, (8<<20)+1))
		rd.Close()
		if e != nil || len(buf) > 8<<20 {
			continue
		}
		if f.Name == "Metadata/project_settings.config" {
			var data struct {
				Model string `json:"printer_model"`
			}
			if json.Unmarshal(buf, &data) == nil {
				out.Model = strings.TrimPrefix(data.Model, "Bambu Lab ")
			}
			continue
		}
		var doc slicerXML
		if xml.Unmarshal(buf, &doc) != nil {
			continue
		}
		for i, x := range doc.Plates {
			meta := map[string]string{}
			for _, m := range x.Meta {
				meta[m.Key] = m.Value
			}
			n := num(meta["plater_id"])
			if n == 0 {
				n = int64(i + 1)
			}
			p := get(n)
			if name := meta["plater_name"]; name != "" {
				p.Name = name
			}
			if t := num(meta["prediction"]); t > 0 {
				p.Seconds = t
			}
			if g, _ := strconv.ParseFloat(meta["weight"], 64); g > 0 {
				p.Grams = g
			}
			for _, o := range x.Objects {
				if o.Name != "" {
					p.Objects = append(p.Objects, o.Name)
				}
			}
			p.ObjectCount = len(p.Objects)
			var grams float64
			for _, f := range x.Filament {
				grams += f.Used
				p.Filaments = append(p.Filaments, row{"id": num(f.ID), "tray_info_idx": f.Preset, "type": f.Type, "color": f.Color, "used_g": f.Used})
			}
			if p.Grams == 0 {
				p.Grams = grams
			}
		}
	}
	for _, p := range ps {
		out.Plates = append(out.Plates, *p)
	}
	sort.Slice(out.Plates, func(i, j int) bool { return out.Plates[i].Index < out.Plates[j].Index })
	return out, nil
}

type register func(method, path string, f endpoint)

func (s *Service) assetRoutes(rt register) {
	for _, kind := range []string{"archives", "library"} {
		base := "/archives"
		if kind == "library" {
			base = "/library/files"
		}
		for _, suffix := range []string{"", "/{$}"} {
			rt("GET", base+suffix, s.assetList(kind))
			if kind == "library" {
				rt("POST", base+suffix, s.upload(kind))
			}
		}
		rt("GET", base+"/{id}", s.assetRead(kind, "detail"))
		rt("GET", base+"/{id}/plates", s.assetRead(kind, "plates"))
		rt("GET", base+"/{id}/download", s.assetRead(kind, "download"))
		rt("GET", base+"/{id}/thumbnail", s.assetRead(kind, "thumbnail"))
		rt("GET", base+"/{id}/plate-thumbnail/{plate}", s.assetRead(kind, "thumbnail"))
		rt("PATCH", base+"/{id}", s.assetEdit(kind))
		rt("DELETE", base+"/{id}", s.assetDelete(kind))
	}
	rt("POST", "/archives/upload", s.upload("archives"))
	rt("GET", "/archives/search", s.assetList("archives"))
	for _, p := range []string{"/library/folders", "/library/folders/{$}"} {
		rt("GET", p, s.folders)
		rt("POST", p, s.addFolder)
	}
}
func (s *Service) accessible(ctx context.Context, d database, p access.Principal, kind string, v row, action string) bool {
	owner := num(v["created_by_id"])
	if !owned(p, sourceResource(kind), action, owner) {
		if kind != "library" || action != "read" || num(v["folder_id"]) == 0 || !p.Has("library:read_own") {
			return false
		}
		folder, e := one(ctx, d, "SELECT shared FROM library_folders WHERE id=?", v["folder_id"])
		if e != nil || !flag(folder["shared"]) {
			return false
		}
	}
	if kind != "archives" || p.PrinterIDs == nil {
		return true
	}
	direct := num(v["printer_id"])
	if action == "read" && direct != 0 && p.AllowsPrinter(direct) {
		return true
	}
	rs, e := records(ctx, d, "SELECT DISTINCT printer_id FROM print_log_entries WHERE archive_id=? AND printer_id IS NOT NULL", v["id"])
	if e != nil {
		return false
	}
	allowed, denied := false, false
	for _, run := range rs {
		if p.AllowsPrinter(num(run["printer_id"])) {
			allowed = true
		} else {
			denied = true
		}
	}
	if action == "read" {
		return allowed || direct == 0 && len(rs) == 0
	}
	return scope(p, direct) && !denied
}
func decodeAsset(v row) {
	for _, k := range []string{"file_metadata", "extra_data", "plate_estimates", "photos"} {
		if v[k] != nil {
			v[k] = unpack(v[k])
		}
	}
	if m, ok := v["file_metadata"].(map[string]any); ok {
		v["metadata"] = m
		v["has_gcode"] = m["has_gcode"]
	}
	delete(v, "confirm_token")
	delete(v, "timelapse_baseline")
	if _, ok := v["source_3mf_path"]; ok {
		v["has_source_3mf"] = val(v["source_3mf_path"]) != ""
	}
	if _, ok := v["f3d_path"]; ok {
		v["has_f3d"] = val(v["f3d_path"]) != ""
	}
}
func (s *Service) assetList(kind string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		if !p.Has(sourceResource(kind)+":read_own") && !p.Has(sourceResource(kind)+":read_all") {
			return nil, fail(403, "Permission denied")
		}
		q := "SELECT * FROM " + sourceTable(kind) + " WHERE deleted_at IS NULL"
		args := []any{}
		qs := r.URL.Query()
		if kind == "library" {
			if flag(qs.Get("internal_only")) && flag(qs.Get("external_only")) {
				return nil, fail(422, "Conflicting storage modes")
			}
			if flag(qs.Get("internal_only")) {
				q += " AND NOT is_external"
			}
			if flag(qs.Get("external_only")) {
				q += " AND is_external"
			}
			if len(qs["tag_ids"]) > 0 {
				for _, entry := range qs["tag_ids"] {
					for _, tag := range strings.Split(entry, ",") {
						q += " AND EXISTS(SELECT 1 FROM library_file_tags t WHERE t.file_id=library_files.id AND t.tag_id=?)"
						args = append(args, num(tag))
					}
				}
			} else if folder := num(qs.Get("folder_id")); folder > 0 {
				if flag(qs.Get("recursive")) {
					q += " AND folder_id IN (WITH RECURSIVE d(id) AS (SELECT ? UNION SELECT f.id FROM library_folders f JOIN d ON f.parent_id=d.id) SELECT id FROM d)"
				} else {
					q += " AND folder_id=?"
				}
				args = append(args, folder)
			} else if flag(qs.Get("include_root")) {
				q += " AND folder_id IS NULL"
			}
		}
		if search := strings.TrimSpace(qs.Get("q")); search != "" {
			for _, word := range strings.Fields(search) {
				term := "%" + strings.TrimSuffix(word, "*") + "%"
				q += " AND (filename LIKE ? OR notes LIKE ?"
				args = append(args, term, term)
				if kind == "archives" {
					for _, field := range []string{"print_name", "tags", "designer", "filament_type"} {
						q += " OR " + field + " LIKE ?"
						args = append(args, term)
					}
				}
				q += ")"
			}
		}
		if qs.Has("project_id") {
			q += " AND project_id=?"
			args = append(args, num(qs.Get("project_id")))
		}
		q += " ORDER BY id DESC"
		rs, e := records(ctx, s.db, q, args...)
		if e != nil {
			return nil, e
		}
		out := []row{}
		skip := max(num(qs.Get("skip")), num(qs.Get("offset")))
		limit := num(r.URL.Query().Get("limit"))
		for _, v := range rs {
			if s.accessible(ctx, s.db, p, kind, v, "read") {
				if e = s.presentAsset(ctx, s.db, p, kind, v); e != nil {
					return nil, e
				}
				if kind == "archives" {
					if s := qs.Get("status"); s != "" && s != val(v["status"]) {
						continue
					}
					if printer := num(qs.Get("printer_id")); printer > 0 && num(v["printer_id"]) != printer {
						continue
					}
					day := val(v["created_at"])
					if len(day) >= 10 {
						day = day[:10]
					}
					if from := qs.Get("date_from"); from != "" && day < from {
						continue
					}
					if to := qs.Get("date_to"); to != "" && day > to {
						continue
					}
				}
				if skip > 0 {
					skip--
					continue
				}
				decodeAsset(v)
				out = append(out, v)
				if limit > 0 && int64(len(out)) >= limit {
					break
				}
			}
		}
		return out, nil
	}
}
func (s *Service) getAsset(ctx context.Context, d database, p access.Principal, kind string, k int64, action string) (row, error) {
	v, e := one(ctx, d, "SELECT * FROM "+sourceTable(kind)+" WHERE id=? AND deleted_at IS NULL", k)
	if errors.Is(e, sql.ErrNoRows) && kind == "archives" {
		v, e = one(ctx, d, "SELECT a.* FROM archive_aliases x JOIN print_archives a ON a.id=x.archive_id WHERE x.merged_archive_id=? AND a.deleted_at IS NULL", k)
	}
	if e != nil {
		return nil, e
	}
	if !s.accessible(ctx, d, p, kind, v, action) {
		return nil, sql.ErrNoRows
	}
	return v, nil
}
func (s *Service) meta(kind string, v row) (plateInfo, error) {
	key := "extra_data"
	if kind == "library" {
		key = "file_metadata"
	}
	var inf plateInfo
	if json.Unmarshal([]byte(val(v[key])), &inf) == nil && inf.Plates != nil {
		return inf, nil
	}
	p, e := s.assetPath(v["file_path"])
	if e != nil {
		return inf, e
	}
	return inspect(p, val(v["filename"]))
}
func (s *Service) assetRead(kind, act string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		v, e := s.getAsset(r.Context(), s.db, access.From(r.Context()), kind, pathID(r), "read")
		if e != nil {
			return nil, e
		}
		if act == "detail" {
			if e = s.presentAsset(r.Context(), s.db, access.From(r.Context()), kind, v); e != nil {
				return nil, e
			}
			decodeAsset(v)
			return v, nil
		}
		if act == "thumbnail" && r.PathValue("plate") == "" && val(v["thumbnail_path"]) != "" {
			src, err := s.assetPath(v["thumbnail_path"])
			if err != nil {
				return nil, err
			}
			w.Header().Set("Content-Type", "image/png")
			http.ServeFile(w, r, src)
			return nil, nil
		}
		path, e := s.assetPath(v["file_path"])
		if e != nil {
			return nil, e
		}
		if act == "download" {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": val(v["filename"])}))
			http.ServeFile(w, r, path)
			return nil, nil
		}
		info, e := s.meta(kind, v)
		if e != nil {
			return nil, e
		}
		if act == "plates" {
			return row{"plates": info.Plates, "has_gcode": info.Printable, "is_multi_plate": len(info.Plates) > 1, "plate_count": len(info.Plates)}, nil
		}
		idx := num(r.PathValue("plate"))
		if idx == 0 && len(info.Plates) > 0 {
			idx = info.Plates[0].Index
		}
		z, e := zip.OpenReader(path)
		if e != nil {
			return nil, sql.ErrNoRows
		}
		defer z.Close()
		for _, f := range z.File {
			if (f.Name == fmt.Sprintf("Metadata/plate_%d.png", idx) || r.PathValue("plate") == "" && f.Name == "Metadata/thumbnail.png") && f.UncompressedSize64 <= 8<<20 {
				rd, e := f.Open()
				if e != nil {
					return nil, e
				}
				defer rd.Close()
				w.Header().Set("Content-Type", "image/png")
				_, _ = io.Copy(w, io.LimitReader(rd, 8<<20))
				return nil, nil
			}
		}
		return nil, sql.ErrNoRows
	}
}
func (s *Service) upload(kind string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		perm := "archives:create"
		if kind == "library" {
			perm = "library:upload"
		}
		if e := permitted(r, perm); e != nil {
			return nil, e
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
		if e := r.ParseMultipartForm(8 << 20); e != nil {
			return nil, fail(400, "Invalid or oversized multipart upload")
		}
		defer r.MultipartForm.RemoveAll()
		f, h, e := r.FormFile("file")
		if e != nil {
			return nil, fail(422, "A file is required")
		}
		defer f.Close()
		name := filepath.Base(strings.ReplaceAll(h.Filename, "\\", "/"))
		ext := strings.ToLower(filepath.Ext(name))
		if len(name) > 250 || name == "." || ext == "" {
			return nil, fail(422, "Invalid filename")
		}
		if kind == "archives" && ext != ".3mf" && ext != ".gcode" {
			return nil, fail(422, "Archive requires a 3MF or G-code file")
		}
		ctx := r.Context()
		p := access.From(ctx)
		printer := num(r.FormValue("printer_id"))
		if !scope(p, printer) {
			return nil, sql.ErrNoRows
		}
		folder := num(r.FormValue("folder_id"))
		if e = s.checkLibraryDestination(ctx, s.db, p, folder, kind == "library"); e != nil {
			return nil, e
		}
		var k int64
		if kind == "library" {
			record, err := s.prepareAsset(f, kind, name)
			if err != nil {
				return nil, err
			}
			record["created_by_id"] = id(p.ID)
			record["project_id"] = id(num(r.FormValue("project_id")))
			record["folder_id"] = id(folder)
			tx, err := s.db.BeginTx(ctx, nil)
			if err != nil {
				return nil, err
			}
			var edits externalEdits
			defer edits.finish(s)
			defer tx.Rollback()
			if err = s.checkLibraryDestination(ctx, tx, p, folder, true); err != nil {
				return nil, err
			}
			k, e = edits.insertLibrary(ctx, s, tx, record, folder)
			if e == nil {
				e = tx.Commit()
			}
		} else {
			k, e = s.storeAsset(ctx, s.db, p, f, kind, name, folder, printer, num(r.FormValue("project_id")))
		}
		if e != nil {
			return nil, e
		}
		v, e := one(ctx, s.db, "SELECT * FROM "+sourceTable(kind)+" WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		decodeAsset(v)
		if kind == "archives" {
			s.emit(row{"type": "archive_created", "data": row{"printer_id": id(printer)}})
		}
		return v, nil
	}
}
func (s *Service) assetEdit(kind string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		if kind == "archives" {
			if k := num(r.URL.Query().Get("run_id")); k != 0 && m["run_id"] == nil {
				m["run_id"] = k
			}
			return s.editArchive(ctx, access.From(ctx), pathID(r), m)
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		var edits externalEdits
		defer edits.finish(s)
		defer tx.Rollback()
		p := access.From(ctx)
		k := pathID(r)
		v, e := s.getAsset(ctx, tx, p, kind, k, "update")
		if e != nil {
			return nil, e
		}
		fields := []string{"notes", "external_url", "filename", "project_id", "folder_id", "fs_modified_at"}
		if n, ok := m["filename"]; ok {
			name := val(n)
			if !validFileName(name) || !strings.EqualFold(filepath.Ext(name), filepath.Ext(val(v["filename"]))) {
				return nil, fail(422, "Invalid filename or extension")
			}
			if flag(v["is_external"]) && name != val(v["filename"]) {
				folder, e := s.externalFolder(ctx, tx, num(v["folder_id"]))
				if e != nil {
					return nil, e
				}
				if folder == nil {
					return nil, fail(409, "External source folder missing")
				}
				if e = edits.transfer(ctx, s, tx, v, folder, folder, name, true); e != nil {
					return nil, e
				}
				m["fs_modified_at"] = v["fs_modified_at"]
			}
		}
		if dest, ok := m["folder_id"]; ok {
			dk := num(dest)
			if dk != num(v["folder_id"]) {
				if e = s.checkLibraryDestination(ctx, tx, p, dk, true); e != nil {
					return nil, e
				}
				target, e := s.externalFolder(ctx, tx, dk)
				if e != nil {
					return nil, e
				}
				if flag(v["is_external"]) || target != nil {
					return nil, fail(400, "Use the library move operation for an external folder")
				}
			}
			m["folder_id"] = id(dk)
		}
		if project, ok := m["project_id"]; ok {
			if n := num(project); n != 0 {
				if _, e = one(ctx, tx, "SELECT id FROM projects WHERE id=?", n); e != nil {
					return nil, e
				}
			}
			m["project_id"] = id(num(project))
		}
		if e = update(ctx, tx, sourceTable(kind), k, m, fields); e != nil {
			return nil, e
		}
		v, e = one(ctx, tx, "SELECT * FROM "+sourceTable(kind)+" WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		decodeAsset(v)
		e = tx.Commit()
		return v, e
	}
}
func update(ctx context.Context, d database, table string, k int64, m row, fields []string) error {
	cols := []string{}
	args := []any{}
	for _, f := range fields {
		if x, ok := m[f]; ok {
			cols = append(cols, f+"=?")
			args = append(args, x)
		}
	}
	if len(cols) == 0 {
		return nil
	}
	args = append(args, k)
	_, e := d.ExecContext(ctx, "UPDATE "+table+" SET "+strings.Join(cols, ",")+" WHERE id=?", args...)
	return e
}
func (s *Service) assetDelete(kind string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		k := pathID(r)
		f, e := s.getAsset(ctx, tx, access.From(ctx), kind, k, "delete")
		if e != nil {
			return nil, e
		}
		field := "archive_id"
		if kind == "library" {
			field = "library_file_id"
		}
		busy, e := one(ctx, tx, "SELECT count(*) n FROM print_queue WHERE "+field+"=? AND status IN ('printing','dispatching','pending')", k)
		if e != nil {
			return nil, e
		}
		if num(busy["n"]) > 0 {
			return nil, fail(409, "File is in use by the queue")
		}
		if kind == "library" && flag(f["is_external"]) {
			if e = s.forgetExternal(ctx, tx, k); e != nil {
				return nil, e
			}
			return row{"status": "success", "success": true, "trashed": false}, tx.Commit()
		}
		_, e = tx.ExecContext(ctx, "UPDATE "+sourceTable(kind)+" SET deleted_at=CURRENT_TIMESTAMP WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		return row{"success": true}, tx.Commit()
	}
}
func folderAccess(p access.Principal, fo row) bool {
	return p.Has("library:read_all") || p.Has("library:read_own") && (p.ID != 0 && num(fo["created_by_id"]) == p.ID || flag(fo["shared"]))
}
func (s *Service) folders(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !p.Has("library:read_all") && !p.Has("library:read_own") {
		return nil, fail(403, "Permission denied")
	}
	rs, e := records(r.Context(), s.db, "SELECT * FROM library_folders ORDER BY name")
	if e != nil {
		return nil, e
	}
	out := []row{}
	seen := map[int64]row{}
	for _, f := range rs {
		if folderAccess(p, f) {
			f["children"] = []row{}
			f["file_count"] = 0
			f["can_write"] = folderWrite(p, f)
			f["can_rename"] = owned(p, "library", "update", num(f["created_by_id"]))
			f["can_delete"] = owned(p, "library", "delete", num(f["created_by_id"]))
			seen[num(f["id"])] = f
		}
	}
	files, e := records(r.Context(), s.db, "SELECT * FROM library_files WHERE deleted_at IS NULL AND folder_id IS NOT NULL")
	if e != nil {
		return nil, e
	}
	for _, f := range files {
		if dir := seen[num(f["folder_id"])]; dir != nil && s.accessible(r.Context(), s.db, p, "library", f, "read") {
			dir["file_count"] = int(num(dir["file_count"])) + 1
		}
	}
	for _, f := range rs {
		if seen[num(f["id"])] == nil {
			continue
		}
		if parent := seen[num(f["parent_id"])]; parent != nil {
			parent["children"] = append(parent["children"].([]row), f)
		} else {
			out = append(out, f)
		}
	}
	return out, nil
}
func (s *Service) addFolder(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:upload"); e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	name := strings.TrimSpace(val(m["name"]))
	if !validFileName(name) {
		return nil, fail(422, "Invalid folder name")
	}
	p := access.From(r.Context())
	if parent := num(m["parent_id"]); parent != 0 {
		fo, e := one(r.Context(), s.db, "SELECT * FROM library_folders WHERE id=?", parent)
		if e != nil {
			return nil, e
		}
		if !folderWrite(p, fo) || flag(fo["is_external"]) {
			return nil, fail(403, "Folder is not writable")
		}
	}
	if flag(m["shared"]) && !p.Has("library:update_all") {
		return nil, fail(403, "Cannot create a shared folder")
	}
	if aid := num(m["archive_id"]); aid != 0 {
		if _, e = s.getAsset(r.Context(), s.db, p, "archives", aid, "update"); e != nil {
			return nil, e
		}
	}
	k, e := insert(r.Context(), s.db, "library_folders", row{"name": name, "parent_id": id(num(m["parent_id"])), "project_id": id(num(m["project_id"])), "archive_id": id(num(m["archive_id"])), "shared": flag(m["shared"]), "created_by_id": id(p.ID), "is_external": false, "external_readonly": false, "external_show_hidden": false})
	if e != nil {
		return nil, e
	}
	return one(r.Context(), s.db, "SELECT * FROM library_folders WHERE id=?", k)
}

func (s *Service) storeAsset(ctx context.Context, d database, p access.Principal, f io.Reader, kind, name string, folder, printer, project int64) (int64, error) {
	v, e := s.prepareAsset(f, kind, name)
	if e != nil {
		return 0, e
	}
	v["created_by_id"] = id(p.ID)
	v["project_id"] = id(project)
	if kind == "archives" {
		v["printer_id"] = id(printer)
	} else {
		v["folder_id"] = id(folder)
	}
	return insert(ctx, d, sourceTable(kind), v)
}

// prepareAsset streams bytes to an immutable content-addressed file and extracts printable plates.
func (s *Service) prepareAsset(f io.Reader, kind, name string) (row, error) {
	ext := strings.ToLower(filepath.Ext(name))
	tmp, e := os.CreateTemp(filepath.Join(s.dir, "production"), "upload-*")
	if e != nil {
		return nil, e
	}
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	sz, e := io.Copy(io.MultiWriter(hash, tmp), f)
	if e == nil {
		e = tmp.Sync()
	}
	ce := tmp.Close()
	if e != nil {
		return nil, e
	}
	if ce != nil {
		return nil, ce
	}
	if sz == 0 {
		return nil, fail(422, "Empty file")
	}
	inf, e := inspect(tmp.Name(), name)
	if e != nil {
		return nil, e
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	rel := filepath.Join("production", "objects", digest+ext)
	if e = os.Link(tmp.Name(), filepath.Join(s.dir, rel)); e != nil && !errors.Is(e, os.ErrExist) {
		return nil, e
	}
	v := row{"filename": name, "file_path": rel, "file_size": sz}
	if kind == "archives" {
		v["print_name"] = strings.TrimSuffix(strings.TrimSuffix(name, filepath.Ext(name)), ".gcode")
		v["content_hash"] = digest
		v["gcode_hash"] = plateDigest(tmp.Name(), name, digest)
		v["status"] = "archived"
		v["is_favorite"] = false
		v["quantity"] = 1
		v["extra_data"] = jstr(inf)
		var secs int64
		var grams float64
		for _, pl := range inf.Plates {
			secs += pl.Seconds
			grams += pl.Grams
		}
		est := map[string]any{}
		for _, pl := range inf.Plates {
			est[strconv.FormatInt(pl.Index, 10)] = row{"grams": pl.Grams, "print_time_seconds": pl.Seconds}
		}
		v["plate_estimates"] = jstr(est)
		v["print_time_seconds"] = secs
		v["filament_used_grams"] = grams
		if inf.Model != "" {
			v["sliced_for_model"] = inf.Model
		}
	} else {
		v["file_hash"] = digest
		v["file_type"] = strings.TrimPrefix(ext, ".")
		v["is_external"] = false
		v["print_count"] = 0
		v["file_metadata"] = jstr(inf)
	}
	return v, nil
}
