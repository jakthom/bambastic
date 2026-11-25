package cloudbackup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"bambastic/internal/access"
	_ "modernc.org/sqlite"
)

type fixture struct {
	db      *sql.DB
	mux     *http.ServeMux
	service *Service
}

func instance(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	db, e := sql.Open("sqlite", filepath.Join(dir, "test.sqlite")+"?_pragma=foreign_keys(1)")
	if e != nil {
		t.Fatal(e)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	ddl, e := os.ReadFile("../../spec/storage.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(ddl)); e != nil {
		t.Fatal(e)
	}
	auth, e := access.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(db, auth)
	if e != nil {
		t.Fatal(e)
	}
	m := http.NewServeMux()
	s.Register(m)
	return fixture{db, m, s}
}
func (f fixture) call(t *testing.T, p access.Principal, method, endpoint string, d any, want int) any {
	t.Helper()
	b, _ := json.Marshal(d)
	r := httptest.NewRequest(method, "/api/v1/github-backup/"+endpoint, bytes.NewReader(b))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != want {
		t.Fatalf("%s %s = %d %s", method, endpoint, w.Code, w.Body.String())
	}
	var v any
	if e := json.Unmarshal(w.Body.Bytes(), &v); e != nil {
		t.Fatal(e)
	}
	return v
}
func (f fixture) exec(t *testing.T, q string, args ...any) {
	t.Helper()
	if _, e := f.db.Exec(q, args...); e != nil {
		t.Fatal(e)
	}
}

type remote struct {
	mu      sync.Mutex
	files   map[string]string
	sha     string
	private bool
	commits int
	changed int
}

func (g *remote) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	base := "/api/v1/repos/my/config"
	p := strings.TrimPrefix(r.URL.Path, base)
	if r.Header.Get("Authorization") != "token git-personal-credential" {
		http.Error(w, "auth", 401)
		return
	}
	output := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case p == "" && r.Method == "GET":
		output(obj{"private": g.private, "full_name": "my/config", "permissions": obj{"push": true}, "default_branch": "main"})
	case p == "/commits" && r.Method == "GET":
		if g.sha == "" {
			output([]obj{})
			return
		}
		output([]obj{{"sha": g.sha, "commit": obj{"message": "Bambastic configuration backup", "author": obj{"name": "git", "date": "2026-11-24T00:00:00Z"}}}})
	case strings.HasPrefix(p, "/contents/") && r.Method == "GET":
		if r.URL.Query().Get("ref") != g.sha {
			http.NotFound(w, r)
			return
		}
		v, ok := g.files[strings.TrimPrefix(p, "/contents/")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		output(obj{"type": "file", "encoding": "base64", "content": base64.StdEncoding.EncodeToString([]byte(v))})
	case p == "/contents" && r.Method == "POST":
		var in struct {
			Branch string
			Files  []obj
		}
		if e := json.NewDecoder(r.Body).Decode(&in); e != nil || in.Branch != "main" {
			http.Error(w, "shape", 400)
			return
		}
		changed := map[string]string{}
		for _, x := range in.Files {
			name := str(x["path"])
			v, e := base64.StdEncoding.DecodeString(str(x["content"]))
			if e != nil {
				http.Error(w, "payload", 400)
				return
			}
			old, exists := g.files[name]
			if exists && (x["operation"] != "update" || x["sha"] != hash([]byte(old))) || !exists && x["operation"] != "create" {
				http.Error(w, "conflict", 409)
				return
			}
			changed[name] = string(v)
		}
		for k, v := range changed {
			g.files[k] = v
		}
		g.commits++
		g.changed = len(changed)
		g.sha = fmt.Sprintf("%040x", g.commits)
		output(obj{"commit": obj{"sha": g.sha}})
	default:
		http.NotFound(w, r)
	}
}

var admin = access.Principal{Admin: true}

func saveFixture(t *testing.T, f fixture, url string) {
	t.Helper()
	v := sub(f.call(t, admin, "POST", "config", obj{"access_token": "git-personal-credential", "repository_url": url + "/my/config", "provider": "gitea", "allow_insecure_http": true, "backup_settings": true, "backup_spools": true, "backup_archives": true}, 200))
	if !truth(v["has_token"]) || v["access_token"] != nil {
		t.Fatal(v)
	}
}
func TestSelectivePrivateBackupRestoreRoundTrip(t *testing.T) {
	git := &remote{files: map[string]string{"README.md": "private human notes"}, private: true}
	srv := httptest.NewServer(git)
	defer srv.Close()
	source := instance(t)
	saveFixture(t, source, srv.URL)
	var sealed string
	source.db.QueryRow("SELECT access_token FROM github_backup_config").Scan(&sealed)
	if strings.Contains(sealed, "git-personal-credential") {
		t.Fatal("credential stored in plain text")
	}
	source.exec(t, `INSERT INTO settings(key,value) VALUES('ha_token','saved-ha-credential'),('currency','EUR'),('ldap_enabled','true'),('external_url','https://person:login@example.test'),('config_obsolete','unknown-preference'),('bambu_cloud_token','cloud-specific-credential')`)
	source.service.CloudProfiles = func(_ context.Context, _ access.Principal, source string) (map[string][]map[string]any, string) {
		return map[string][]obj{"filament": {{"setting_id": "id1", "name": "Ocean", "is_custom": true, "base_id": "base"}, {"setting_id": "vendor", "is_custom": false}}}, "ok"
	}
	source.service.ResolvePreset = func(_ context.Context, p access.Principal, source, id, kind string) (map[string]any, error) {
		if id != "id1" || kind != "filament" {
			t.Fatalf("unrequested preset %s/%s", id, kind)
		}
		return obj{"filament_type": "PLA", "printhost_apikey": "slicer-credential", "nested": []any{"http://user:login@machine"}}, nil
	}
	source.exec(t, `INSERT INTO users(id,username,role,auth_source,is_active) VALUES(3,'alice','user','local',1);
	INSERT INTO spool(id,material,storage_location,label_weight,core_weight,weight_used,weight_locked,tag_uid) VALUES(8,'PLA','North',1000,200,21,0,'tag8');
	INSERT INTO print_archives(id,filename,file_path,file_size,status,is_favorite,quantity,created_by_id,confirm_token,thumbnail_path) VALUES(4,'cube.3mf','private/asset.3mf',48,'completed',1,1,3,'private-confirm','private/thumbnail.png');
	INSERT INTO print_log_entries(id,archive_id,print_name,status,created_by_id,created_by_username,billing_run_id) VALUES(9,4,'Cube','completed',3,'alice','private-billing');
	INSERT INTO spool_usage_history(id,spool_id,archive_id,weight_used,percent_used,status) VALUES(14,8,4,21,2,'completed');`)
	run := sub(source.call(t, admin, "POST", "run", nil, 200))
	if !truth(run["success"]) || ident(run["files_changed"]) != 8 {
		t.Fatal(run)
	}
	git.mu.Lock()
	all := ""
	for _, v := range git.files {
		all += v
	}
	version := git.sha
	git.mu.Unlock()
	for _, secret := range []string{"saved-ha-credential", "private/asset", "private-confirm", "private-billing", "unknown-preference", "person:login", "private/thumbnail", "slicer-credential", "cloud-specific-credential", "user:login", "vendor"} {
		if strings.Contains(all, secret) {
			t.Fatal("unexpected content in backup", secret)
		}
	}
	if !strings.Contains(all, mask) || !strings.Contains(all, "Ocean") {
		t.Fatal("missing masked preset")
	}
	if v := sub(source.call(t, admin, "POST", "run", nil, 200)); !truth(v["success"]) || ident(v["files_changed"]) != 0 {
		t.Fatal(v)
	}
	dest := instance(t)
	saveFixture(t, dest, srv.URL)
	dest.exec(t, `INSERT INTO users(id,username,role,auth_source,is_active) VALUES(91,'alice','user','local',1);
	INSERT INTO spool(id,material,label_weight,core_weight,weight_used,weight_locked) VALUES(8,'PETG',1000,200,0,0);
	INSERT INTO settings(key,value) VALUES('ha_token','local-token'),('currency','USD'),('ldap_enabled','false')`)
	v := sub(dest.call(t, admin, "GET", "restore/preview", nil, 200))
	if v["ref"] != version || !truth(v["success"]) {
		t.Fatal(v)
	}
	restore := obj{"ref": version, "categories": []string{"settings", "archives", "spools"}, "overwrite_existing": true}
	p := access.Principal{Permissions: map[string]bool{"github:restore": true, "inventory:update": true}}
	dest.call(t, p, "POST", "restore", restore, 403)
	report := sub(dest.call(t, admin, "POST", "restore", restore, 200))
	if !truth(report["success"]) {
		t.Fatal(report)
	}
	var loc, name, asset, runOwner, token, currency, ldap string
	var spoolID, archiveID, matchedSpool, matchedArchive, runArchive int64
	if e := dest.db.QueryRow("SELECT s.id,l.name FROM spool s JOIN locations l ON l.id=s.location_id WHERE s.tag_uid='tag8'").Scan(&spoolID, &loc); e != nil || loc != "North" || spoolID == 8 {
		t.Fatal(loc, spoolID, e, report)
	}
	if e := dest.db.QueryRow("SELECT a.id,a.file_path,u.username FROM print_archives a JOIN users u ON u.id=a.created_by_id").Scan(&archiveID, &asset, &name); e != nil || name != "alice" || asset != "" {
		t.Fatal(e, name, asset)
	}
	if e := dest.db.QueryRow("SELECT l.archive_id,u.username FROM print_log_entries l JOIN users u ON u.id=l.created_by_id").Scan(&runArchive, &runOwner); e != nil || runArchive != archiveID || runOwner != "alice" {
		t.Fatal(e, runArchive, runOwner)
	}
	if e := dest.db.QueryRow("SELECT spool_id,archive_id FROM spool_usage_history").Scan(&matchedSpool, &matchedArchive); e != nil || matchedSpool != spoolID || matchedArchive != archiveID {
		t.Fatal(matchedSpool, matchedArchive, e)
	}
	dest.db.QueryRow("SELECT value FROM settings WHERE key='ha_token'").Scan(&token)
	dest.db.QueryRow("SELECT value FROM settings WHERE key='currency'").Scan(&currency)
	dest.db.QueryRow("SELECT value FROM settings WHERE key='ldap_enabled'").Scan(&ldap)
	if token != "local-token" || currency != "EUR" || ldap != "false" {
		t.Fatal(token, currency, ldap)
	}
	restore["overwrite_existing"] = false
	report = sub(dest.call(t, admin, "POST", "restore", restore, 200))
	if !truth(report["success"]) {
		t.Fatal(report)
	}
	for _, k := range []string{"settings", "archives", "spools"} {
		if ident(sub(sub(report["results"])[k])["restored"]) != 0 {
			t.Fatal(report)
		}
	}
	git.mu.Lock()
	git.private = false
	git.mu.Unlock()
	report = sub(source.call(t, admin, "POST", "run", nil, 200))
	if truth(report["success"]) {
		t.Fatal("public repository accepted")
	}
	logs := source.call(t, admin, "GET", "logs?limit=1&offset=0", nil, 200).([]any)
	if len(logs) != 1 || sub(logs[0])["status"] != "failed" {
		t.Fatal(logs)
	}
	if out := sub(source.call(t, admin, "DELETE", "logs?keep_last=1", nil, 200)); ident(out["deleted"]) != 2 {
		t.Fatal(out)
	}
}

func TestGitHubAndGitLabWriteSelectiveCommits(t *testing.T) {
	for _, kind := range []string{"github", "gitlab"} {
		t.Run(kind, func(t *testing.T) {
			root := strings.Repeat("a", 40)
			tree := strings.Repeat("b", 40)
			commit := strings.Repeat("c", 40)
			created := false
			updated := false
			seeded := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var v obj
				if r.Body != nil && r.Method != "GET" {
					if e := json.NewDecoder(r.Body).Decode(&v); e != nil {
						t.Fatal(e)
					}
				}
				out := func(x any) { json.NewEncoder(w).Encode(x) }
				p := r.URL.Path
				if kind == "github" {
					p = strings.TrimPrefix(p, "/api/v3/repos/o/r")
					if r.Header.Get("Authorization") != "token test" {
						http.Error(w, "auth", 401)
						return
					}
					switch {
					case p == "/commits":
						http.Error(w, "empty", 409)
					case p == "":
						out(obj{"private": true, "default_branch": "main"})
					case r.Method == "PUT" && strings.HasPrefix(p, "/contents/"):
						if p == "/contents/"+metadataPath {
							t.Fatal("manifest published before contents")
						}
						seeded = true
						out(obj{"commit": obj{"sha": root}})
					case p == "/git/commits/"+root:
						out(obj{"tree": obj{"sha": tree}})
					case p == "/git/trees":
						if !seeded || v["base_tree"] != tree {
							t.Fatal(v)
						}
						created = true
						out(obj{"sha": tree})
					case p == "/git/commits":
						if !created || v["parents"].([]any)[0] != root {
							t.Fatal(v)
						}
						out(obj{"sha": commit})
					case p == "/git/refs/heads/main":
						if v["force"] != false || v["sha"] != commit {
							t.Fatal(v)
						}
						updated = true
						out(obj{})
					default:
						http.NotFound(w, r)
					}
				} else {
					p = strings.TrimPrefix(p, "/api/v4/projects/group/o/r")
					if r.Header.Get("PRIVATE-TOKEN") != "test" {
						http.Error(w, "auth", 401)
						return
					}
					switch {
					case p == "":
						out(obj{"visibility": "private", "default_branch": "main"})
					case p == "/repository/commits" && r.Method == "GET":
						if r.URL.Query().Get("ref_name") == "main" {
							out([]obj{{"id": root}})
						} else {
							out([]obj{})
						}
					case p == "/repository/branches":
						if v["branch"] != "backup" || v["ref"] != root {
							t.Fatal(v)
						}
						created = true
						out(obj{})
					case strings.HasPrefix(p, "/repository/files/"):
						if strings.HasSuffix(p, metadataPath) {
							out(obj{"encoding": "base64", "content": "e30=", "last_commit_id": tree})
						} else {
							http.NotFound(w, r)
						}
					case p == "/repository/commits":
						if !created || v["branch"] != "backup" {
							t.Fatal(v)
						}
						a := v["actions"].([]any)
						if len(a) != 2 {
							t.Fatal(v)
						}
						valid := false
						for _, item := range a {
							x := sub(item)
							if x["action"] == "update" && x["last_commit_id"] == tree {
								valid = true
							}
						}
						if !valid {
							t.Fatal("missing per-file concurrency guard")
						}
						updated = true
						out(obj{"id": commit})
					default:
						http.NotFound(w, r)
					}
				}
			}))
			defer srv.Close()
			url := srv.URL + "/o/r"
			branch := "main"
			if kind == "gitlab" {
				url = srv.URL + "/group/o/r"
				branch = "backup"
			}
			r, e := newRepo(url, kind, branch, "test", true)
			if e != nil {
				t.Fatal(e)
			}
			ref, n, e := r.push(context.Background(), map[string][]byte{metadataPath: []byte("metadata"), settingsPath: []byte("settings")})
			if e != nil || n != 2 || ref != commit || !updated {
				t.Fatal(ref, n, e)
			}
		})
	}
}

func TestCalibrationSnapshotUsesSerialAndFirmwareAcknowledgements(t *testing.T) {
	git := &remote{files: map[string]string{}, private: true}
	srv := httptest.NewServer(git)
	defer srv.Close()
	f := instance(t)
	saveFixture(t, f, srv.URL)
	f.exec(t, `INSERT INTO printers(name,serial_number,ip_address,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES('Printer','SIM123','127.0.0.1','credential-hidden',1,1,1,0,0,0,0,0,0)`)
	f.service.CalibrationConnected = func(k int64) bool { return k == 1 }
	f.service.KProfiles = func(_ context.Context, k int64, nozzle string) ([]map[string]any, error) {
		if k == 1 && nozzle == "0.4" {
			return []obj{{"name": "PLA Fine", "filament_id": "GFA00", "k_value": 0.025, "slot_id": 3, "private": "private-field"}}, nil
		}
		return nil, nil
	}
	report := sub(f.call(t, admin, "POST", "run", nil, 200))
	if !truth(report["success"]) {
		t.Fatal(report)
	}
	git.mu.Lock()
	contents := git.files["kprofiles/SIM123/0.4.json"]
	ref := git.sha
	git.mu.Unlock()
	if !strings.Contains(contents, "PLA Fine") || strings.Contains(contents, "private-field") {
		t.Fatal(contents)
	}
	calls := 0
	f.service.RestoreKProfiles = func(_ context.Context, k int64, nozzle string, records []obj) (bool, error) {
		calls++
		if k != 1 || nozzle != "0.4" || len(records) != 1 || records[0]["name"] != "PLA Fine" {
			t.Fatal(k, nozzle, records)
		}
		return false, nil
	}
	body := obj{"ref": ref, "categories": []string{"kprofiles"}}
	f.call(t, access.Principal{Permissions: map[string]bool{"github:restore": true}}, "POST", "restore", body, 403)
	out := sub(f.call(t, admin, "POST", "restore", body, 200))
	r := sub(sub(out["results"])["kprofiles"])
	if calls != 1 || ident(r["restored"]) != 1 || !strings.Contains(fmt.Sprint(r["notes"]), "kprofilesAckUnreliable") {
		t.Fatal(out)
	}
	f.service.CalibrationConnected = func(int64) bool { return false }
	out = sub(f.call(t, admin, "POST", "restore", body, 200))
	if calls != 1 || ident(sub(sub(out["results"])["kprofiles"])["skipped"]) != 1 {
		t.Fatal(out)
	}
}
