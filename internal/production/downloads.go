package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

func (s *Service) RegisterDownloads(public *http.ServeMux, auth *access.Service) {
	s.auth = auth
	for _, method := range []string{"GET", "POST"} {
		public.HandleFunc(method+" /api/v1/archives/confirm/{token}/{verdict}", wrap(s.confirmOutcome))
	}
	public.HandleFunc("GET /api/v1/printers/{id}/files/dl/{token}/{filename}", wrap(s.downloadResult))
}
func (s *Service) jobRoutes(rt register) {
	rt("POST", "/printers/{id}/files/download-job", s.cardJob)
	rt("GET", "/printers/{id}/files/download-jobs/{job}", s.cardJob)
	rt("DELETE", "/printers/{id}/files/download-jobs/{job}", s.cardJob)
}
func tokenHash(v string) string { sum := sha256.Sum256([]byte(v)); return hex.EncodeToString(sum[:]) }
func (s *Service) cardJob(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	if _, e := s.printerRow(r, "printers:files"); e != nil {
		return nil, e
	}
	if s.auth == nil {
		return nil, fail(503, "Download service unavailable")
	}
	key := r.PathValue("job")
	if r.Method == "POST" {
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		items, ok := m["paths"].([]any)
		if !ok || len(items) == 0 || len(items) > 1000 {
			return nil, fail(422, "Select up to 1000 files")
		}
		names := []string{}
		seen := map[string]bool{}
		for _, v := range items {
			n, e := cardPath(val(v), false)
			if e != nil {
				return nil, e
			}
			if !seen[n] {
				names = append(names, n)
				seen[n] = true
			}
		}
		zipRequested := true
		if a, ok := m["as_zip"]; ok {
			zipRequested = flag(a)
		}
		if !zipRequested && len(names) != 1 {
			return nil, fail(422, "Select one file or request a ZIP")
		}
		filename := val(m["filename"])
		if filename == "" {
			filename = "printer-files.zip"
			if !zipRequested {
				filename = path.Base(names[0])
			}
		}
		if !validFileName(filename) {
			return nil, fail(422, "Invalid download filename")
		}
		token, e := s.auth.Mint(ctx, p, "printer-files-worker", time.Hour, "")
		if e != nil {
			return nil, e
		}
		key = uuid()
		res, e := s.db.ExecContext(ctx, "INSERT INTO bb_file_jobs(job_id,printer_id,owner,key_id,paths,filename,as_zip,state,requested,credential,expires) SELECT ?,?,?,?,?,?,?,'queued',?,?,unixepoch()+3600 WHERE (SELECT count(*) FROM bb_file_jobs WHERE owner=? AND key_id=? AND state IN ('queued','preparing') AND expires>unixepoch()) < 8", key, k, p.ID, p.KeyID, jstr(names), filename, zipRequested, len(names), token, p.ID, p.KeyID)
		if e != nil {
			return nil, e
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, fail(429, "Too many active downloads")
		}
	}
	j, e := one(ctx, s.db, "SELECT * FROM bb_file_jobs WHERE job_id=? AND printer_id=? AND owner=? AND key_id=? AND expires>unixepoch()", key, k, p.ID, p.KeyID)
	if e != nil {
		return nil, e
	}
	if r.Method == "DELETE" {
		_, e = s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET state='cancelled',credential=NULL WHERE job_id=?", key)
		if e != nil {
			return nil, e
		}
		s.mu.Lock()
		if stop := s.jobStops[key]; stop != nil {
			stop()
		}
		s.mu.Unlock()
		os.Remove(s.jobPath(key))
		return row{"status": "cancelled"}, nil
	}
	out := row{}
	for _, field := range []string{"job_id", "state", "message", "filename", "requested", "successful", "failed"} {
		out[field] = j[field]
	}
	out["token"] = nil
	if j["state"] == "ready" {
		token, e := s.auth.Mint(ctx, p, "printer-files-download", 10*time.Minute, "")
		if e != nil {
			return nil, e
		}
		_, e = s.db.ExecContext(ctx, "INSERT INTO bb_file_tokens(hash,job_id,expires) VALUES(?,?,unixepoch()+600)", tokenHash(token), key)
		if e != nil {
			return nil, e
		}
		out["token"] = token
	}
	return out, nil
}
func (s *Service) jobPath(k string) string { return filepath.Join(s.dir, "production", "download-"+k) }
func (s *Service) downloadResult(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	token := r.PathValue("token")
	p, e := s.auth.Resolve(ctx, token, "printer-files-download")
	if e != nil || !p.Has("printers:files") {
		return nil, fail(401, "Download authorization expired")
	}
	k := pathID(r)
	if !p.AllowsPrinter(k) {
		return nil, sql.ErrNoRows
	}
	j, e := one(ctx, s.db, "SELECT j.* FROM bb_file_tokens t JOIN bb_file_jobs j ON j.job_id=t.job_id WHERE t.hash=? AND t.expires>unixepoch() AND j.expires>unixepoch() AND j.state='ready' AND j.printer_id=? AND j.owner=? AND j.key_id=? AND j.filename=?", tokenHash(token), k, p.ID, p.KeyID, r.PathValue("filename"))
	if e != nil {
		return nil, e
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": val(j["filename"])}))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeFile(w, r, s.jobPath(val(j["job_id"])))
	return nil, nil
}
func (s *Service) downloadLoop(ctx context.Context) {
	s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET state='queued',successful=0,failed=0 WHERE state='preparing'")
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		expired, e := records(ctx, s.db, "SELECT job_id FROM bb_file_jobs WHERE expires<=unixepoch()")
		if e != nil {
			continue
		}
		for _, j := range expired {
			os.Remove(s.jobPath(val(j["job_id"])))
		}
		s.db.ExecContext(ctx, "DELETE FROM bb_file_jobs WHERE expires<=unixepoch()")
		s.db.ExecContext(ctx, "DELETE FROM bb_file_tokens WHERE expires<=unixepoch()")
		j, e := one(ctx, s.db, "SELECT * FROM bb_file_jobs WHERE state='queued' ORDER BY expires LIMIT 1")
		if e == nil {
			s.prepareDownload(ctx, j)
		}
	}
}
func (s *Service) prepareDownload(parent context.Context, j row) {
	if s.auth == nil {
		return
	}
	job := val(j["job_id"])
	ctx, cancel := context.WithCancel(parent)
	s.mu.Lock()
	s.jobStops[job] = cancel
	s.mu.Unlock()
	defer func() { cancel(); s.mu.Lock(); delete(s.jobStops, job); s.mu.Unlock() }()
	res, e := s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET state='preparing',successful=0,failed=0 WHERE job_id=? AND state='queued'", job)
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return
	}
	p, e := s.auth.Resolve(ctx, val(j["credential"]), "printer-files-worker")
	if e != nil {
		s.failDownload(parent, job, "Download authorization expired")
		return
	}
	tmp, e := os.CreateTemp(filepath.Join(s.dir, "production"), "building-*")
	if e != nil {
		s.failDownload(parent, job, "Storage unavailable")
		return
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	names := []string{}
	json.Unmarshal([]byte(val(j["paths"])), &names)
	var z *zip.Writer
	if flag(j["as_zip"]) {
		z = zip.NewWriter(tmp)
	}
	var budget int64 = 1 << 30
	good, bad := 0, 0
	for _, name := range names {
		if ctx.Err() != nil {
			return
		}
		staged, e := os.CreateTemp(filepath.Join(s.dir, "production"), "member-*")
		if e != nil {
			s.failDownload(parent, job, "Storage unavailable")
			return
		}
		e = s.ReadPrinterFile(ctx, p, num(j["printer_id"]), name, staged, budget)
		if e == nil {
			_, e = staged.Seek(0, 0)
		}
		if e != nil {
			staged.Close()
			os.Remove(staged.Name())
			bad++
		} else {
			var dest io.Writer = tmp
			if z != nil {
				dest, e = z.Create(strings.TrimPrefix(name, "/"))
			}
			if e == nil {
				var copied int64
				copied, e = io.Copy(dest, staged)
				budget -= copied
			}
			staged.Close()
			os.Remove(staged.Name())
			if e != nil {
				s.failDownload(parent, job, "Could not assemble archive")
				return
			}
			good++
		}
		s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET successful=?,failed=? WHERE job_id=? AND state='preparing'", good, bad, job)
	}
	if good == 0 {
		s.failDownload(parent, job, "None of the requested files could be retrieved")
		return
	}
	if z != nil {
		if e = z.Close(); e != nil {
			s.failDownload(parent, job, "Could not finalize ZIP")
			return
		}
	}
	if e = tmp.Close(); e != nil {
		s.failDownload(parent, job, "Storage unavailable")
		return
	}
	if e = os.Rename(tmp.Name(), s.jobPath(job)); e != nil {
		s.failDownload(parent, job, "Storage unavailable")
		return
	}
	res, e = s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET state='ready',credential=NULL WHERE job_id=? AND state='preparing'", job)
	if e != nil {
		os.Remove(s.jobPath(job))
		return
	}
	n, _ = res.RowsAffected()
	if n == 0 {
		os.Remove(s.jobPath(job))
	}
}
func (s *Service) failDownload(ctx context.Context, job, msg string) {
	s.db.ExecContext(ctx, "UPDATE bb_file_jobs SET state='failed',credential=NULL,message=? WHERE job_id=? AND state='preparing'", msg, job)
}
