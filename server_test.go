package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"bambastic/internal/backup"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type testSystem struct {
	*httptest.Server
	app *application
	t   *testing.T
}

func startTestSystem(t *testing.T) *testSystem {
	t.Helper()
	return startTestSystemAtOrigin(t, "")
}
func startTestSystemAtOrigin(t *testing.T, origin string) *testSystem {
	t.Helper()
	t.Setenv("BAMBASTIC_PUBLIC_URL", origin)
	a, err := testApplication(t)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(a.handler)
	t.Cleanup(func() { server.Close(); a.close() })
	return &testSystem{server, a, t}
}
func TestMCPLoopbackProxyAuthority(t *testing.T) {
	const host = "farm.example.ts.net"
	const origin = "https://" + host
	listTools := func(t *testing.T, s *testSystem, authority, from, key string, want int) {
		t.Helper()
		r, err := http.NewRequest("POST", s.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
		if err != nil {
			t.Fatal(err)
		}
		r.Host = authority
		// Only Host can identify an explicitly configured proxy authority.
		r.Header.Set("X-Forwarded-Host", host)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		if from != "" {
			r.Header.Set("Origin", from)
		}
		if key != "" {
			r.Header.Set("Authorization", "Bearer "+key)
		}
		res, err := s.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil || res.StatusCode != want || (want == 200 && !bytes.Contains(body, []byte(`"list_categories"`))) {
			t.Fatalf("MCP through %s from %q: got %d, want %d: %s (%v)", authority, from, res.StatusCode, want, body, err)
		}
		if want == 401 && !strings.Contains(res.Header.Get("WWW-Authenticate"), origin+"/.well-known/oauth-protected-resource/mcp") {
			t.Fatalf("unexpected MCP authentication challenge: %s", res.Header.Get("WWW-Authenticate"))
		}
	}
	t.Run("default listener", func(t *testing.T) {
		s := startTestSystem(t)
		listTools(t, s, host, "", "", 403)
	})
	t.Run("configured proxy origin", func(t *testing.T) {
		s := startTestSystemAtOrigin(t, origin)
		listTools(t, s, host, origin, "", 200)
		listTools(t, s, "other.example.ts.net", "", "", 403)
		listTools(t, s, host+":8443", "", "", 403)
		listTools(t, s, host, "http://"+host, "", 403)
		listTools(t, s, host, "https://other.example.ts.net", "", 403)

		admin := s.install()
		listTools(t, s, host, origin, "", 401)
		info := s.json("GET", "/.well-known/oauth-protected-resource/mcp", "", "", 200)
		if info["resource"] != origin+"/mcp" {
			t.Fatalf("unexpected protected resource metadata: %v", info)
		}
		key := s.json("POST", "/api/v1/api-keys/", `{"name":"MCP test","can_read_status":true}`, admin, 201)["key"].(string)
		listTools(t, s, host, "", key, 200)
	})
}
func (s *testSystem) call(method, path, media, payload, token string, want int) ([]byte, http.Header) {
	s.t.Helper()
	r, err := http.NewRequest(method, s.URL+path, strings.NewReader(payload))
	if err != nil {
		s.t.Fatal(err)
	}
	r.Header.Set("Content-Type", media)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := s.Client().Do(r)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		s.t.Fatal(err)
	}
	if resp.StatusCode != want {
		s.t.Fatalf("%s %s: got %d, want %d: %.500s", method, path, resp.StatusCode, want, data)
	}
	return data, resp.Header
}
func (s *testSystem) json(method, path, body, token string, want int) map[string]any {
	s.t.Helper()
	data, _ := s.call(method, path, "application/json", body, token, want)
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		s.t.Fatal(err)
	}
	return result
}
func (s *testSystem) install() string {
	s.t.Helper()
	s.json("POST", "/api/v1/auth/setup", `{"auth_enabled":true,"admin_username":"operator","admin_password":"Local-test-pass-2468"}`, "", 200)
	return s.json("POST", "/api/v1/auth/login", `{"username":"operator","password":"Local-test-pass-2468"}`, "", 200)["access_token"].(string)
}
func TestPackagedSiteAndSetup(t *testing.T) {
	s := startTestSystem(t)
	if v := s.json("GET", "/api/v1/auth/status", "", "", 200); v["requires_setup"] != true {
		t.Fatal(v)
	}
	doc, headers := s.call("GET", "/printers", "", "", "", 200)
	if got := headers.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("SPA shell Cache-Control = %q", got)
	}
	match := regexp.MustCompile(`src="(/assets/[^"]+\.js)"`).FindSubmatch(doc)
	if match == nil {
		t.Fatal("deep link did not return the Vite shell")
	}
	for _, tc := range []struct {
		method, path string
		status       int
	}{
		{"GET", string(match[1]), 200},
		{"HEAD", string(match[1]), 200},
		{"GET", "/sw.js", 200},
		{"GET", "/assets/missing.js", 404},
		{"POST", "/", 405},
	} {
		_, headers := s.call(tc.method, tc.path, "", "", "", tc.status)
		if got := headers.Get("Cache-Control"); got != "no-store" {
			t.Errorf("%s %s Cache-Control = %q", tc.method, tc.path, got)
		}
	}
	s.json("GET", "/api/v1/missing", "", "", 404)
	session := s.install()
	s.call("GET", "/api/v1/printers/", "", "", "", 401)
	token := s.json("POST", "/api/v1/auth/ws-token", "{}", session, 200)["token"].(string)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(s.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(token), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if err = wsjson.Write(ctx, ws, map[string]string{"type": "ping"}); err != nil {
		t.Fatal(err)
	}
	var msg map[string]any
	for msg["type"] != "pong" {
		if err = wsjson.Read(ctx, ws, &msg); err != nil {
			t.Fatal(err)
		}
	}
	s.json("POST", "/api/v1/auth/logout", "{}", session, 200)
	s.call("GET", "/api/v1/printers/", "", "", session, 401)
}
func TestStdioChild(t *testing.T) {
	endpoint := os.Getenv("BAMBASTIC_STDIO_TEST_ENDPOINT")
	if endpoint == "" {
		return
	}
	if err := run(context.Background(), []string{"mcp", "--stdio", "--server", endpoint}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

type bearerClient struct{ token string }

func (b bearerClient) RoundTrip(r *http.Request) (*http.Response, error) {
	c := r.Clone(r.Context())
	c.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(c)
}
func checkMCP(t *testing.T, transport mcp.Transport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c := mcp.NewClient(&mcp.Implementation{Name: "integration-test", Version: "1"}, nil)
	sess, err := c.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	listed, err := sess.ListTools(ctx, &mcp.ListToolsParams{})
	if err != nil || len(listed.Tools) != 3 {
		t.Fatalf("tool discovery: %v, %v", listed, err)
	}
	for _, p := range []*mcp.CallToolParams{
		{Name: "list_categories", Arguments: map[string]any{}},
		{Name: "search_tools", Arguments: map[string]any{"query": "printers"}},
		{Name: "execute_tool", Arguments: map[string]any{"name": "list_printers", "arguments": map[string]any{}}},
	} {
		res, err := sess.CallTool(ctx, p)
		if err != nil {
			t.Fatal(err)
		}
		if res.IsError || res.StructuredContent == nil {
			t.Fatalf("%s: %+v", p.Name, res)
		}
	}
}
func TestMCPTransportsAndConsent(t *testing.T) {
	s := startTestSystem(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestStdioChild$")
	cmd.Env = append(os.Environ(), "BAMBASTIC_STDIO_TEST_ENDPOINT="+s.URL, "BAMBASTIC_API_KEY=")
	checkMCP(t, &mcp.CommandTransport{Command: cmd})
	session := s.install()
	_, h := s.call("POST", "/mcp", "application/json", "{}", "", 401)
	if !strings.Contains(h.Get("WWW-Authenticate"), "oauth-protected-resource") {
		t.Fatal(h)
	}
	const redirect = "http://127.0.0.1:32111/finish"
	id := s.json("POST", "/oauth/register", `{"client_name":"Workshop","redirect_uris":["`+redirect+`"]}`, "", 201)["client_id"].(string)
	verifier := strings.Repeat("a", 64)
	sum := sha256.Sum256([]byte(verifier))
	consent := map[string]string{"client_id": id, "redirect_uri": redirect, "code_challenge": base64.RawURLEncoding.EncodeToString(sum[:]), "code_challenge_method": "S256", "resource": s.URL + "/mcp", "response_type": "code", "decision": "allow", "state": "check"}
	data, _ := json.Marshal(consent)
	out := s.json("POST", "/api/v1/mcp/consent", string(data), session, 200)
	u, err := url.Parse(out["redirect_uri"].(string))
	if err != nil || u.Query().Get("state") != "check" {
		t.Fatal(out)
	}
	form := url.Values{"grant_type": {"authorization_code"}, "code": {u.Query().Get("code")}, "client_id": {id}, "redirect_uri": {redirect}, "resource": {s.URL + "/mcp"}, "code_verifier": {verifier}}
	tokenData, _ := s.call("POST", "/oauth/token", "application/x-www-form-urlencoded", form.Encode(), "", 200)
	var body struct {
		Token string `json:"access_token"`
	}
	if err = json.Unmarshal(tokenData, &body); err != nil || body.Token == "" {
		t.Fatal("missing OAuth token")
	}
	s.call("POST", "/oauth/token", "application/x-www-form-urlencoded", form.Encode(), "", 400)
	s.call("GET", "/api/v1/printers/", "", "", body.Token, 401)
	checkMCP(t, &mcp.StreamableClientTransport{Endpoint: s.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerClient{body.Token}}, DisableStandaloneSSE: true, MaxRetries: -1})
}
func TestPortableBackup(t *testing.T) {
	s := startTestSystem(t)
	token := s.install()
	data, _ := s.call("GET", "/api/v1/settings/backup", "", "", token, 200)
	if !bytes.HasPrefix(data, []byte("PK")) {
		t.Fatal("missing ZIP header")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "backup.zip")
	if err := os.WriteFile(file, data, 0600); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(dir, "restored")
	if err := backup.Restore(t.Context(), file, dest); err != nil {
		t.Fatal(err)
	}
	if err := backup.Restore(t.Context(), file, dest); err == nil {
		t.Fatal("overwrote existing installation")
	}
	a, err := createApp(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer a.close()
	var name string
	var count int
	if err = a.db.QueryRow("SELECT username FROM users").Scan(&name); err != nil || name != "operator" {
		t.Fatal(name, err)
	}
	if err = a.db.QueryRow("SELECT count(*) FROM bambastic_credentials").Scan(&count); err != nil || count != 0 {
		t.Fatal(count, err)
	}
}

func TestEnrollmentAndSingleUseRecovery(t *testing.T) {
	s := startTestSystem(t)
	session := s.install()
	enrolled := s.json("POST", "/api/v1/auth/2fa/totp/setup", "{}", session, 200)
	secret, e := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrolled["secret"].(string))
	if e != nil {
		t.Fatal(e)
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], uint64(time.Now().Unix()/30))
	hash := hmac.New(sha1.New, secret)
	hash.Write(counter[:])
	sum := hash.Sum(nil)
	offset := sum[len(sum)-1] & 15
	code := fmt.Sprintf("%06d", (binary.BigEndian.Uint32(sum[offset:offset+4])&0x7fffffff)%1000000)
	enabled := s.json("POST", "/api/v1/auth/2fa/totp/enable", `{"code":"`+code+`"}`, session, 200)
	backupCode := enabled["backup_codes"].([]any)[0].(string)
	client := s.Client()
	client.Jar, _ = cookiejar.New(nil)
	login := func() string {
		response := s.json("POST", "/api/v1/auth/login", `{"username":"operator","password":"Local-test-pass-2468"}`, "", 200)
		if response["requires_2fa"] != true || response["access_token"] != nil {
			t.Fatal("second factor bypassed")
		}
		return response["pre_auth_token"].(string)
	}
	verify := func(token, method, code string, want int) map[string]any {
		req, _ := json.Marshal(map[string]string{"pre_auth_token": token, "method": method, "code": code})
		return s.json("POST", "/api/v1/auth/2fa/verify", string(req), "", want)
	}
	token := login()
	verify(token, "totp", code, 401)
	result := verify(token, "backup", backupCode, 200)
	if result["access_token"] == "" {
		t.Fatal("missing session")
	}
	verify(token, "backup", backupCode, 401)
	verify(login(), "backup", backupCode, 401)
}

func TestRestoreActivationAndRetention(t *testing.T) {
	s := startTestSystem(t)
	admin := s.install()
	if _, e := s.app.db.Exec("INSERT INTO settings(key,value) VALUES('local_backup_retention','1')"); e != nil {
		t.Fatal(e)
	}
	getName := func() string {
		return s.json("POST", "/api/v1/local-backup/run", "{}", admin, 200)["filename"].(string)
	}
	_ = getName()
	s.json("POST", "/api/v1/library/folders", `{"name":"Printer fixtures"}`, admin, 200)
	current := getName()
	if _, e := s.app.db.Exec("UPDATE library_folders SET name=? WHERE name=?", "Newer state", "Printer fixtures"); e != nil {
		t.Fatal(e)
	}
	v, _ := s.call("GET", "/api/v1/local-backup/backups", "", "", admin, 200)
	var entries []map[string]any
	if e := json.Unmarshal(v, &entries); e != nil || len(entries) != 1 || entries[0]["filename"] != current {
		t.Fatal(string(v), e)
	}
	// The live installation is retained when the restore pointer is switched. On
	// the next start, the original data-directory lock still protects dispatch.
	root := s.app.localBackup.Root
	if _, e := createApp(root); e == nil {
		t.Fatal("started a second scheduler")
	}
	s.json("POST", "/api/v1/local-backup/backups/"+current+"/restore", "{}", admin, 200)
	dest, e := backup.Active(root)
	if e != nil || dest == root {
		t.Fatal(dest, e)
	}
	s.app.close()
	restored, e := createConfiguredApp(context.Background(), root, s.app.database)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.close()
	if restored.localBackup.Data != dest {
		t.Fatal("discarded active restore pointer")
	}
	var folder string
	if e = restored.db.QueryRow("SELECT name FROM library_folders WHERE name=?", "Printer fixtures").Scan(&folder); e != nil {
		t.Fatal("restored generation did not retain the snapshot", e)
	}
	var count int
	if e = restored.db.QueryRow("SELECT count(*) FROM bambastic_credentials").Scan(&count); e != nil || count != 0 {
		t.Fatal(count, e)
	}
}

func TestDashboardServicesAndPermissionBoundaries(t *testing.T) {
	s := startTestSystem(t)
	for _, path := range []string{"settings/ui-preferences", "settings/ui-flags", "settings/network-interfaces", "settings/spoolman", "system/info", "system/storage-usage", "maintenance/overview", "maintenance/summary", "printer-locations/", "local-backup/status", "smart-plugs/", "bulk/card-plugs?ids=1", "bulk/ha-sensor-readings?ids=1", "ha-sensors/", "location-ha-sensors/", "virtual-printers", "slicer-pipelines/", "auth/oidc/providers"} {
		data, _ := s.call("GET", "/api/v1/"+path, "", "", "", 200)
		if !json.Valid(data) {
			t.Fatalf("%s did not return JSON", path)
		}
	}
	admin := s.install()
	s.json("POST", "/api/v1/printer-locations/", `{"name":"Studio","color":"#006699"}`, admin, 201)
	s.json("PATCH", "/api/v1/printer-locations/", `{"name":"Studio","new_name":"Workshop"}`, admin, 200)
	removed := s.json("POST", "/api/v1/printer-locations/delete", `{"names":["Workshop"]}`, admin, 200)
	if removed["deleted"] != float64(1) {
		t.Fatal(removed)
	}
}

func TestProjectZIPRoundtrip(t *testing.T) {
	s := startTestSystem(t)
	var source bytes.Buffer
	writer := zip.NewWriter(&source)
	entries := map[string]string{"project.json": `{"name":"Valve jig","bom_items":[{"name":"M3 bolt","quantity_needed":2}],"linked_folders":[{"name":"Fixtures","files":[{"filename":"readme.txt","notes":"Operator instructions"}]}]}`, "files/Fixtures/readme.txt": "Inspect finished fixture."}
	for path, content := range entries {
		w, e := writer.Create(path)
		if e != nil {
			t.Fatal(e)
		}
		io.WriteString(w, content)
	}
	if e := writer.Close(); e != nil {
		t.Fatal(e)
	}
	var payload bytes.Buffer
	parts := multipart.NewWriter(&payload)
	p, e := parts.CreateFormFile("file", "jig.zip")
	if e != nil {
		t.Fatal(e)
	}
	p.Write(source.Bytes())
	parts.Close()
	data, _ := s.call("POST", "/api/v1/projects/import/file", parts.FormDataContentType(), payload.String(), "", 200)
	var project map[string]any
	if e = json.Unmarshal(data, &project); e != nil {
		t.Fatal(e)
	}
	id := fmt.Sprint(project["id"])
	if id == "<nil>" {
		t.Fatal(string(data))
	}
	archive, _ := s.call("GET", "/api/v1/projects/"+id+"/export", "", "", "", 200)
	z, e := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if e != nil {
		t.Fatal(e)
	}
	got := map[string]string{}
	for _, f := range z.File {
		in, e := f.Open()
		if e != nil {
			t.Fatal(e)
		}
		b, e := io.ReadAll(in)
		in.Close()
		if e != nil {
			t.Fatal(e)
		}
		got[f.Name] = string(b)
	}
	if got["files/Fixtures/readme.txt"] != entries["files/Fixtures/readme.txt"] || !strings.Contains(got["project.json"], "M3 bolt") {
		t.Fatal(got)
	}
}

func TestMCPReviewedMultiPlateQueue(t *testing.T) {
	s := startTestSystem(t)
	admin := s.install()
	for i := 1; i <= 2; i++ {
		_, e := s.app.db.Exec(`INSERT INTO printers(id,name,serial_number,ip_address,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(?,?,?,?,?,1,1,1,0,0,0,0,0,0)`, i, fmt.Sprintf("Fixture %d", i), fmt.Sprintf("SIM%d", i), "127.0.0.1", "fixture")
		if e != nil {
			t.Fatal(e)
		}
	}
	grp := s.json("POST", "/api/v1/groups", `{"name":"Submitters","permissions":["api_keys:create","printers:read","archives:read_all","archives:reprint_all","queue:create","queue:read_own","queue:update_own"],"restrict_printers":true,"printer_ids":[1]}`, admin, 201)
	s.json("POST", "/api/v1/users", fmt.Sprintf(`{"username":"trainee","password":"Example-long-2857","group_ids":[%.0f]}`, grp["id"]), admin, 201)
	session := s.json("POST", "/api/v1/auth/login", `{"username":"trainee","password":"Example-long-2857"}`, "", 200)["access_token"].(string)
	key := s.json("POST", "/api/v1/api-keys", `{"name":"Workshop","printer_ids":[1],"can_read_status":true,"can_queue":true}`, session, 201)
	reviewer := s.json("POST", "/api/v1/api-keys", `{"name":"Reviewer","can_queue":true}`, admin, 201)["key"].(string)
	var zipBytes, upload bytes.Buffer
	z := zip.NewWriter(&zipBytes)
	for i := 1; i <= 2; i++ {
		d, _ := z.Create(fmt.Sprintf("Metadata/plate_%d.gcode", i))
		fmt.Fprintln(d, "; fixture\nG1 X2")
	}
	z.Close()
	form := multipart.NewWriter(&upload)
	f, _ := form.CreateFormFile("file", "bracket.3mf")
	f.Write(zipBytes.Bytes())
	form.Close()
	b, _ := s.call("POST", "/api/v1/archives/upload", form.FormDataContentType(), upload.String(), admin, 200)
	var asset map[string]any
	json.Unmarshal(b, &asset)
	aid := asset["id"]
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "operator-acceptance", Version: "1"}, nil)
	connect := func(token string, stdio bool) *mcp.ClientSession {
		var tr mcp.Transport
		if stdio {
			cmd := exec.Command(os.Args[0], "-test.run=^TestStdioChild$")
			cmd.Env = append(os.Environ(), "BAMBASTIC_STDIO_TEST_ENDPOINT="+s.URL, "BAMBASTIC_API_KEY="+token)
			tr = &mcp.CommandTransport{Command: cmd}
		} else {
			tr = &mcp.StreamableClientTransport{Endpoint: s.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerClient{token}}, DisableStandaloneSSE: true, MaxRetries: -1}
		}
		ss, e := client.Connect(ctx, tr, nil)
		if e != nil {
			t.Fatal(e)
		}
		return ss
	}
	call := func(ss *mcp.ClientSession, name string, args map[string]any, success bool) string {
		t.Helper()
		v, e := ss.CallTool(ctx, &mcp.CallToolParams{Name: "execute_tool", Arguments: map[string]any{"name": name, "arguments": args}})
		if e != nil {
			t.Fatal(e)
		}
		b, _ := json.Marshal(v.Content)
		if v.IsError == success {
			t.Fatalf("%s: %s", name, b)
		}
		if !success {
			detail, _ := json.Marshal(v.StructuredContent)
			var report struct {
				Error struct {
					Operation, Code string
					Status          int `json:"http_status"`
				} `json:"error"`
			}
			if e = json.Unmarshal(detail, &report); e != nil || report.Error.Operation != name || report.Error.Code == "" || report.Error.Status < 400 {
				t.Fatalf("missing actionable MCP error for %s: %s", name, detail)
			}
		}
		txt, ok := v.Content[0].(*mcp.TextContent)
		if !ok {
			t.Fatal(v.Content)
		}
		return txt.Text
	}
	writer := connect(key["key"].(string), true)
	defer writer.Close()
	network := connect(key["key"].(string), false)
	defer network.Close()
	manager := connect(reviewer, false)
	defer manager.Close()
	if got := call(writer, "list_printers", map[string]any{}, true); !strings.Contains(got, "Fixture 1") || strings.Contains(got, "Fixture 2") {
		t.Fatal(got)
	}
	call(network, "get_printer_status", map[string]any{"printer_id": 1}, true)
	if got := call(writer, "list_archive_plates", map[string]any{"archive_id": aid}, true); !strings.Contains(got, "Plate 2") {
		t.Fatal(got)
	}
	args := map[string]any{"archive_id": aid, "printer_id": 1, "quantity": 2, "plate_id": 2, "request_key": "bracket-run"}
	first := call(writer, "submit_queue_item", args, true)
	second := call(network, "submit_queue_item", args, true)
	readIDs := func(v string) []int {
		var row struct {
			IDs []int `json:"queue_item_ids"`
		}
		if json.Unmarshal([]byte(v), &row) != nil || len(row.IDs) != 2 {
			t.Fatal(v)
		}
		return row.IDs
	}
	ids := readIDs(first)
	if fmt.Sprint(readIDs(second)) != fmt.Sprint(ids) {
		t.Fatal(first, second)
	}
	detail := s.json("GET", fmt.Sprintf("/api/v1/queue/%d", ids[0]), "", session, 200)
	if detail["plate_id"] != float64(2) || detail["manual_start"] != true {
		t.Fatal(detail)
	}
	for _, id := range ids {
		item := map[string]any{"item_id": id}
		call(writer, "start_queue_item", item, false)
		call(manager, "start_queue_item", item, true)
	}
	var count, held int
	if e := s.app.db.QueryRow("SELECT count(*),sum(manual_start) FROM print_queue").Scan(&count, &held); e != nil || count != 2 || held != 0 {
		t.Fatal(count, held, e)
	}
	s.json("PATCH", fmt.Sprintf("/api/v1/api-keys/%.0f", key["id"]), `{"can_queue":false}`, admin, 200)
	args["request_key"] = "now-denied"
	call(writer, "submit_queue_item", args, false)
	call(network, "submit_queue_item", args, false)
}

func TestReportReceiverMilestonesAndReset(t *testing.T) {
	s := startTestSystem(t)
	admin := s.install()
	if s.json("GET", "/api/v1/sponsor-prompt/check", "", admin, 200)["show"] != false {
		t.Fatal("unexpected milestone")
	}
	for i := 0; i < 10; i++ {
		if _, e := s.app.db.Exec("INSERT INTO print_log_entries(status,created_by_id) VALUES('completed',1)"); e != nil {
			t.Fatal(e)
		}
	}
	d := s.json("GET", "/api/v1/sponsor-prompt/check", "", admin, 200)
	if d["milestone"] != "prints-10" {
		t.Fatal(d)
	}
	s.call("POST", "/api/v1/sponsor-prompt/dismiss", "application/json", `{"milestone":"prints-10"}`, admin, 204)
	if s.json("GET", "/api/v1/sponsor-prompt/check", "", admin, 200)["show"] != false {
		t.Fatal("cooldown")
	}
	r := s.json("POST", "/api/v1/bug-report/start-logging", "{}", admin, 200)
	if r["started"] != true {
		t.Fatal(r)
	}
	s.json("POST", "/api/v1/bug-report/stop-logging?was_debug=false", "{}", admin, 200)
	receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var v map[string]any
		if e := json.NewDecoder(r.Body).Decode(&v); e != nil || v["description"] != "Camera preview stalled" || v["support_info"] != nil {
			t.Error(v, e)
		}
		io.WriteString(w, `{"issue_url":"https://example.test/issues/7","issue_number":7}`)
	}))
	defer receiver.Close()
	t.Setenv("BAMBASTIC_REPORT_URL", receiver.URL)
	r = s.json("POST", "/api/v1/bug-report/submit", `{"description":"Camera preview stalled","include_support_info":false}`, admin, 200)
	if r["issue_number"] != float64(7) {
		t.Fatal(r)
	}
	s.json("POST", "/api/v1/settings/reset", "{}", admin, 200)
	s.call("GET", "/api/v1/printers/", "", "", "", 401)
}
