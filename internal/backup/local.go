package backup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

// PrepareRestore stages database state and returns a callback that keeps it
// only if the filesystem generation pointer was successfully published.
type PrepareRestore func(context.Context, string) (func(bool), error)

type Local struct {
	DB             *sql.DB
	Export         ExportDatabase
	PrepareRestore PrepareRestore
	Data, Root     string
	Reload         func()
	mu             sync.Mutex
	working        bool
	next           time.Time
	scheduleKey    string
}

const RestoreIDFile = ".bambastic-restore-id"
const marker = ".bambastic-generation"

var generationName = regexp.MustCompile(`^[a-f0-9]{32}$`)
var backupName = regexp.MustCompile(`^(bambastic|bambuddy)[_-]backup[_-][A-Za-z0-9_-]+\.zip$`)

func Active(root string) (string, error) {
	data, e := os.ReadFile(filepath.Join(root, marker))
	if os.IsNotExist(e) {
		return root, nil
	}
	if e != nil {
		return "", e
	}
	name := strings.TrimSpace(string(data))
	if !generationName.MatchString(name) {
		return "", errors.New("invalid active restoration pointer")
	}
	path := filepath.Join(root, ".restores", name)
	if info, e := os.Stat(filepath.Join(path, "bambastic.db")); e != nil || !info.Mode().IsRegular() {
		return "", errors.New("restored database unavailable")
	}
	return path, nil
}
func (s *Local) activate(ctx context.Context, archive string) error {
	var raw [16]byte
	rand.Read(raw[:])
	name := hex.EncodeToString(raw[:])
	dest := filepath.Join(s.Root, ".restores", name)
	if err := Restore(ctx, archive, dest); err != nil {
		return err
	}
	activated := false
	defer func() {
		if !activated {
			os.RemoveAll(dest)
		}
	}()
	if s.PrepareRestore != nil {
		finish, err := s.PrepareRestore(ctx, dest)
		if err != nil {
			return err
		}
		defer func() { finish(activated) }()
	}
	f, err := os.CreateTemp(s.Root, ".generation-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(name + "\n")
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(f.Name(), filepath.Join(s.Root, marker))
	}
	activated = err == nil
	return err
}
func (s *Local) config(ctx context.Context) (map[string]any, error) {
	out := map[string]any{}
	defaults := map[string]string{"enabled": "false", "schedule": "daily", "time": "03:00", "retention": "5", "path": "", "last_backup_at": "", "last_status": "", "last_message": ""}
	for k, d := range defaults {
		v, e := access.Setting(ctx, s.DB, "local_backup_"+k, d)
		if e != nil {
			return nil, e
		}
		switch k {
		case "enabled":
			out[k] = v == "true"
		case "retention":
			n, _ := strconv.Atoi(v)
			out[k] = max(1, n)
		default:
			out[k] = v
		}
	}
	out["default_path"] = filepath.Join(s.Root, "backups")
	out["timezone"] = time.Local.String()
	s.mu.Lock()
	out["is_running"] = s.working
	out["next_run"] = nil
	if !s.next.IsZero() && out["enabled"] == true {
		out["next_run"] = s.next
	}
	s.mu.Unlock()
	return out, nil
}
func (s *Local) folder(v map[string]any) (string, error) {
	dest := v["path"].(string)
	if dest == "" {
		dest = v["default_path"].(string)
	}
	dest, e := filepath.Abs(dest)
	if e != nil {
		return "", e
	}
	root, e := filepath.Abs(s.Root)
	if e != nil {
		return "", e
	}
	if path, err := filepath.Rel(root, dest); err == nil && (path == "." || path != "backups" && !strings.HasPrefix(path, ".."+string(filepath.Separator)) && path != "..") {
		return "", errors.New("choose the backups directory or a location outside the installation")
	}
	return dest, nil
}

type item struct {
	Name    string    `json:"filename"`
	Size    int64     `json:"size"`
	Created time.Time `json:"created_at"`
}

func (s *Local) list(dir string) ([]item, error) {
	list := []item{}
	rows, e := os.ReadDir(dir)
	if os.IsNotExist(e) {
		return list, nil
	}
	if e != nil {
		return nil, e
	}
	for _, entry := range rows {
		if !backupName.MatchString(entry.Name()) || !entry.Type().IsRegular() {
			continue
		}
		info, e := entry.Info()
		if e != nil {
			return nil, e
		}
		list = append(list, item{entry.Name(), info.Size(), info.ModTime()})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Created.After(list[j].Created) })
	return list, nil
}
func (s *Local) lock() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.working {
		return false
	}
	s.working = true
	return true
}
func (s *Local) unlock() { s.mu.Lock(); s.working = false; s.mu.Unlock() }
func (s *Local) create(ctx context.Context, v map[string]any) (string, error) {
	if !s.lock() {
		return "", errors.New("backup or restore in progress")
	}
	defer s.unlock()
	dir, err := s.folder(v)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	temp, err := os.MkdirTemp(dir, ".snapshot-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(temp)
	zip, err := snapshot(ctx, s.DB, s.Data, temp, s.Export)
	if err != nil {
		return "", err
	}
	name := "bambastic-backup-" + time.Now().UTC().Format("20060102T150405000000000Z") + "-" + filepath.Base(temp)[10:] + ".zip"
	if err = os.Rename(zip, filepath.Join(dir, name)); err != nil {
		return "", err
	}
	backups, err := s.list(dir)
	if err != nil {
		return "", err
	}
	if max := v["retention"].(int); len(backups) > max {
		for _, old := range backups[max:] {
			if err = os.Remove(filepath.Join(dir, old.Name)); err != nil {
				return "", err
			}
		}
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	for k, val := range map[string]string{"last_backup_at": time.Now().UTC().Format(time.RFC3339), "last_status": "success", "last_message": name} {
		if err = access.Put(tx, "local_backup_"+k, val); err != nil {
			return "", err
		}
	}
	return name, tx.Commit()
}
func next(at time.Time, schedule, clock string) time.Time {
	if schedule == "hourly" {
		return at.Add(time.Hour)
	}
	t, e := time.Parse("15:04", clock)
	if e != nil {
		t, _ = time.Parse("15:04", "03:00")
	}
	d := time.Date(at.Year(), at.Month(), at.Day(), t.Hour(), t.Minute(), 0, 0, at.Location())
	if !d.After(at) {
		d = d.AddDate(0, 0, 1)
	}
	if schedule == "weekly" {
		d = d.AddDate(0, 0, 7)
	}
	return d
}
func (s *Local) Run(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		v, e := s.config(ctx)
		if e == nil {
			now := time.Now()
			key := fmt.Sprint(v["enabled"], v["schedule"], v["time"])
			s.mu.Lock()
			if key != s.scheduleKey {
				s.scheduleKey = key
				s.next = next(now, v["schedule"].(string), v["time"].(string))
			}
			due := v["enabled"] == true && !now.Before(s.next)
			if due {
				s.next = next(now, v["schedule"].(string), v["time"].(string))
			}
			s.mu.Unlock()
			if due {
				if _, e = s.create(ctx, v); e != nil && ctx.Err() == nil {
					s.DB.Exec("INSERT INTO settings(key,value) VALUES('local_backup_last_status','failed') ON CONFLICT(key) DO UPDATE SET value='failed'")
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Local) Register(api *http.ServeMux) {
	for _, p := range []string{"GET /api/v1/local-backup/status", "GET /api/v1/local-backup/path-check", "POST /api/v1/local-backup/run", "GET /api/v1/local-backup/backups", "GET /api/v1/local-backup/backups/{filename}/download", "POST /api/v1/local-backup/backups/{filename}/restore", "DELETE /api/v1/local-backup/backups/{filename}", "POST /api/v1/settings/restore"} {
		api.HandleFunc(p, s.handle)
	}
}
func (s *Local) handle(w http.ResponseWriter, r *http.Request) {
	if !access.From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required for full backups")
		return
	}
	v, e := s.config(r.Context())
	if e != nil {
		httpx.Error(w, 500, "Cannot read backup configuration")
		return
	}
	dir, e := s.folder(v)
	if e != nil {
		httpx.Error(w, 422, e.Error())
		return
	}
	path := r.URL.Path
	name := r.PathValue("filename")
	file := ""
	if name != "" {
		if !backupName.MatchString(name) {
			httpx.Error(w, 404, "Unknown backup")
			return
		}
		file = filepath.Join(dir, name)
		info, err := os.Lstat(file)
		if err != nil || !info.Mode().IsRegular() {
			httpx.Error(w, 404, "Backup unavailable")
			return
		}
	}
	switch {
	case strings.HasSuffix(path, "/status"):
		httpx.JSON(w, 200, v)
	case strings.HasSuffix(path, "/path-check"):
		e = os.MkdirAll(dir, 0700)
		if e == nil {
			var f *os.File
			f, e = os.CreateTemp(dir, ".probe-")
			if e == nil {
				f.Close()
				os.Remove(f.Name())
			}
		}
		code := "ok"
		detail := ""
		if e != nil {
			code = "error"
			detail = e.Error()
		}
		httpx.JSON(w, 200, map[string]any{"writable": e == nil, "path": dir, "code": code, "detail": detail})
	case strings.HasSuffix(path, "/run"):
		name, err := s.create(r.Context(), v)
		if err != nil {
			httpx.Error(w, 409, err.Error())
			return
		}
		httpx.JSON(w, 200, map[string]any{"success": true, "message": "Backup created", "filename": name})
	case strings.HasSuffix(path, "/backups"):
		list, err := s.list(dir)
		if err != nil {
			httpx.Error(w, 500, "Cannot list backups")
			return
		}
		httpx.JSON(w, 200, list)
	case strings.HasSuffix(path, "/download"):
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		http.ServeFile(w, r, file)
	case r.Method == "DELETE":
		if err := os.Remove(file); err != nil {
			httpx.Error(w, 409, "Could not delete backup")
			return
		}
		httpx.JSON(w, 200, map[string]any{"success": true, "message": "Backup deleted"})
	case strings.HasSuffix(path, "/restore"):
		if !s.lock() {
			httpx.Error(w, 409, "Backup or restore in progress")
			return
		}
		defer s.unlock()
		if file == "" {
			r.Body = http.MaxBytesReader(w, r.Body, 64<<30)
			reader, e := r.MultipartReader()
			if e != nil {
				httpx.Error(w, 422, "Expected a backup file")
				return
			}
			for {
				part, e := reader.NextPart()
				if e != nil {
					httpx.Error(w, 422, "Backup file missing")
					return
				}
				if part.FormName() != "file" {
					part.Close()
					continue
				}
				f, e := os.CreateTemp("", "bambastic-upload-")
				if e != nil {
					httpx.Error(w, 500, "Cannot stage upload")
					return
				}
				file = f.Name()
				defer os.Remove(file)
				_, e = io.Copy(f, part)
				closeErr := f.Close()
				if e != nil || closeErr != nil {
					httpx.Error(w, 400, "Incomplete backup upload")
					return
				}
				break
			}
		}
		if err := s.activate(r.Context(), file); err != nil {
			httpx.Error(w, 422, err.Error())
			return
		}
		httpx.JSON(w, 200, map[string]any{"success": true, "message": "Backup restored. Bambastic is restarting.", "restart_required": true})
		s.Reload()
	}
}
