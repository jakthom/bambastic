package cloudbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type Service struct {
	CalibrationConnected func(int64) bool
	KProfiles            func(context.Context, int64, string) ([]map[string]any, error)
	RestoreKProfiles     func(context.Context, int64, string, []map[string]any) (bool, error)

	db            *sql.DB
	auth          *access.Service
	mu            sync.Mutex
	busy, restore bool
	progress      string
	CloudProfiles func(context.Context, access.Principal, string) (map[string][]map[string]any, string)
	ResolvePreset func(context.Context, access.Principal, string, string, string) (map[string]any, error)
}

func New(db *sql.DB, auth *access.Service) (*Service, error) {
	s := &Service{db: db, auth: auth}
	if _, e := db.Exec("CREATE TABLE IF NOT EXISTS bambastic_git_restore_ids(repo TEXT NOT NULL,kind TEXT NOT NULL,source_id INTEGER NOT NULL,identity TEXT NOT NULL,target_id INTEGER NOT NULL,PRIMARY KEY(repo,kind,source_id,identity))"); e != nil {
		return nil, e
	}
	_, e := db.Exec("UPDATE github_backup_logs SET status='failed',completed_at=CURRENT_TIMESTAMP,error_message='Interrupted by server restart' WHERE status='running'")
	return s, e
}

type fault struct {
	code   int
	reason string
}

func (e fault) Error() string { return e.reason }
func invalid(s string) error  { return fault{422, s} }
func str(v any) string {
	if v == nil {
		return ""
	}
	if t, ok := v.(time.Time); ok {
		return t.UTC().Format(time.RFC3339Nano)
	}
	return fmt.Sprint(v)
}
func ident(v any) int64 { i, _ := strconv.ParseInt(str(v), 10, 64); return i }
func truth(v any) bool  { return v == true || str(v) == "1" || str(v) == "true" }
func sub(v any) obj {
	if x, ok := v.(map[string]any); ok {
		return x
	}
	return obj{}
}
func body(r *http.Request) (obj, error) {
	var v obj
	d := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if e := d.Decode(&v); e != nil || v == nil {
		return nil, invalid("Invalid request body")
	}
	var more any
	if d.Decode(&more) != io.EOF {
		return nil, invalid("Expected one JSON object")
	}
	return v, nil
}

type query interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func rows(ctx context.Context, db query, q string, args ...any) ([]obj, error) {
	r, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(r)
}
func first(ctx context.Context, db query, q string, args ...any) (obj, error) {
	v, e := rows(ctx, db, q, args...)
	if e != nil {
		return nil, e
	}
	if len(v) == 0 {
		return nil, sql.ErrNoRows
	}
	return v[0], nil
}
func mutate(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, t string, id int64, d obj) (int64, error) {
	keys := []string{}
	for k := range d {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := []any{}
	for _, k := range keys {
		x := d[k]
		switch x.(type) {
		case map[string]any, []any:
			b, _ := json.Marshal(x)
			x = string(b)
		}
		args = append(args, x)
	}
	if id == 0 {
		res, e := db.ExecContext(ctx, "INSERT INTO "+t+"("+strings.Join(keys, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+")", args...)
		if e != nil {
			return 0, e
		}
		return res.LastInsertId()
	}
	if len(keys) == 0 {
		return id, nil
	}
	_, e := db.ExecContext(ctx, "UPDATE "+t+" SET "+strings.Join(keys, "=?,")+"=? WHERE id=?", append(args, id)...)
	return id, e
}
func (s *Service) config(ctx context.Context) (obj, error) {
	return first(ctx, s.db, "SELECT * FROM github_backup_config ORDER BY id LIMIT 1")
}
func (s *Service) repo(ctx context.Context, c obj) (repository, error) {
	token, e := s.auth.OpenSecret(str(c["access_token"]))
	if e != nil {
		return repository{}, errors.New("stored repository credential unavailable")
	}
	return newRepo(str(c["repository_url"]), str(c["provider"]), str(c["branch"]), token, truth(c["allow_insecure_http"]))
}
func public(c obj) obj {
	d := obj{}
	for k, x := range c {
		if k != "access_token" && k != "schedule_cron" {
			d[k] = x
		}
	}
	d["has_token"] = str(c["access_token"]) != ""
	return d
}
func next(schedule string) string {
	interval := map[string]time.Duration{"hourly": time.Hour, "daily": 24 * time.Hour, "weekly": 7 * 24 * time.Hour}[schedule]
	return time.Now().UTC().Add(interval).Format(time.RFC3339)
}
func (s *Service) lock(restore bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return false
	}
	s.busy = true
	s.restore = restore
	s.progress = "Checking repository"
	return true
}
func (s *Service) unlock() {
	s.mu.Lock()
	s.busy = false
	s.restore = false
	s.progress = ""
	s.mu.Unlock()
}
func (s *Service) step(msg string) { s.mu.Lock(); s.progress = msg; s.mu.Unlock() }
func (s *Service) save(r *http.Request) (any, error) {
	if !s.lock(false) {
		return nil, fault{409, "Backup operation in progress"}
	}
	defer s.unlock()
	d, e := body(r)
	if e != nil {
		return nil, e
	}
	ctx := r.Context()
	old, e := s.config(ctx)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	patch := r.Method == "PATCH"
	if patch && old == nil {
		return nil, sql.ErrNoRows
	}
	c := obj{"enabled": true, "branch": "main", "provider": "github", "allow_insecure_http": false, "schedule_enabled": false, "schedule_type": "daily", "backup_kprofiles": true, "backup_cloud_profiles": true, "backup_settings": false, "backup_spools": false, "backup_archives": false}
	if patch {
		for k := range c {
			c[k] = old[k]
		}
		c["repository_url"] = old["repository_url"]
	}
	for _, k := range strings.Fields("enabled branch provider allow_insecure_http schedule_enabled schedule_type backup_kprofiles backup_cloud_profiles backup_settings backup_spools backup_archives repository_url") {
		if val, ok := d[k]; ok {
			switch c[k].(type) {
			case bool:
				if _, ok := val.(bool); !ok {
					return nil, invalid("Invalid " + k)
				}
			}
			c[k] = val
		}
	}
	schedule := str(c["schedule_type"])
	if schedule != "daily" && schedule != "weekly" && schedule != "hourly" {
		return nil, invalid("Unknown schedule")
	}
	token := str(d["access_token"])
	if token == "" || token == "********" {
		if !patch || c["repository_url"] != old["repository_url"] || c["provider"] != old["provider"] {
			return nil, invalid("Enter a token for this repository")
		}
		token, e = s.auth.OpenSecret(str(old["access_token"]))
		if e != nil {
			return nil, invalid("Stored token unavailable")
		}
	}
	repo, e := newRepo(str(c["repository_url"]), str(c["provider"]), str(c["branch"]), token, truth(c["allow_insecure_http"]))
	if e != nil {
		return nil, invalid(e.Error())
	}
	if _, e = repo.inspect(ctx); e != nil {
		return nil, fault{400, e.Error()}
	}
	c["access_token"] = s.auth.SealSecret(token)
	if c["access_token"] == "" {
		return nil, errors.New("unable to seal credential")
	}
	c["next_scheduled_run"] = nil
	if truth(c["enabled"]) && truth(c["schedule_enabled"]) {
		c["next_scheduled_run"] = next(schedule)
	}
	c["updated_at"] = time.Now().UTC().Format(time.RFC3339)
	if _, e = mutate(ctx, s.db, "github_backup_config", ident(old["id"]), c); e != nil {
		return nil, e
	}
	out, e := s.config(ctx)
	return public(out), e
}
func (s *Service) Register(m *http.ServeMux) {
	type handler func(*http.Request) (any, error)
	reg := func(method, path, perm string, h handler) {
		m.HandleFunc(method+" /api/v1/github-backup/"+path, func(w http.ResponseWriter, r *http.Request) {
			a := access.From(r.Context())
			if !a.Has(perm) || a.PrinterIDs != nil {
				httpx.Error(w, 403, "Global configuration backup permission required")
				return
			}
			v, e := h(r)
			if e != nil {
				var f fault
				switch {
				case errors.As(e, &f):
					httpx.Error(w, f.code, f.reason)
				case errors.Is(e, sql.ErrNoRows):
					httpx.Error(w, 404, "Record not found")
				default:
					log.Printf("git configuration backup: %v", e)
					httpx.Error(w, 502, "Backup operation failed")
				}
				return
			}
			httpx.JSON(w, 200, v)
		})
	}
	const backup = "github:backup"
	const restore = "github:restore"
	reg("GET", "config", backup, func(r *http.Request) (any, error) {
		v, e := s.config(r.Context())
		if errors.Is(e, sql.ErrNoRows) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		return public(v), nil
	})
	reg("POST", "config", backup, s.save)
	reg("PATCH", "config", backup, s.save)
	reg("DELETE", "config", backup, func(r *http.Request) (any, error) {
		if !s.lock(false) {
			return nil, fault{409, "Backup operation in progress"}
		}
		defer s.unlock()
		_, e := s.db.ExecContext(r.Context(), "DELETE FROM github_backup_config")
		return obj{"message": "Configuration removed"}, e
	})
	reg("GET", "cloud-accounts", backup, func(r *http.Request) (any, error) {
		v, e := s.accounts(r.Context())
		if e != nil {
			return nil, e
		}
		return obj{"bambu": len(v["cloud"]), "orca": len(v["orca_cloud"])}, nil
	})
	for _, path := range []string{"test", "test-stored"} {
		reg("POST", path, backup, func(r *http.Request) (any, error) {
			var repo repository
			var e error
			if path == "test" {
				d, err := body(r)
				if err != nil {
					return nil, err
				}
				kind := str(d["provider"])
				if kind == "" {
					kind = "github"
				}
				repo, e = newRepo(str(d["repo_url"]), kind, "main", str(d["token"]), true)
			} else {
				c, err := s.config(r.Context())
				if err != nil {
					return nil, err
				}
				repo, e = s.repo(r.Context(), c)
			}
			if e != nil {
				return nil, invalid(e.Error())
			}
			v, e := repo.inspect(r.Context())
			if e != nil {
				return obj{"success": false, "message": e.Error()}, nil
			}
			return v, nil
		})
	}
	reg("GET", "status", backup, func(r *http.Request) (any, error) {
		c, e := s.config(r.Context())
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		return obj{"configured": c != nil, "enabled": truth(c["enabled"]), "is_running": s.busy && !s.restore, "restore_running": s.restore, "progress": s.progress, "last_backup_at": c["last_backup_at"], "last_backup_status": c["last_backup_status"], "next_scheduled_run": c["next_scheduled_run"]}, nil
	})
	reg("POST", "run", backup, func(r *http.Request) (any, error) { return s.run(r.Context(), "manual") })
	reg("GET", "commits", restore, func(r *http.Request) (any, error) {
		c, e := s.config(r.Context())
		if e != nil {
			return nil, e
		}
		repo, e := s.repo(r.Context(), c)
		if e != nil {
			return nil, e
		}
		n := ident(r.URL.Query().Get("limit"))
		if n <= 0 {
			n = 20
		}
		v, e := repo.commits(r.Context(), repo.branch, min(100, int(n)))
		if e != nil {
			return obj{"success": false, "branch": repo.branch, "message": e.Error(), "commits": []obj{}}, nil
		}
		return obj{"success": true, "branch": repo.branch, "message": "Commit history loaded", "commits": v}, nil
	})
	reg("GET", "restore/preview", restore, s.preview)
	reg("POST", "restore", restore, s.apply)
	reg("GET", "logs", backup, func(r *http.Request) (any, error) {
		n := ident(r.URL.Query().Get("limit"))
		if n <= 0 {
			n = 50
		}
		return rows(r.Context(), s.db, "SELECT * FROM github_backup_logs ORDER BY id DESC LIMIT ? OFFSET ?", min(n, 1000), max(0, ident(r.URL.Query().Get("offset"))))
	})
	reg("DELETE", "logs", backup, func(r *http.Request) (any, error) {
		keep := int64(10)
		if r.URL.Query().Has("keep_last") {
			keep = ident(r.URL.Query().Get("keep_last"))
		}
		res, e := s.db.ExecContext(r.Context(), "DELETE FROM github_backup_logs WHERE status!='running' AND id NOT IN (SELECT id FROM github_backup_logs ORDER BY id DESC LIMIT ?)", max(0, keep))
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		return obj{"deleted": n, "message": "Backup history pruned"}, nil
	})
}
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			c, e := s.config(ctx)
			if e == nil && truth(c["enabled"]) && truth(c["schedule_enabled"]) {
				var due bool
				if e = s.db.QueryRowContext(ctx, "SELECT coalesce(julianday(?)<=julianday('now'),1)", c["next_scheduled_run"]).Scan(&due); e == nil && due {
					s.run(ctx, "scheduled")
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}
func (s *Service) run(ctx context.Context, trigger string) (any, error) {
	if !s.lock(false) {
		return nil, fault{409, "Backup operation already running"}
	}
	defer s.unlock()
	c, e := s.config(ctx)
	if e != nil {
		return nil, e
	}
	if !truth(c["enabled"]) {
		return nil, fault{400, "Backup is disabled"}
	}
	id, e := mutate(ctx, s.db, "github_backup_logs", 0, obj{"config_id": c["id"], "started_at": time.Now().UTC().Format(time.RFC3339), "trigger": trigger, "status": "running", "files_changed": 0})
	if e != nil {
		return nil, e
	}
	var sha string
	changed := 0
	state := "success"
	msg := "Configuration backed up"
	work := func() error {
		repo, e := s.repo(ctx, c)
		if e != nil {
			return e
		}
		if _, e = repo.inspect(ctx); e != nil {
			return e
		}
		s.step("Collecting selected categories")
		files, e := s.collect(ctx, c)
		if e != nil {
			return e
		}
		s.step("Committing changed files")
		sha, changed, e = repo.push(ctx, files)
		return e
	}
	e = work()
	if e != nil {
		state = "failed"
		msg = e.Error()
	} else if changed == 0 {
		state = "skipped"
		msg = "No configuration changes"
	}
	done := time.Now().UTC().Format(time.RFC3339)
	logFields := obj{"status": state, "completed_at": done, "files_changed": changed, "commit_sha": sha}
	if e != nil {
		logFields["error_message"] = msg
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(recordCtx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = mutate(recordCtx, tx, "github_backup_logs", id, logFields); err != nil {
		return nil, err
	}
	upd := obj{"last_backup_at": done, "last_backup_status": state, "last_backup_message": msg}
	if sha != "" {
		upd["last_backup_commit_sha"] = sha
	}
	if truth(c["schedule_enabled"]) {
		upd["next_scheduled_run"] = next(str(c["schedule_type"]))
	}
	if _, err = mutate(recordCtx, tx, "github_backup_config", ident(c["id"]), upd); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return obj{"success": e == nil, "message": msg, "log_id": id, "commit_sha": sha, "files_changed": changed}, nil
}
