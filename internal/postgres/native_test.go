package postgres_test

import (
	"bambastic/internal/access"
	"bambastic/internal/management"
	"bambastic/internal/operations"
	"bambastic/internal/postgres"
	"bambastic/internal/production"
	"bambastic/internal/support"
	"bambastic/internal/virtualprinter"
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"

	"testing"
	"time"
)

func cluster(t *testing.T) string {
	t.Helper()
	if d := os.Getenv("BAMBASTIC_TEST_DATABASE_URL"); d != "" {
		return d
	}
	if _, e := exec.LookPath("initdb"); e != nil || os.Getuid() == 0 {
		t.Skip("local PostgreSQL initdb or BAMBASTIC_TEST_DATABASE_URL required")
	}
	dir, e := os.MkdirTemp("", "bb-native-pg-")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	data := filepath.Join(dir, "data")
	if out, e := exec.Command("initdb", "-D", data, "-A", "trust", "--no-locale", "--encoding=UTF8").CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	port := "55460"
	if out, e := exec.Command("pg_ctl", "-D", data, "-l", filepath.Join(dir, "server.log"), "-o", "-k "+dir+" -p "+port+" -c listen_addresses=''", "-w", "start").CombinedOutput(); e != nil {
		t.Fatal(string(out), e)
	}
	t.Cleanup(func() {
		if out, e := exec.Command("pg_ctl", "-D", data, "-m", "fast", "-w", "stop").CombinedOutput(); e != nil {
			t.Error(string(out), e)
		}
	})
	return "postgres:///postgres?host=" + url.QueryEscape(dir) + "&port=" + port
}
func setup(t *testing.T) (*sql.DB, string, string) {
	t.Helper()
	ctx := t.Context()
	dsn := cluster(t)
	schema := fmt.Sprintf("bb_test_%d", time.Now().UnixNano())
	db, e := postgres.Open(ctx, dsn, schema)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		db.Close()
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		raw, e := postgres.Raw(ctx, dsn, schema)
		if e == nil {
			defer raw.Close()
			_, e = raw.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		}
		if e != nil {
			t.Error(e)
		}
	})
	ddl, e := os.ReadFile("../../spec/storage.sql")
	if e != nil {
		t.Fatal(e)
	}
	if e = postgres.Bootstrap(ctx, db, string(ddl)); e != nil {
		t.Fatal(e)
	}
	return db, dsn, schema
}
func TestNativeServiceBootstrap(t *testing.T) {
	db, dsn, schema := setup(t)
	ctx := t.Context()
	dir := t.TempDir()
	if _, e := access.New(db, dir); e != nil {
		t.Fatal("access", e)
	}
	if _, e := management.New(db); e != nil {
		t.Fatal("management", e)
	}
	f, e := production.New(db, dir)
	if e != nil {
		t.Fatal("production", e)
	}
	if _, e = operations.New(db); e != nil {
		t.Fatal("operations", e)
	}
	if _, e = support.New(db); e != nil {
		t.Fatal("support", e)
	}
	if _, e = virtualprinter.New(db, f); e != nil {
		t.Fatal("vp", e)
	}
	owner, e := postgres.Acquire(ctx, dsn, schema)
	if e != nil {
		t.Fatal(e)
	}
	defer owner.Close()
	if l, e := postgres.Acquire(ctx, dsn, schema); e == nil {
		l.Close()
		t.Fatal("duplicate owner")
	}
	tx, e := db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback()
	res, e := tx.Exec("INSERT INTO printers(name,ip_address,serial_number,access_code,model,is_active,auto_archive,nozzle_count,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(?,?,?,?,?,1,1,1,0,0,0,0,0,0)", "fixture", "127.0.0.1", "abcde", "fixture", "X1C")
	if e != nil {
		t.Fatal(e)
	}
	inserted, e := res.LastInsertId()
	if e != nil || inserted != 1 {
		t.Fatal(inserted, e)
	}
	id, e := tx.Exec("INSERT INTO locations(name,name_key) VALUES(?,?)", "fixture", "fixture")
	if e != nil {
		t.Fatal(e)
	}
	n, e := id.LastInsertId()
	if e != nil || n != 1 {
		t.Fatal(n, e)
	}
	if _, e = tx.Exec("INSERT INTO locations(name,name_key) VALUES(?,?)", "fixture", "fixture"); e == nil {
		t.Fatal("unique constraint skipped")
	}
	var cid int64
	if e = tx.QueryRow("SELECT id FROM locations WHERE name_key IS ?", "fixture").Scan(&cid); e != nil || cid != 1 {
		t.Fatal(cid, e)
	}
	if e = tx.Commit(); e != nil {
		t.Fatal(e)
	}
}

func TestNativeIdentityAndConcurrentReservation(t *testing.T) {
	db, _, _ := setup(t)
	ctx := t.Context()
	if _, e := db.Exec("INSERT INTO locations(id,name,name_key) VALUES(400,'test','test');DELETE FROM locations WHERE id=400"); e != nil {
		t.Fatal(e)
	}
	var water int64
	if e := db.QueryRow("SELECT seq FROM sqlite_sequence WHERE name='locations'").Scan(&water); e != nil || water != 400 {
		t.Fatal(water, e)
	}
	res, e := db.Exec("INSERT INTO locations(name,name_key) VALUES('new','new')")
	if e != nil {
		t.Fatal(e)
	}
	id, e := res.LastInsertId()
	if e != nil || id != 401 {
		t.Fatal(id, e)
	}
	if _, e = db.Exec("CREATE TABLE IF NOT EXISTS bb_probe(id INTEGER PRIMARY KEY,qty INTEGER NOT NULL);INSERT INTO bb_probe(qty) VALUES(8)"); e != nil {
		t.Fatal(e)
	}
	errs := make(chan error, 8)
	for range 8 {
		go func() {
			tx, e := db.BeginTx(ctx, nil)
			if e != nil {
				errs <- e
				return
			}
			defer tx.Rollback()
			var qty int
			if e = tx.QueryRow("SELECT qty FROM bb_probe WHERE id=1").Scan(&qty); e == nil {
				_, e = tx.Exec("UPDATE bb_probe SET qty=? WHERE id=1", qty-1)
			}
			if e == nil {
				e = tx.Commit()
			}
			errs <- e
		}()
	}
	for range 8 {
		if e = <-errs; e != nil {
			t.Fatal(e)
		}
	}
	var got int
	if e = db.QueryRow("SELECT qty FROM bb_probe WHERE id=1").Scan(&got); e != nil || got != 0 {
		t.Fatal(got, e)
	}
	var equal bool
	if e = db.QueryRow("SELECT name=? COLLATE NOCASE FROM locations WHERE id=?", "NeW", 401).Scan(&equal); e != nil || !equal {
		t.Fatal(equal, e)
	}
}
