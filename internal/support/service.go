// Package support records bounded, redacted diagnostics for the operator support panel.
package support

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type Service struct {
	db       *sql.DB
	debug    atomic.Bool
	messages chan entry
}
type entry struct{ Logger, Level, Message string }

func New(db *sql.DB) (*Service, error) {
	_, e := db.Exec(`CREATE TABLE IF NOT EXISTS bambastic_diagnostics(id INTEGER PRIMARY KEY, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP NOT NULL, level TEXT NOT NULL,logger_name TEXT NOT NULL,message TEXT NOT NULL);CREATE TRIGGER IF NOT EXISTS bambastic_limit_log AFTER INSERT ON bambastic_diagnostics WHEN NEW.id%100=0 BEGIN DELETE FROM bambastic_diagnostics WHERE id<=NEW.id-5000;END;`)
	if e != nil {
		return nil, e
	}
	s := &Service{db: db, messages: make(chan entry, 256)}
	debug, e := access.Setting(context.Background(), db, "debug_logging_enabled", "false")
	s.debug.Store(debug == "true")
	return s, e
}
func (s *Service) put(logger, level, msg string) {
	select {
	case s.messages <- entry{logger, level, msg}:
	default:
	}
}
func (s *Service) Domain(event map[string]any) {
	kind, _ := event["type"].(string)
	if kind == "" || kind == "printer_status" && !s.debug.Load() {
		return
	}
	// Domain event names are emitted by application code. Payloads can contain private filenames.
	s.put("workflows", "INFO", kind)
}
func (s *Service) Request(r *http.Request) {
	if !s.debug.Load() || strings.HasPrefix(r.URL.Path, "/api/v1/support/") || !strings.HasPrefix(r.URL.Path, "/api/") {
		return
	}
	// Only record the API family, never query strings, user-supplied names or IDs.
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	family, _, _ := strings.Cut(path, "/")
	if len(family) > 40 {
		return
	}
	for _, c := range family {
		if (c < 'a' || c > 'z') && c != '-' && c != '2' {
			return
		}
	}
	s.put("http", "DEBUG", r.Method+" /api/v1/"+family)
}
func (s *Service) Run(ctx context.Context) {
	s.put("bambastic", "INFO", "Service started")
	for {
		select {
		case <-ctx.Done():
			return
		case msg := <-s.messages:
			s.db.ExecContext(ctx, "INSERT INTO bambastic_diagnostics(level,logger_name,message) VALUES(?,?,?)", msg.Level, msg.Logger, msg.Message)
		}
	}
}
func (s *Service) Register(api *http.ServeMux) {
	s.bugRoutes(api)
	for _, route := range []string{"GET /api/v1/support/debug-logging", "POST /api/v1/support/debug-logging", "GET /api/v1/support/logs", "DELETE /api/v1/support/logs", "GET /api/v1/support/bundle"} {
		api.HandleFunc(route, s.serve)
	}
}
func (s *Service) serve(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	if !p.Admin {
		httpx.Error(w, 403, "Administrator required for support diagnostics")
		return
	}
	ctx := r.Context()
	base := "/api/v1/support/"
	switch strings.TrimPrefix(r.URL.Path, base) {
	case "debug-logging":
		if r.Method == "POST" {
			var v struct{ Enabled *bool }
			if !httpx.Decode(w, r, &v) {
				return
			}
			if v.Enabled == nil {
				httpx.Error(w, 422, "Enabled required")
				return
			}
			tx, e := s.db.Begin()
			if e != nil {
				httpx.Error(w, 500, "Cannot save debug preference")
				return
			}
			defer tx.Rollback()
			since := ""
			if *v.Enabled {
				since = time.Now().UTC().Format(time.RFC3339)
			}
			if e = access.Put(tx, "debug_logging_enabled", strconv.FormatBool(*v.Enabled)); e == nil {
				e = access.Put(tx, "debug_logging_enabled_at", since)
			}
			if e == nil {
				e = tx.Commit()
			}
			if e != nil {
				httpx.Error(w, 500, "Cannot save debug preference")
				return
			}
			s.debug.Store(*v.Enabled)
		}
		var since, duration any
		if s.debug.Load() {
			v, _ := access.Setting(ctx, s.db, "debug_logging_enabled_at", "")
			if t, e := time.Parse(time.RFC3339, v); e == nil {
				since = t
				duration = int(time.Since(t).Seconds())
			}
		}
		httpx.JSON(w, 200, map[string]any{"enabled": s.debug.Load(), "enabled_at": since, "duration_seconds": duration})
	case "logs":
		if r.Method == "DELETE" {
			_, e := s.db.ExecContext(ctx, "DELETE FROM bambastic_diagnostics")
			if e != nil {
				httpx.Error(w, 500, "Could not clear logs")
				return
			}
			httpx.JSON(w, 200, map[string]string{"message": "Diagnostic logs cleared"})
			return
		}
		q := r.URL.Query()
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit < 1 || limit > 5000 {
			limit = 500
		}
		rows, total, filtered, e := s.logs(ctx, q.Get("level"), q.Get("search"), limit)
		if e != nil {
			httpx.Error(w, 500, "Could not read logs")
			return
		}
		httpx.JSON(w, 200, map[string]any{"entries": rows, "total_in_file": total, "filtered_count": filtered})
	case "bundle":
		logs, _, _, e := s.logs(ctx, "", "", 5000)
		if e != nil {
			httpx.Error(w, 500, "Could not read logs")
			return
		}
		counts := map[string]int{}
		for _, table := range []string{"printers", "print_queue", "print_archives", "library_files", "users", "spool", "projects"} {
			var n int
			if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); e != nil {
				httpx.Error(w, 500, "Could not collect metrics")
				return
			}
			counts[table] = n
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="bambastic-support.zip"`)
		z := zip.NewWriter(w)
		defer z.Close()
		for file, data := range map[string]any{"system.json": map[string]any{"product": "Bambastic", "version": "0.1.0", "go_version": runtime.Version(), "platform": runtime.GOOS, "architecture": runtime.GOARCH, "generated_at": time.Now().UTC(), "counts": counts}, "diagnostics.json": logs} {
			out, e := z.Create(file)
			if e != nil {
				return
			}
			if json.NewEncoder(out).Encode(data) != nil {
				return
			}
		}
	}
}
func (s *Service) logs(ctx context.Context, level, search string, limit int) ([]httpx.Object, int, int, error) {
	filt := " WHERE (?='' OR level=?) AND (?='' OR instr(lower(message ||' '||logger_name),lower(?))>0)"
	args := []any{strings.ToUpper(level), strings.ToUpper(level), search, search}
	var total, count int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM bambastic_diagnostics").Scan(&total); e != nil {
		return nil, 0, 0, e
	}
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM bambastic_diagnostics"+filt, args...).Scan(&count); e != nil {
		return nil, 0, 0, e
	}
	rows, e := s.db.QueryContext(ctx, fmt.Sprintf("SELECT timestamp,level,logger_name,message FROM bambastic_diagnostics%s ORDER BY id DESC LIMIT ?", filt), append(args, limit)...)
	if e != nil {
		return nil, 0, 0, e
	}
	v, e := httpx.Rows(rows)
	return v, total, count, e
}
