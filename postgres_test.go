package main

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bambastic/internal/backup"
	"bambastic/internal/postgres"
)

func testApplication(t *testing.T) (*application, error) {
	t.Helper()
	return testApplicationAt(t, t.TempDir())
}

func testApplicationAt(t *testing.T, dir string) (*application, error) {
	t.Helper()
	dsn := os.Getenv("BAMBASTIC_TEST_DATABASE_URL")
	if dsn == "" {
		return createApp(dir)
	}
	ctx := context.Background()
	schema := fmt.Sprintf("bb_test_%d", time.Now().UnixNano())
	opts := databaseOptions{url: dsn, schema: schema}
	t.Cleanup(func() {
		raw, e := postgres.Raw(ctx, dsn, schema)
		if e != nil {
			t.Error(e)
			return
		}
		defer raw.Close()
		schemas := []string{schema}
		if current, err := opts.namespace(dir); err == nil && current != schema {
			schemas = append(schemas, current)
		}
		if active, err := backup.Active(dir); err == nil && active != dir {
			if restored, err := opts.namespace(active); err == nil {
				schemas = append(schemas, restored)
			}
		}
		for _, name := range schemas {
			if _, err := raw.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+name+" CASCADE"); err != nil {
				t.Error(err)
			}
		}
	})
	return createConfiguredApp(ctx, dir, opts)
}
func TestPostgresApplication(t *testing.T) {
	if os.Getenv("BAMBASTIC_TEST_DATABASE_URL") == "" {
		t.Skip("set BAMBASTIC_TEST_DATABASE_URL for the PostgreSQL operator flows")
	}
	app, e := testApplication(t)
	if e != nil {
		t.Fatal(e)
	}
	defer app.close()
	server := httptest.NewServer(app.handler)
	defer server.Close()
	s := &testSystem{server, app, t}
	token := s.install()
	if other, err := createConfiguredApp(t.Context(), t.TempDir(), app.database); err == nil {
		other.close()
		t.Fatal("second service acquired the same PostgreSQL scheduler lease")
	}
	for _, route := range []string{"printers/", "archives/", "queue/", "notifications/", "projects/", "settings/", "groups/", "finance/cost-centers", "local-backup/status", "inventory/spools", "system/info", "maintenance/overview", "library/stats", "slicer-pipelines/", "virtual-printers", "api-keys/", "spoolbuddy/devices", "mqtt"} {
		if route == "mqtt" {
			route = "settings/mqtt/status"
		}
		s.call("GET", "/api/v1/"+route, "", "", token, 200)
	}
	if r := s.json("POST", "/api/v1/library/folders", `{"name":"Native PG models"}`, token, 200); r["id"] == nil {
		t.Fatal(r)
	}
	key := s.json("POST", "/api/v1/api-keys", `{"name":"Native service","can_read_status":true}`, token, 201)
	if !strings.HasPrefix(key["key"].(string), "bb_") {
		t.Fatal("key was not generated")
	}
}

func TestFailedPostgresRestoreLeavesActiveGeneration(t *testing.T) {
	if os.Getenv("BAMBASTIC_TEST_DATABASE_URL") == "" {
		t.Skip("set BAMBASTIC_TEST_DATABASE_URL for PostgreSQL restore checks")
	}
	app, err := testApplication(t)
	if err != nil {
		t.Fatal(err)
	}
	defer app.close()
	ctx := t.Context()
	staged := t.TempDir()
	if err = os.WriteFile(filepath.Join(staged, backup.RestoreIDFile), []byte("8a88e01d85ef81b9d16ea6bfccf98a27"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(staged, "bambastic.db"), []byte("not a portable database"), 0600); err != nil {
		t.Fatal(err)
	}
	finish, err := app.localBackup.PrepareRestore(ctx, staged)
	if err == nil {
		finish(false)
		t.Fatal("invalid restore succeeded")
	}
	name, err := app.database.namespace(staged)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := postgres.Raw(ctx, app.database.url, app.database.schema)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var exists bool
	if err = raw.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pg_namespace WHERE nspname=$1)", name).Scan(&exists); err != nil || exists {
		t.Fatal("failed generation survived rollback", err)
	}
	var current string
	if err = app.db.QueryRow("SELECT current_schema()").Scan(&current); err != nil || current != app.database.schema {
		t.Fatal("active generation changed", err)
	}
}
