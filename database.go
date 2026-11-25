package main

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"bambastic/internal/backup"
	"bambastic/internal/pgtransfer"
	"bambastic/internal/postgres"
)

type databaseOptions struct{ url, schema string }
type databaseLease interface {
	Done() <-chan struct{}
	Close() error
}
type storageHooks struct {
	pg         bool
	lease      databaseLease
	export     backup.ExportDatabase
	prepare    backup.PrepareRestore
	importData func(context.Context) (bool, error)
}

var pgNamespace = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func validNamespace(name string) bool {
	return pgNamespace.MatchString(name) && !strings.HasPrefix(name, "pg_") && name != "information_schema" && name != "public"
}

// A restored file generation gets a separate PostgreSQL schema. The stable
// base schema still identifies the single scheduler lease for the installation.
func (o databaseOptions) namespace(data string) (string, error) {
	base := o.schema
	if base == "" {
		base = "bambastic"
	}
	if !validNamespace(base) {
		return "", errors.New("choose an application-specific PostgreSQL schema using lowercase letters, digits and underscores")
	}
	id, e := os.ReadFile(filepath.Join(data, backup.RestoreIDFile))
	if os.IsNotExist(e) {
		return base, nil
	}
	if e != nil {
		return "", e
	}
	k, e := hex.DecodeString(strings.TrimSpace(string(id)))
	if e != nil || len(k) != 16 {
		return "", errors.New("invalid restore generation identifier")
	}
	hash := sha256.Sum256(append([]byte(base+"-"), k...))
	return base[:min(len(base), 44)] + "_r_" + hex.EncodeToString(hash[:8]), nil
}
func (o databaseOptions) open(ctx context.Context, root, data string) (*sql.DB, storageHooks, error) {
	if o.url == "" {
		db, e := openDB(data)
		return db, storageHooks{}, e
	}
	u, e := url.Parse(o.url)
	if e != nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") {
		return nil, storageHooks{}, errors.New("database URL must use postgres or postgresql")
	}
	if o.schema == "" {
		o.schema = "bambastic"
	}
	if !validNamespace(o.schema) {
		return nil, storageHooks{}, errors.New("invalid Bambastic PostgreSQL schema")
	}
	lease, e := postgres.Acquire(ctx, o.url, o.schema)
	if e != nil {
		return nil, storageHooks{}, fmt.Errorf("acquire PostgreSQL scheduler lease: %w", e)
	}
	db, hooks, e := o.generation(ctx, root, data)
	if e != nil {
		lease.Close()
		return nil, storageHooks{}, e
	}
	hooks.lease = lease
	return db, hooks, nil
}
func (o databaseOptions) generation(ctx context.Context, root, data string) (*sql.DB, storageHooks, error) {
	schema, e := o.namespace(data)
	if e != nil {
		return nil, storageHooks{}, e
	}
	db, e := postgres.Open(ctx, o.url, schema)
	if e != nil {
		return nil, storageHooks{}, e
	}
	if e = postgres.Bootstrap(ctx, db, ddl); e != nil {
		db.Close()
		return nil, storageHooks{}, fmt.Errorf("initialize PostgreSQL schema: %w", e)
	}
	hooks := storageHooks{pg: true}
	hooks.importData = func(ctx context.Context) (bool, error) {
		file := filepath.Join(data, "bambastic.db")
		if _, e := os.Stat(file); errors.Is(e, os.ErrNotExist) {
			return false, nil
		} else if e != nil {
			return false, e
		}
		raw, e := postgres.Raw(ctx, o.url, schema)
		if e != nil {
			return false, e
		}
		defer raw.Close()
		var markerExists bool
		if e = raw.QueryRowContext(ctx, "SELECT to_regclass('bb_pg_transfer_state') IS NOT NULL").Scan(&markerExists); e != nil {
			return false, e
		}
		if markerExists {
			var done bool
			if e = raw.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM bb_pg_transfer_state WHERE one=1 AND imported)").Scan(&done); e != nil || done {
				return false, e
			}
		}
		path, e := filepath.Abs(file)
		if e != nil {
			return false, e
		}
		u := url.URL{Scheme: "file", Path: path}
		src, e := sql.Open("sqlite", u.String()+"?mode=ro")
		if e != nil {
			return false, e
		}
		defer src.Close()
		if e = pgtransfer.Import(ctx, src, raw); e != nil {
			return false, fmt.Errorf("import portable SQLite data into PostgreSQL: %w", e)
		}
		return true, nil
	}
	hooks.export = func(ctx context.Context, target string) error {
		raw, e := postgres.Raw(ctx, o.url, schema)
		if e != nil {
			return e
		}
		defer raw.Close()
		temp, e := os.MkdirTemp(filepath.Dir(target), ".portable-schema-")
		if e != nil {
			return e
		}
		defer os.RemoveAll(temp)
		sqlite, e := openDB(temp)
		if e != nil {
			return e
		}
		defer sqlite.Close()
		scaffold, e := assembleApp(temp, temp, sqlite, storageHooks{})
		if e != nil {
			return e
		}
		defer scaffold.slicer.Close()
		if e = pgtransfer.Export(ctx, raw, sqlite); e != nil {
			return e
		}
		_, e = sqlite.ExecContext(ctx, "VACUUM INTO ?", target)
		return e
	}
	hooks.prepare = func(ctx context.Context, staged string) (_ func(bool), err error) {
		next, e := o.namespace(staged)
		if e != nil {
			return nil, e
		}
		if next == schema || next == o.schema {
			return nil, errors.New("restore must use a new PostgreSQL generation")
		}
		admin, e := postgres.Raw(ctx, o.url, schema)
		if e != nil {
			return nil, e
		}
		// Creating the namespace exclusively proves rollback can only remove
		// objects owned by this attempt. Existing generations remain protected.
		identifier := `"` + next + `"`
		if _, e = admin.ExecContext(ctx, "CREATE SCHEMA "+identifier); e != nil {
			admin.Close()
			return nil, e
		}
		finish := func(activated bool) {
			defer admin.Close()
			if activated {
				return
			}
			cleanup, stop := context.WithTimeout(context.Background(), 15*time.Second)
			defer stop()
			if _, e := admin.ExecContext(cleanup, "DROP SCHEMA "+identifier+" CASCADE"); e != nil {
				log.Printf("Could not remove staged PostgreSQL generation %s: %v", next, e)
			}
		}
		defer func() {
			if err != nil {
				finish(false)
			}
		}()
		restoreDB, h, e := o.generation(ctx, root, staged)
		if e != nil {
			return nil, e
		}
		defer restoreDB.Close()
		stagedApp, e := assembleApp(root, staged, restoreDB, h)
		if e != nil {
			return nil, e
		}
		stagedApp.slicer.Close()
		imported, e := h.importData(ctx)
		if e != nil {
			return nil, e
		}
		if !imported {
			return nil, errors.New("staged backup was not imported")
		}
		// Apply migrations with the restored rows and their keys before switching
		// the active filesystem generation.
		migrated, e := assembleApp(root, staged, restoreDB, h)
		if e != nil {
			return nil, e
		}
		migrated.slicer.Close()
		return finish, nil
	}
	return db, hooks, nil
}
