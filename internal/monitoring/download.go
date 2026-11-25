package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type download struct {
	kind, field, prefix, mint, redeem string
	singleUse                         bool
}

func (s *Service) registerDownloads(public, api *http.ServeMux) {
	for _, d := range []download{
		{"archives", "file_path", "archives", "slicer-token", "dl", false},
		{"archives", "source_3mf_path", "archives", "source-slicer-token", "source-dl", false},
		{"archives", "timelapse_path", "archives", "media-download-token", "media/dl", true},
		{"library", "file_path", "library/files", "slicer-token", "dl", false},
	} {
		base := "/api/v1/" + d.prefix + "/{id}/"
		api.HandleFunc("POST "+base+d.mint, func(w http.ResponseWriter, r *http.Request) { s.downloadLink(w, r, d, true) })
		route := "GET " + base + d.redeem + "/{token}/{filename}"
		h := func(w http.ResponseWriter, r *http.Request) { s.downloadLink(w, r, d, false) }
		public.HandleFunc(route, h)
		if public != api {
			api.HandleFunc(route, h)
		}
	}
}
func (s *Service) downloadLink(w http.ResponseWriter, r *http.Request, d download, mint bool) {
	ctx := r.Context()
	id := num(r.PathValue("id"))
	// Resolve the identifier to its current contents. The digest binds the token to
	// the selected attachment, including replacements stored under the same name.
	table := "print_archives"
	if d.kind == "library" {
		table = "library_files"
	}
	var rel sql.NullString
	err := s.db.QueryRowContext(ctx, "SELECT "+d.field+" FROM "+table+" WHERE id=? AND deleted_at IS NULL", id).Scan(&rel)
	root, openErr := os.OpenRoot(s.dir)
	if openErr != nil {
		httpx.Error(w, 503, "Storage unavailable")
		return
	}
	defer root.Close()
	var f *os.File
	var st os.FileInfo
	if err == nil && filepath.IsLocal(rel.String) {
		f, err = root.Open(rel.String)
		if err == nil {
			defer f.Close()
			st, err = f.Stat()
		}
	}
	bad := func() {
		if mint {
			httpx.Error(w, 404, "File unavailable")
		} else {
			httpx.Error(w, 403, "Invalid or expired download link")
		}
	}
	if err != nil || st == nil || !st.Mode().IsRegular() {
		bad()
		return
	}
	scope := fmt.Sprintf("download/%s/%s/%d/%x", d.kind, d.field, id, sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%d", rel.String, st.Size(), st.ModTime().UnixNano()))))
	p := access.From(ctx)
	token := r.PathValue("token")
	if !mint {
		if token == "" {
			bad()
			return
		}
		p, err = s.auth.Resolve(ctx, token, scope)
		if err != nil {
			bad()
			return
		}
	}
	asset, err := production.MediaAsset(ctx, s.db, p, d.kind, id, "read")
	if err != nil || asString(asset[d.field]) != rel.String {
		bad()
		return
	}
	if d.singleUse && !s.mediaVisible(r, p, id, rel.String, num(asString(asset["printer_id"]))) {
		bad()
		return
	}
	filename := asString(asset["filename"])
	if d.field != "file_path" {
		filename = filepath.Base(rel.String)
	}
	if mint {
		token, err = s.auth.Mint(ctx, p, scope, 5*time.Minute, access.Credential(r))
		if err != nil {
			httpx.Error(w, 500, "Cannot issue download link")
			return
		}
		response := object{"token": token}
		if d.singleUse {
			response["filename"] = filename
		}
		httpx.JSON(w, 200, response)
		return
	}
	if d.singleUse && !s.auth.Consume(ctx, token, scope) {
		bad()
		return
	}
	filename = filepath.Base(strings.ReplaceAll(filename, "\\", "/"))
	if t := mime.TypeByExtension(filepath.Ext(filename)); t != "" {
		w.Header().Set("Content-Type", t)
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filename, st.ModTime(), f)
}
func asString(v any) string {
	if v == nil {
		return ""
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return fmt.Sprint(v)
}

// Latest-archive media for an out-of-scope run must not be visible through an
// archive that is otherwise accessible through an older in-scope run.
func (s *Service) mediaVisible(r *http.Request, p access.Principal, id int64, media string, direct int64) bool {
	if p.PrinterIDs == nil {
		return true
	}
	rows, err := s.db.QueryContext(r.Context(), "SELECT printer_id FROM print_log_entries WHERE archive_id=? AND timelapse_path=? AND printer_id IS NOT NULL", id, media)
	if err != nil {
		return false
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var k int64
		if rows.Scan(&k) != nil {
			return false
		}
		found = true
		if p.AllowsPrinter(k) {
			return true
		}
	}
	return rows.Err() == nil && !found && (direct == 0 || p.AllowsPrinter(direct))
}
