// Package testdb opens isolated release-schema databases for integration fixtures.
package testdb

import (
	"bambastic/internal/postgres"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	_ "modernc.org/sqlite"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func Open(t testing.TB) *sql.DB {
	t.Helper()
	ddl, err := os.ReadFile("../../spec/storage.sql")
	if err != nil {
		t.Fatal(err)
	}
	dsn := os.Getenv("BAMBASTIC_TEST_DATABASE_URL")
	if dsn == "" {
		db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "fixture.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_txlock=immediate")
		if e != nil {
			t.Fatal(e)
		}
		t.Cleanup(func() { db.Close() })
		// Match the running service: telemetry readers and a committing sampler
		// use separate connections and can overlap.
		db.SetMaxOpenConns(8)
		if _, e = db.Exec("PRAGMA journal_mode=WAL"); e != nil {
			t.Fatal(e)
		}
		if _, e = db.Exec(string(ddl)); e != nil {
			t.Fatal(e)
		}
		return db
	}
	var entropy [12]byte
	if _, e := rand.Read(entropy[:]); e != nil {
		t.Fatal(e)
	}
	schema := "bb_fixture_" + hex.EncodeToString(entropy[:])
	db, e := postgres.Open(t.Context(), dsn, schema)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, e := postgres.Raw(ctx, dsn, schema)
		if e == nil {
			defer raw.Close()
			_, e = raw.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		}
		if e != nil {
			t.Error(e)
		}
	})
	if e = postgres.Bootstrap(t.Context(), db, string(ddl)); e != nil {
		t.Fatal(e)
	}
	return db
}
