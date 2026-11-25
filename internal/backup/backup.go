// Package backup creates portable installation snapshots and restores them into a new generation.
package backup

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type ExportDatabase func(context.Context, string) error

func snapshot(ctx context.Context, db *sql.DB, root, temp string, export ExportDatabase) (string, error) {
	file := filepath.Join(temp, "bambuddy.db")
	engine := "sqlite"
	if export == nil {
		if _, e := db.ExecContext(ctx, "VACUUM INTO ?", file); e != nil {
			return "", e
		}
	} else {
		engine = "postgresql"
		if e := export(ctx, file); e != nil {
			return "", e
		}
	}
	out := filepath.Join(temp, "backup.zip")
	f, e := os.OpenFile(out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return "", e
	}
	defer f.Close()
	z := zip.NewWriter(f)
	add := func(source, name string) error {
		v, e := os.Open(source)
		if e != nil {
			return e
		}
		defer v.Close()
		w, e := z.Create(name)
		if e != nil {
			return e
		}
		_, e = io.Copy(w, v)
		return e
	}
	if e = add(file, "bambuddy.db"); e != nil {
		return "", e
	}
	e = filepath.WalkDir(root, func(path string, entry fs.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		name, e := filepath.Rel(root, path)
		if e != nil {
			return e
		}
		name = filepath.ToSlash(name)
		if name == "." {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			if name == "ui-diagnostics" || name == "backups" || name == ".restores" || name == "tmp" || name == "production/external-work" {
				return filepath.SkipDir
			}
			_, e := z.Create(name + "/")
			return e
		}
		if strings.HasPrefix(name, "bambastic.db") || name == "bambastic.lock" || name == marker || name == RestoreIDFile || name == "manifest.json" || name == "bambuddy.db" {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		return add(path, name)
	})
	if e != nil {
		return "", e
	}
	m, e := z.Create("manifest.json")
	if e != nil {
		return "", e
	}
	e = json.NewEncoder(m).Encode(map[string]any{"format": 1, "app_version": "0.1.0", "application": "Bambastic", "created_at": time.Now().UTC(), "database": engine})
	if e != nil {
		return "", e
	}
	if e = z.Close(); e != nil {
		return "", e
	}
	return out, f.Sync()
}
func Serve(db *sql.DB, dir string, export ExportDatabase) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !access.From(r.Context()).Admin {
			httpx.Error(w, 403, "Administrator required for full backup")
			return
		}
		tmp, e := os.MkdirTemp("", "bambastic-snapshot-")
		if e != nil {
			httpx.Error(w, 500, "Cannot allocate backup storage")
			return
		}
		defer os.RemoveAll(tmp)
		file, e := snapshot(r.Context(), db, dir, tmp, export)
		if e != nil {
			httpx.Error(w, 500, "Could not complete backup")
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="bambastic-backup.zip"`)
		http.ServeFile(w, r, file)
	}
}

// Restore stages the complete archive beside a new destination, checks the
// database and clears session credentials before publishing the directory.
func Restore(ctx context.Context, source, dest string) error {
	if source == "" || dest == "" {
		return errors.New("provide a backup ZIP and a new destination directory")
	}
	absolute, e := filepath.Abs(dest)
	if e != nil {
		return e
	}
	if _, e = os.Lstat(absolute); e == nil {
		return fmt.Errorf("destination already exists: %s", dest)
	} else if !os.IsNotExist(e) {
		return e
	}
	f, e := zip.OpenReader(source)
	if e != nil {
		return e
	}
	defer f.Close()
	if e = os.MkdirAll(filepath.Dir(absolute), 0700); e != nil {
		return e
	}
	stage, e := os.MkdirTemp(filepath.Dir(absolute), ".bambastic-restore-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	seen := map[string]bool{}
	var total uint64
	for _, entry := range f.File {
		if e = ctx.Err(); e != nil {
			return e
		}
		name := strings.TrimSuffix(entry.Name, "/")
		if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\:") || entry.Mode()&os.ModeSymlink != 0 || seen[name] {
			return errors.New("invalid or duplicate backup entry")
		}
		seen[name] = true
		if name == RestoreIDFile {
			return errors.New("reserved restore entry")
		}
		if name == ".restores" || strings.HasPrefix(name, ".restores/") || name == marker || name == "production/external-work" || strings.HasPrefix(name, "production/external-work/") {
			return errors.New("invalid installation entry")
		}
		if name == "manifest.json" {
			continue
		}
		if strings.HasPrefix(name, "bambastic.db") || name == "bambastic.lock" || strings.HasPrefix(name, "bambuddy.db-") {
			return errors.New("conflicting database file in archive")
		}
		if name == "bambuddy.db" {
			name = "bambastic.db"
		}
		target := filepath.Join(stage, filepath.FromSlash(name))
		if entry.FileInfo().IsDir() {
			if e = os.MkdirAll(target, 0700); e != nil {
				return e
			}
			continue
		}
		if total > 1<<40 || entry.UncompressedSize64 > 1<<40-total {
			return errors.New("backup is too large")
		}
		total += entry.UncompressedSize64
		if e = os.MkdirAll(filepath.Dir(target), 0700); e != nil {
			return e
		}
		e = extract(entry, target)
		if e != nil {
			return e
		}
	}
	if !seen["bambuddy.db"] {
		return errors.New("backup is missing bambuddy.db")
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(stage, "bambastic.db")}
	db, e := sql.Open("sqlite", u.String()+"?mode=rw")
	if e != nil {
		return e
	}
	if e = checkDB(ctx, db); e != nil {
		db.Close()
		return e
	}
	if e = db.Close(); e != nil {
		return e
	}
	var generation [16]byte
	if _, e = rand.Read(generation[:]); e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(stage, RestoreIDFile), []byte(hex.EncodeToString(generation[:])), 0600); e != nil {
		return e
	}
	return os.Rename(stage, absolute)
}
func extract(in *zip.File, target string) error {
	r, e := in.Open()
	if e != nil {
		return e
	}
	defer r.Close()
	w, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	_, e = io.Copy(w, r)
	closeErr := w.Close()
	if e != nil {
		return e
	}
	return closeErr
}
func checkDB(ctx context.Context, db *sql.DB) error {
	var check string
	if e := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&check); e != nil || check != "ok" {
		return errors.New("backup failed SQLite integrity check")
	}
	for _, t := range []string{"printers", "users", "settings", "library_files", "print_archives", "print_queue", "print_log_entries"} {
		var kind string
		if e := db.QueryRowContext(ctx, "SELECT type FROM sqlite_master WHERE name=?", t).Scan(&kind); e != nil || kind != "table" {
			return fmt.Errorf("backup is missing %s", t)
		}
	}
	var broken int
	if e := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_foreign_key_check)").Scan(&broken); e != nil || broken != 0 {
		return errors.New("backup failed SQLite foreign key validation")
	}
	for _, t := range []string{"bambastic_credentials", "bambastic_mcp_codes", "bambastic_connect_codes", "bambastic_email_tokens", "bambastic_oidc_exchange", "bambastic_oidc_state", "bambastic_challenges", "bambastic_totp_pending", "auth_ephemeral_tokens", "long_lived_tokens"} {
		var n int
		if e := db.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", t).Scan(&n); e != nil {
			return e
		}
		if n != 0 {
			if _, e := db.ExecContext(ctx, "DELETE FROM "+t); e != nil {
				return e
			}
		}
	}
	return nil
}
