package production

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"bambastic/internal/access"
	"github.com/jlaffaye/ftp"
)

type card interface {
	List(string) ([]*ftp.Entry, error)
	Open(string) (io.ReadCloser, error)
	Stor(string, io.Reader) error
	Delete(string) error
	Rename(string, string) error
	FileSize(string) (int64, error)
	Quit() error
}

type lanCard struct{ *ftp.ServerConn }

func (c lanCard) Open(name string) (io.ReadCloser, error) { return c.Retr(name) }
func cardPath(name string, dir bool) (string, error) {
	if name == "" && dir {
		name = "/"
	}
	if !strings.HasPrefix(name, "/") || len(name) > 1024 || strings.ContainsAny(name, "\\\x00\r\n") {
		return "", fail(422, "An absolute printer storage path is required")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." || part == "." {
			return "", fail(422, "Invalid printer storage path")
		}
	}
	name = path.Clean(name)
	if !dir && name == "/" {
		return "", fail(422, "A file path is required")
	}
	return name, nil
}
func (s *Service) card(ctx context.Context, p access.Principal, k int64) (card, error) {
	if !p.Has("printers:files") {
		return nil, fail(403, "Printer file access required")
	}
	if !p.AllowsPrinter(k) {
		return nil, sql.ErrNoRows
	}
	return s.trustedCard(ctx, k)
}
func (s *Service) trustedCard(ctx context.Context, k int64) (card, error) {
	pr, e := one(ctx, s.db, "SELECT p.*,t.fingerprint FROM printers p JOIN bb_printer_trust t ON t.printer_id=p.id WHERE p.id=?", k)
	if e != nil {
		return nil, e
	}
	if s.openCard != nil {
		return s.openCard(ctx, pr)
	}
	device := lan{printer: pr, fingerprint: val(pr["fingerprint"])}
	c, e := device.files(ctx)
	if e != nil {
		return nil, fail(503, "Printer storage unavailable")
	}
	return lanCard{c}, nil
}

// ListPrinterFiles reads a printer's storage through its enrolled, pinned TLS connection.
func (s *Service) ListPrinterFiles(ctx context.Context, p access.Principal, k int64, folder string) ([]map[string]any, error) {
	folder, e := cardPath(folder, true)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	c, e := s.card(ctx, p, k)
	if e != nil {
		return nil, e
	}
	defer c.Quit()
	entries, e := c.List(folder)
	if e != nil {
		return nil, fail(503, "Cannot list printer directory")
	}
	out := []map[string]any{}
	for _, v := range entries {
		if !validFileName(v.Name) || v.Type == ftp.EntryTypeLink {
			continue
		}
		mtime := any(nil)
		if !v.Time.IsZero() {
			mtime = v.Time.UTC().Format(time.RFC3339)
		}
		out = append(out, map[string]any{"name": v.Name, "path": path.Join(folder, v.Name), "is_dir": v.Type == ftp.EntryTypeFolder, "is_directory": v.Type == ftp.EntryTypeFolder, "size": v.Size, "mtime": mtime, "modified": mtime})
	}
	return out, nil
}
func (s *Service) ReadPrinterFile(ctx context.Context, p access.Principal, k int64, name string, w io.Writer, limit int64) error {
	name, e := cardPath(name, false)
	if e != nil {
		return e
	}
	if limit < 1 {
		return fail(413, "Download exceeds size limit")
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	c, e := s.card(ctx, p, k)
	if e != nil {
		return e
	}
	defer c.Quit()
	expected, sizeErr := c.FileSize(name)
	if sizeErr == nil && expected > limit {
		return fail(413, "Printer file exceeds size limit")
	}
	resp, e := c.Open(name)
	if e != nil {
		return fail(404, "Printer file unavailable")
	}
	defer func() {
		if resp != nil {
			resp.Close()
		}
	}()
	n, e := io.Copy(w, io.LimitReader(resp, limit))
	if e != nil {
		return e
	}
	if n == limit {
		var one [1]byte
		n, e := resp.Read(one[:])
		if n > 0 {
			return fail(413, "Printer file exceeds size limit")
		}
		if e != nil && !errors.Is(e, io.EOF) {
			return e
		}
	}
	e = resp.Close()
	resp = nil
	if e == nil && sizeErr == nil && n != expected {
		return fail(502, "Printer file changed during transfer")
	}
	return e
}
func (s *Service) storageRoutes(rt register) {
	for _, a := range []string{"download", "plates", "gcode"} {
		rt("GET", "/printers/{id}/files/"+a, s.storage(a))
	}
	rt("GET", "/printers/{id}/files/plate-thumbnail/{plate}", s.storage("thumbnail"))
	rt("GET", "/printers/{id}/files", s.storage("list"))
	rt("POST", "/printers/{id}/files", s.storage("upload"))
	rt("DELETE", "/printers/{id}/files", s.storage("delete"))
	rt("POST", "/printers/{id}/files/download-zip", s.storage("zip"))
	rt("GET", "/printers/{id}/storage", s.cardInfo)
	rt("GET", "/printers/{id}/current-print-user", s.currentUser)
	for _, a := range []string{"connect", "disconnect"} {
		rt("POST", "/printers/{id}/"+a, s.connection(a))
	}
}
func (s *Service) storage(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		k := pathID(r)
		name := r.URL.Query().Get("path")
		if !p.Has("printers:files") {
			return nil, fail(403, "Printer file access required")
		}
		if !p.AllowsPrinter(k) {
			return nil, sql.ErrNoRows
		}
		switch action {
		case "list":
			if name == "" {
				name = "/"
			}
			entries, e := s.ListPrinterFiles(ctx, p, k, name)
			if e != nil {
				return nil, e
			}
			return row{"path": name, "files": entries, "warnings": []string{}}, nil
		case "upload", "delete":
			return s.changeFile(w, r, action)
		}
		temp, e := os.CreateTemp(filepath.Join(s.dir, "production"), "download-*")
		if e != nil {
			return nil, e
		}
		defer os.Remove(temp.Name())
		defer temp.Close()
		if action == "zip" {
			m, e := input(w, r)
			if e != nil {
				return nil, e
			}
			ps, ok := m["paths"].([]any)
			if !ok || len(ps) == 0 || len(ps) > 1000 {
				return nil, fail(422, "Select up to 1000 files")
			}
			names := map[string]bool{}
			z := zip.NewWriter(temp)
			var remaining int64 = 1 << 30
			for _, x := range ps {
				n, e := cardPath(val(x), false)
				if e != nil {
					return nil, e
				}
				if names[n] {
					continue
				}
				names[n] = true
				w, e := z.Create(strings.TrimPrefix(n, "/"))
				if e != nil {
					return nil, e
				}
				count := &meter{dest: w}
				if e = s.ReadPrinterFile(ctx, p, k, n, count, remaining); e != nil {
					return nil, e
				}
				remaining -= count.n
			}
			if e = z.Close(); e != nil {
				return nil, e
			}
			name = "printer-files.zip"
		} else if e = s.ReadPrinterFile(ctx, p, k, name, temp, 1<<30); e != nil {
			return nil, e
		}
		if _, e = temp.Seek(0, 0); e != nil {
			return nil, e
		}
		if action == "download" || action == "zip" {
			w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(name)}))
			http.ServeContent(w, r, path.Base(name), time.Time{}, temp)
			return nil, nil
		}
		inf, e := inspect(temp.Name(), name)
		if e != nil {
			return nil, e
		}
		if action == "plates" {
			return row{"plates": inf.Plates, "has_gcode": inf.Printable, "filename": path.Base(name)}, nil
		}
		index := num(r.PathValue("plate"))
		if index == 0 {
			index = num(r.URL.Query().Get("plate"))
		}
		if index == 0 {
			for _, pl := range inf.Plates {
				if pl.Printable {
					index = pl.Index
					break
				}
			}
		}
		if action == "gcode" && strings.EqualFold(path.Ext(name), ".gcode") {
			w.Header().Set("Content-Type", "text/plain")
			http.ServeContent(w, r, name, time.Time{}, temp)
			return nil, nil
		}
		z, e := zip.OpenReader(temp.Name())
		if e != nil {
			return nil, fail(422, "Unsupported printer file")
		}
		defer z.Close()
		suffix := "gcode"
		mimeType := "text/plain; charset=utf-8"
		maxBytes := uint64(1 << 30)
		if action == "thumbnail" {
			suffix = "png"
			mimeType = "image/png"
			maxBytes = 8 << 20
		}
		for _, f := range z.File {
			if f.Name != fmt.Sprintf("Metadata/plate_%d.%s", index, suffix) || f.UncompressedSize64 > maxBytes {
				continue
			}
			rd, e := f.Open()
			if e != nil {
				return nil, e
			}
			defer rd.Close()
			w.Header().Set("Content-Type", mimeType)
			_, e = io.Copy(w, io.LimitReader(rd, int64(maxBytes)))
			return nil, e
		}
		return nil, sql.ErrNoRows
	}
}

type meter struct {
	dest io.Writer
	n    int64
}

func (m *meter) Write(p []byte) (int, error) { n, e := m.dest.Write(p); m.n += int64(n); return n, e }
func (s *Service) changeFile(w http.ResponseWriter, r *http.Request, action string) (any, error) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	p := access.From(ctx)
	k := pathID(r)
	name := r.URL.Query().Get("path")
	if action == "delete" {
		name, e := cardPath(name, false)
		if e != nil {
			return nil, e
		}
		c, e := s.card(ctx, p, k)
		if e != nil {
			return nil, e
		}
		defer c.Quit()
		if e = c.Delete(name); e != nil {
			return nil, fail(409, "Printer did not delete the file")
		}
		return row{"success": true, "path": name}, nil
	}
	folder, e := cardPath(name, true)
	if e != nil {
		return nil, e
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<30)
	if e = r.ParseMultipartForm(8 << 20); e != nil {
		return nil, fail(422, "Invalid or oversized upload")
	}
	defer r.MultipartForm.RemoveAll()
	file, h, e := r.FormFile("file")
	if e != nil {
		return nil, fail(422, "Select a file")
	}
	defer file.Close()
	if !validFileName(h.Filename) {
		return nil, fail(422, "Invalid filename")
	}
	name = path.Join(folder, h.Filename)
	c, e := s.card(ctx, p, k)
	if e != nil {
		return nil, e
	}
	defer c.Quit()
	if _, e = c.FileSize(name); e == nil {
		return nil, fail(409, "A printer file already has that name")
	}
	tmp := path.Join(folder, "bambastic-upload-"+uuid()+".part")
	defer c.Delete(tmp)
	if e = c.Stor(tmp, file); e != nil {
		return nil, fail(502, "Printer refused file transfer")
	}
	n, e := c.FileSize(tmp)
	if e != nil || n != h.Size {
		return nil, fail(502, "Printer file size did not verify")
	}
	if e = c.Rename(tmp, name); e != nil {
		return nil, fail(502, "Printer could not publish uploaded file")
	}
	return row{"success": true, "path": name, "size": n}, nil
}
func (s *Service) cardInfo(w http.ResponseWriter, r *http.Request) (any, error) {
	pr, e := s.printerRow(r, "printers:read")
	if e != nil {
		return nil, e
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := row{"used_bytes": nil, "free_bytes": nil}
	if d := s.devices[num(pr["id"])]; d != nil {
		if obj, ok := d.report["storage"].(map[string]any); ok {
			out["used_bytes"] = obj["used_bytes"]
			out["free_bytes"] = obj["free_bytes"]
		}
	}
	return out, nil
}
func (s *Service) currentUser(w http.ResponseWriter, r *http.Request) (any, error) {
	if _, e := s.printerRow(r, "printers:read"); e != nil {
		return nil, e
	}
	run, e := one(r.Context(), s.db, "SELECT l.created_by_id user_id,u.username FROM print_log_entries l JOIN users u ON u.id=l.created_by_id WHERE l.printer_id=? AND l.completed_at IS NULL ORDER BY l.id DESC LIMIT 1", pathID(r))
	if errors.Is(e, sql.ErrNoRows) {
		return row{}, nil
	}
	return run, e
}
func (s *Service) connection(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		pr, e := s.printerRow(r, "printers:control")
		if e != nil {
			return nil, e
		}
		on := action == "connect"
		if on {
			trust, e := one(r.Context(), s.db, "SELECT fingerprint FROM bb_printer_trust WHERE printer_id=?", pr["id"])
			if e != nil {
				return nil, e
			}
			ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			defer cancel()
			h, e := s.open(ctx, pr, val(trust["fingerprint"]), nil)
			if e != nil {
				return nil, fail(503, "Printer connection failed")
			}
			h.Close()
		}
		if _, e = s.db.ExecContext(r.Context(), "UPDATE printers SET is_active=? WHERE id=?", on, pr["id"]); e != nil {
			return nil, e
		}
		s.kick()
		return row{"connected": on}, nil
	}
}
