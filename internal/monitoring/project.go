package monitoring

import (
	"archive/zip"
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"bytes"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	qrcode "github.com/skip2/go-qrcode"
	"html"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var projectKeys = map[string]string{"Title": "title", "Description": "description", "Designer": "designer", "DesignerUserId": "designer_user_id", "License": "license", "Copyright": "copyright", "CreationDate": "creation_date", "ModificationDate": "modification_date", "Origin": "origin", "ProfileTitle": "profile_title", "ProfileDescription": "profile_description", "ProfileCover": "profile_cover", "ProfileUserId": "profile_user_id", "ProfileUserName": "profile_user_name", "DesignModelId": "design_model_id", "DesignProfileId": "design_profile_id", "DesignRegion": "design_region"}

func projectImageKind(name string) string {
	if path.Clean(name) != name || strings.Contains(name, "\\") {
		return ""
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif":
	default:
		return ""
	}
	for folder, kind := range map[string]string{"Auxiliaries/Model Pictures/": "model_pictures", "Auxiliaries/Profile Pictures/": "profile_pictures", "Auxiliaries/.thumbnails/": "thumbnails"} {
		if strings.HasPrefix(name, folder) {
			return kind
		}
	}
	return ""
}
func (s *Service) registerProject(public, api *http.ServeMux) {
	base := "/api/v1/archives/{id}/"
	for _, p := range []string{"plate-preview", "qrcode"} {
		h := s.withMedia("archive", s.project)
		route := "GET " + base + p
		public.HandleFunc(route, h)
		if public != api {
			api.HandleFunc(route, s.project)
		}
	}
	for _, p := range []string{"GET project-image/{image...}", "GET project-page", "GET printer-media", "PATCH project-page", "GET file/{filename}"} {
		m, suffix, _ := strings.Cut(p, " ")
		api.HandleFunc(m+" "+base+suffix, s.project)
	}
}
func (s *Service) project(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	action := "read"
	if r.Method == "PATCH" {
		action = "update"
	}
	asset, e := production.MediaAsset(r.Context(), s.db, p, "archives", num(r.PathValue("id")), action)
	if e != nil {
		httpx.Error(w, 404, "Archive not found")
		return
	}
	id := num(asString(asset["id"]))
	if strings.HasSuffix(r.URL.Path, "/printer-media") {
		s.printerMedia(w, r, asset)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/qrcode") {
		size, _ := strconv.Atoi(r.URL.Query().Get("size"))
		if size == 0 {
			size = 200
		}
		if size < 64 || size > 1024 {
			httpx.Error(w, 422, "Choose a QR size from 64 to 1024")
			return
		}
		base, _ := access.Setting(r.Context(), s.db, "external_url", "")
		if !validBase(base) {
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			base = scheme + "://" + r.Host
		}
		qr, e := qrcode.Encode(strings.TrimRight(base, "/")+fmt.Sprintf("/api/v1/archives/%d/download", id), qrcode.Medium, size)
		if e != nil {
			httpx.Error(w, 422, "Cannot encode archive URL")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = w.Write(qr)
		return
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 503, "Archive storage unavailable")
		return
	}
	defer root.Close()
	rel := asString(asset["file_path"])
	f, e := root.Open(rel)
	if e != nil {
		httpx.Error(w, 404, "Archive file missing")
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		httpx.Error(w, 404, "Archive file unavailable")
		return
	}
	if r.PathValue("filename") != "" {
		name := filepath.Base(asString(asset["filename"]))
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		http.ServeContent(w, r, name, st.ModTime(), f)
		return
	}
	z, e := zip.NewReader(f, st.Size())
	if e != nil {
		httpx.Error(w, 422, "Archive is not a readable 3MF")
		return
	}
	if image := r.PathValue("image"); image != "" {
		if projectImageKind(image) != "" {
			for _, part := range z.File {
				if part.Name == image {
					sendArchiveImage(w, part)
					return
				}
			}
		}
		httpx.Error(w, 404, "Project image unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/plate-preview") {
		plate := num(r.URL.Query().Get("plate_id"))
		if plate == 0 {
			plate = num(asString(asset["plate_id"]))
		}
		prefer := []string{fmt.Sprintf("Metadata/plate_%d.png", max(1, plate)), "Metadata/plate_1.png", "Metadata/thumbnail.png"}
		for _, part := range z.File {
			if strings.HasPrefix(part.Name, "Metadata/plate_") && strings.HasSuffix(part.Name, ".png") && !strings.Contains(part.Name, "_small") {
				prefer = append(prefer, part.Name)
			}
		}
		for _, name := range prefer {
			for _, part := range z.File {
				if part.Name == name {
					sendArchiveImage(w, part)
					return
				}
			}
		}
		httpx.Error(w, 404, "No plate preview in this 3MF")
		return
	}
	if r.Method == "PATCH" {
		s.editProject(w, r, asset, z)
		return
	}
	httpx.JSON(w, 200, projectInfo(z, id))
}
func sendArchiveImage(w http.ResponseWriter, part *zip.File) {
	if part.UncompressedSize64 > 16<<20 {
		httpx.Error(w, 413, "Project image is too large")
		return
	}
	src, e := part.Open()
	if e != nil {
		httpx.Error(w, 404, "Image unavailable")
		return
	}
	defer src.Close()
	b, e := io.ReadAll(io.LimitReader(src, 16<<20+1))
	if e != nil || len(b) > 16<<20 {
		httpx.Error(w, 422, "Invalid image")
		return
	}
	typ := http.DetectContentType(b)
	if typ != "image/png" && typ != "image/jpeg" && typ != "image/gif" && typ != "image/webp" {
		httpx.Error(w, 422, "Invalid project image format")
		return
	}
	w.Header().Set("Content-Type", typ)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=60")
	_, _ = w.Write(b)
}
func projectInfo(z *zip.Reader, id int64) object {
	out := object{}
	for _, k := range projectKeys {
		out[k] = nil
	}
	for _, k := range []string{"model_pictures", "profile_pictures", "thumbnails"} {
		out[k] = []object{}
	}
	for _, part := range z.File {
		if k := projectImageKind(part.Name); k != "" {
			out[k] = append(out[k].([]object), object{"name": path.Base(part.Name), "path": part.Name, "url": fmt.Sprintf("/api/v1/archives/%d/project-image/%s", id, url.PathEscape(part.Name))})
		}
		if part.Name != "3D/3dmodel.model" {
			continue
		}
		src, e := part.Open()
		if e != nil {
			continue
		}
		d := xml.NewDecoder(io.LimitReader(src, 8<<20))
		for {
			tok, e := d.Token()
			if e != nil {
				break
			}
			start, ok := tok.(xml.StartElement)
			if !ok {
				continue
			}
			if start.Name.Local == "resources" || start.Name.Local == "build" {
				break
			}
			if start.Name.Local != "metadata" {
				continue
			}
			name := ""
			for _, a := range start.Attr {
				if a.Name.Local == "name" {
					name = a.Value
				}
			}
			var value string
			if d.DecodeElement(&value, &start) != nil {
				break
			}
			if k := projectKeys[name]; k != "" && value != "" {
				for n := 0; n < 2; n++ {
					value = html.UnescapeString(value)
				}
				out[k] = value
			}
		}
		src.Close()
	}
	return out
}

var metaTag = regexp.MustCompile(`(?s)<metadata\s+name=["']([a-zA-Z0-9_]+)["'][^>]*?(?:/>|>.*?</metadata\s*>)`)

func rewriteHeader(b []byte, fields map[string]string) ([]byte, error) {
	end := bytes.Index(b, []byte("<resources"))
	if end < 0 {
		end = bytes.Index(b, []byte("<build"))
	}
	if end < 0 {
		end = bytes.Index(b, []byte("</model>"))
	}
	if end < 0 {
		return nil, fmt.Errorf("3MF metadata prefix exceeds eight MiB")
	}
	prefix := metaTag.ReplaceAllFunc(b[:end], func(old []byte) []byte {
		match := metaTag.FindSubmatch(old)
		if _, ok := fields[string(match[1])]; ok {
			return nil
		}
		return old
	})
	var out bytes.Buffer
	out.Write(prefix)
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(&out, "<metadata name=\"%s\">", k)
		_ = xml.EscapeText(&out, []byte(fields[k]))
		out.WriteString("</metadata>\n")
	}
	out.Write(b[end:])
	return out.Bytes(), nil
}
func (s *Service) editProject(w http.ResponseWriter, r *http.Request, asset object, z *zip.Reader) {
	var input map[string]*string
	if !httpx.Decode(w, r, &input) {
		return
	}
	writable := map[string]bool{"title": true, "description": true, "designer": true, "license": true, "copyright": true, "profile_title": true, "profile_description": true}
	fields := map[string]string{}
	for k, v := range input {
		if !writable[k] {
			httpx.Error(w, 422, "Unsupported project page field")
			return
		}
		for name, field := range projectKeys {
			if field == k {
				if v == nil {
					fields[name] = ""
				} else {
					fields[name] = *v
				}
			}
		}
	}
	id := num(asString(asset["id"]))
	if len(fields) == 0 {
		httpx.JSON(w, 200, projectInfo(z, id))
		return
	}
	dir := filepath.Join(s.dir, "production", "objects")
	if e := os.MkdirAll(dir, 0700); e != nil {
		httpx.Error(w, 500, "Storage unavailable")
		return
	}
	tmp, e := os.CreateTemp(dir, ".project-*.3mf")
	if e != nil {
		httpx.Error(w, 500, "Cannot create edited archive")
		return
	}
	defer tmp.Close()
	defer os.Remove(tmp.Name())
	hash := sha256.New()
	out := zip.NewWriter(io.MultiWriter(tmp, hash))
	found := false
	for _, part := range z.File {
		if part.Name != "3D/3dmodel.model" {
			e = out.Copy(part)
		} else if !found {
			found = true
			var source io.ReadCloser
			source, e = part.Open()
			if e == nil {
				var prefix []byte
				prefix, e = io.ReadAll(io.LimitReader(source, 8<<20))
				if e == nil {
					prefix, e = rewriteHeader(prefix, fields)
				}
				var dest io.Writer
				if e == nil {
					h := part.FileHeader
					h.Flags &^= 8
					dest, e = out.CreateHeader(&h)
				}
				if e == nil {
					_, e = dest.Write(prefix)
				}
				if e == nil {
					_, e = io.Copy(dest, source)
				}
				source.Close()
			}
		} else {
			e = fmt.Errorf("Duplicate model entry")
		}
		if e != nil {
			break
		}
	}
	if er := out.Close(); e == nil {
		e = er
	}
	if e != nil || !found {
		httpx.Error(w, 422, "Cannot update this 3MF model")
		return
	}
	if e = tmp.Sync(); e != nil {
		httpx.Error(w, 500, "Cannot save model")
		return
	}
	st, e := tmp.Stat()
	if e != nil {
		httpx.Error(w, 500, "Cannot size model")
		return
	}
	key, _ := tokenSecret()
	rel := filepath.Join("production", "objects", key+".3mf")
	dest := filepath.Join(s.dir, rel)
	if e = os.Rename(tmp.Name(), dest); e != nil {
		httpx.Error(w, 500, "Cannot save edited model")
		return
	}
	result, e := s.db.ExecContext(r.Context(), "UPDATE print_archives SET file_path=?,file_size=?,content_hash=? WHERE id=? AND file_path=? AND deleted_at IS NULL", rel, st.Size(), fmt.Sprintf("%x", hash.Sum(nil)), id, asString(asset["file_path"]))
	if e != nil {
		_ = os.Remove(dest)
		httpx.Error(w, 500, "Cannot update project")
		return
	}
	if n, _ := result.RowsAffected(); n != 1 {
		_ = os.Remove(dest)
		httpx.Error(w, 409, "Project source changed")
		return
	}
	edited, e := zip.NewReader(tmp, st.Size())
	if e != nil {
		httpx.Error(w, 500, "Cannot reopen edited model")
		return
	}
	httpx.JSON(w, 200, projectInfo(edited, id))
}
func (s *Service) printerMedia(w http.ResponseWriter, r *http.Request, asset object) {
	ctx := r.Context()
	id, pid := num(asString(asset["id"])), num(asString(asset["printer_id"]))
	p := access.From(ctx)
	warnings := []string{}
	files := []object{}
	out := object{"archive_id": id, "printer_id": nil, "local_timelapse": nil}
	defer func() { out["warnings"] = warnings; out["remote_files"] = files; httpx.JSON(w, 200, out) }()
	rel := asString(asset["timelapse_path"])
	if filepath.IsLocal(rel) && s.mediaVisible(r, p, id, rel, pid) {
		if root, e := os.OpenRoot(s.dir); e == nil {
			st, e := root.Stat(rel)
			root.Close()
			if e == nil && st.Mode().IsRegular() {
				out["local_timelapse"] = object{"name": path.Base(rel), "size": st.Size()}
			}
		}
	}
	if pid <= 0 || !p.AllowsPrinter(pid) {
		return
	}
	out["printer_id"] = pid
	if !p.Has("printers:files") {
		warnings = append(warnings, "printer_files_forbidden")
		return
	}
	farm, ok := s.farm.(fileFarm)
	if !ok {
		warnings = append(warnings, "printer_unavailable")
		return
	}
	if out["local_timelapse"] == nil {
		reachable := false
		list := []printerVideo{}
		for _, dir := range []string{"/timelapse", "/timelapse/video", "/record", "/recording"} {
			vs, e := videoListing(ctx, farm, p, pid, dir)
			if e == nil {
				reachable = true
				list = append(list, vs...)
			}
		}
		if !reachable {
			warnings = append(warnings, "timelapse_unavailable")
		} else if selected, _ := selectVideo(list, nil, asset); selected != nil {
			files = append(files, object{"source": "timelapse", "kind": "timelapse", "path": selected.Path, "name": selected.Name, "size": selected.Size, "mtime": selected.Mtime})
		}
	}
	entries, e := farm.ListPrinterFiles(ctx, p, pid, "/ipcam")
	if e != nil {
		warnings = append(warnings, "ipcam_unavailable")
		return
	}
	start, end := mediaTime(asset["started_at"]), mediaTime(asset["completed_at"])
	if start.IsZero() || end.IsZero() {
		return
	}
	for _, v := range entries {
		t := mediaTime(v["mtime"])
		name := asString(v["name"])
		if t.Before(start.Add(-15*time.Minute)) || t.After(end.Add(15*time.Minute)) || strings.ContainsAny(name, "/\\") || v["is_dir"] == true {
			continue
		}
		if ext := strings.ToLower(path.Ext(name)); ext != ".mp4" && ext != ".avi" && ext != ".mkv" {
			continue
		}
		files = append(files, object{"source": "ipcam", "kind": "ipcam", "name": name, "path": path.Join("/ipcam", name), "size": v["size"], "mtime": v["mtime"]})
	}
}
