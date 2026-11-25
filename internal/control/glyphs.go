package control

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/openbridge"
	"database/sql"
	"io"
	"net/http"
	"strings"
)

func (s *Service) linkIcon(w http.ResponseWriter, r *http.Request) {
	perm := "external_links:read"
	if r.Method != "GET" {
		perm = "external_links:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	id := r.PathValue("id")
	var old string
	var custom sql.NullString
	if e := s.DB.QueryRowContext(r.Context(), "SELECT icon,custom_icon FROM external_links WHERE id=?", id).Scan(&old, &custom); e != nil {
		httpx.Error(w, 404, "External link not found")
		return
	}
	if r.Method == "GET" {
		name := openbridge.Name(old)
		if v := openbridge.Name(custom.String); v != "" {
			name = v
		}
		if name == "" {
			name = "link"
		}
		openbridge.Serve(w, r, name)
		return
	}
	name := "link"
	var override any
	if r.Method == "POST" {
		r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
		if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
			f, _, e := r.FormFile("file")
			if r.MultipartForm != nil {
				defer r.MultipartForm.RemoveAll()
			}
			if e != nil {
				httpx.Error(w, 422, "Choose a registered OpenBridge icon")
				return
			}
			defer f.Close()
			b, e := io.ReadAll(io.LimitReader(f, 65537))
			if e != nil || len(b) > 65536 {
				httpx.Error(w, 413, "Icon exceeds limit")
				return
			}
			name = openbridge.Match(b)
		} else {
			var in struct{ Icon string }
			if !httpx.Decode(w, r, &in) {
				return
			}
			name = openbridge.Name(in.Icon)
		}
		if name == "" {
			httpx.Error(w, 422, "Choose a registered OpenBridge icon")
			return
		}
		override = "openbridge:" + name
	}
	if _, e := s.DB.ExecContext(r.Context(), "UPDATE external_links SET icon=?,custom_icon=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", name, override, id); e != nil {
		httpx.Error(w, 500, "Cannot update link icon")
		return
	}
	s.sendLinks(w, r, id, 200)
}
