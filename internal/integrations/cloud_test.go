package integrations

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (fn transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }
func useRemote(s *Service, server *httptest.Server) {
	base, _ := url.Parse(server.URL)
	s.client.Transport = transportFunc(func(r *http.Request) (*http.Response, error) {
		copy := r.Clone(r.Context())
		copy.URL.Scheme = base.Scheme
		copy.URL.Host = base.Host
		return server.Client().Transport.RoundTrip(copy)
	})
}
func TestCloudPairingUserIsolationAndMakerWorld(t *testing.T) {
	s, h := fixture(t)
	for _, name := range []string{"first", "second"} {
		_, e := s.db.Exec("INSERT INTO users(username,password_hash,role,auth_source,is_active) VALUES(?,'unused','admin','local',1)", name)
		if e != nil {
			t.Fatal(e)
		}
	}
	// Vendor tokens and device codes can exceed the release schema's original
	// VARCHAR length hints once authenticated encryption is applied.
	bambuToken := strings.Repeat("fixture-token.", 80)
	deviceCode := strings.Repeat("fixture-verifier.", 12)
	refreshToken := strings.Repeat("fixture-refresh.", 24)
	var model bytes.Buffer
	z := zip.NewWriter(&model)
	entry, _ := z.Create("3D/3dmodel.model")
	entry.Write([]byte("<model/>"))
	z.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/design-user-service/my/preference":
			fmt.Fprint(w, `{"email":"first@example.test"}`)
		case "/v1/iot-service/api/slicer/setting/one":
			if r.Header.Get("Authorization") != "Bearer "+bambuToken {
				http.Error(w, "token", 403)
				return
			}
			fmt.Fprint(w, `{"setting":{"type":"process","layer_height":"0.12"}}`)
		case "/oauth/device/code":
			if r.FormValue("scope") != "sync:read" {
				t.Error("unexpected scope")
			}
			fmt.Fprintf(w, `{"device_code":%q,"user_code":"ABCDE","verification_uri":"https://www.orcaslicer.com","expires_in":600,"interval":5}`, deviceCode)
		case "/oauth/token":
			if r.FormValue("grant_type") == "refresh_token" {
				if r.FormValue("refresh_token") != refreshToken {
					t.Error("wrong refresh token")
				}
				fmt.Fprint(w, `{"access_token":"next-access","refresh_token":"next-refresh","expires_in":1800}`)
			} else {
				if r.FormValue("device_code") != deviceCode {
					t.Error("wrong pending code")
				}
				fmt.Fprintf(w, `{"access_token":"first-access","refresh_token":%q,"expires_in":1800}`, refreshToken)
			}
		case "/api/v1/external-apps/me":
			fmt.Fprint(w, `{"user_id":"or-123"}`)
		case "/api/v1/external/sync/pull":
			if r.Header.Get("Authorization") != "Bearer next-access" {
				http.Error(w, "{}", 401)
				return
			}
			fmt.Fprint(w, `{"upserts":[{"id":"orca1","name":"Speed","content":{"type":"print","layer_height":"0.15"}}]}`)
		case "/v1/design-service/design/101":
			fmt.Fprint(w, `{"modelId":"USabc","title":"Clamp","instances":[{"id":800,"profileId":99}]}`)
		case "/v1/design-service/design/101/instances":
			fmt.Fprint(w, `{"hits":[{"id":800,"profileId":99}]}`)
		case "/v1/design-service/instance/800/f3mf":
			if r.Header.Get("Authorization") != "Bearer "+bambuToken {
				http.Error(w, "{}", 403)
				return
			}
			fmt.Fprint(w, `{"url":"https://makerworld.bblmw.com/models/clamp.3mf?signature=example","name":"clamp.3mf"}`)
		case "/models/clamp.3mf":
			if r.Header.Get("Authorization") != "" {
				t.Error("bearer sent to download host")
			}
			w.Write(model.Bytes())
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	useRemote(s, server)
	p := access.Principal{ID: 1, Admin: true}
	other := access.Principal{ID: 2, Admin: true}
	code, v := invoke(t, h, p, "POST", "cloud/token", object{"access_token": bambuToken, "region": "global"})
	if code != 200 || v["is_authenticated"] != true {
		t.Fatal(code, v)
	}
	var stored string
	if e := s.db.QueryRow("SELECT cloud_token FROM users WHERE id=1").Scan(&stored); e != nil || stored == bambuToken {
		t.Fatal("unencrypted token", e)
	}
	if data, e := s.ResolvePreset(context.Background(), p, "cloud", "one", "process"); e != nil || data["layer_height"] != "0.12" {
		t.Fatal(data, e)
	}
	_, v = invoke(t, h, other, "GET", "cloud/status", nil)
	if v["is_authenticated"] != false {
		t.Fatal("other user inherited cloud account")
	}
	code, v = invoke(t, h, p, "POST", "orca-cloud/device/start", nil)
	if code != 200 || v["user_code"] != "ABCDE" {
		t.Fatal(code, v)
	}
	if _, ok := v["device_code"]; ok {
		t.Fatal("device secret exposed")
	}
	_, v = invoke(t, h, p, "POST", "orca-cloud/device/poll", nil)
	if v["connected"] != true {
		t.Fatal(v)
	}
	_ = s.saveCreds(context.Background(), 1, map[string]string{"expires": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)})
	groups, status := s.CloudProfiles(context.Background(), p, "orca_cloud")
	if status != "ok" || len(groups["process"]) != 1 {
		t.Fatal(status, groups)
	}
	c, _ := s.readCreds(context.Background(), 1, "refresh")
	if c["refresh"] != "next-refresh" {
		t.Fatal("refresh rotation lost")
	}
	if data, e := s.ResolvePreset(context.Background(), p, "orca_cloud", "orca1", "process"); e != nil || data["layer_height"] != "0.15" {
		t.Fatal(data, e)
	}
	code, v = invoke(t, h, p, "POST", "makerworld/resolve", object{"url": "makerworld.com/en/models/101-clamp#profileId-99"})
	if code != 200 || intVal(v["model_id"]) != 101 || len(v["instances"].([]any)) != 1 {
		t.Fatal(code, v)
	}
	code, v = invoke(t, h, p, "POST", "makerworld/import", object{"model_id": 101, "profile_id": 99})
	if code != 200 || v["filename"] != "clamp.3mf" {
		t.Fatal(code, v)
	}
	code, v = invoke(t, h, other, "POST", "makerworld/import", object{"model_id": 101, "profile_id": 99})
	if code != 403 {
		t.Fatal("credential leaked to other user", code, v)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), bambuToken) {
		t.Fatal("secret in output")
	}
}

func TestBambuAccountStatusAndUpstreamFailures(t *testing.T) {
	s, api := fixture(t)
	admin := access.Principal{Admin: true}
	for _, tc := range []struct {
		actor access.Principal
		want  int
	}{{admin, 401}, {access.Principal{Permissions: map[string]bool{"settings:read": true}}, 403}} {
		code, _ := invoke(t, api, tc.actor, "GET", "cloud/settings", nil)
		if code != tc.want {
			t.Fatalf("cloud/settings got %d, want %d", code, tc.want)
		}
	}
	code, none := invoke(t, api, admin, "GET", "cloud/status", nil)
	if code != 200 || none["is_authenticated"] != false || none["region"] != nil || none["email"] != nil || none["sign_in_expired"] != false {
		t.Fatal(code, none)
	}
	var mode atomic.Int32
	var probes atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/design-user-service/my/preference" {
			probes.Add(1)
		}
		switch mode.Load() {
		case 0:
			w.WriteHeader(200)
			io.WriteString(w, `{"email":"farm@example.test"}`)
		case 1:
			w.WriteHeader(503)
			io.WriteString(w, `{"error":"unavailable"}`)
		case 2:
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"edge route"}`)
		case 3:
			w.WriteHeader(401)
			io.WriteString(w, `{"code":4,"error":"Please login."}`)
		}
	}))
	defer remote.Close()
	useRemote(s, remote)
	refresh := func(token string) {
		t.Helper()
		if e := s.saveCreds(t.Context(), 0, map[string]string{"token": token, "expired": "", "region": "global", "email": "farm@example.test"}); e != nil {
			t.Fatal(e)
		}
	}
	check := func(auth, expired bool) {
		t.Helper()
		code, data := invoke(t, api, admin, "GET", "cloud/status", nil)
		if code != 200 || data["is_authenticated"] != auth || data["sign_in_expired"] != expired {
			t.Fatal(code, data)
		}
	}
	refresh("good-token")
	check(true, false)
	check(true, false)
	if probes.Load() != 1 {
		t.Fatal("valid credential probe was not cached")
	}
	refresh("rotated-token")
	mode.Store(1)
	check(true, false)
	code, _ = invoke(t, api, admin, "POST", "cloud/login", object{"email": "farm@example.test", "password": "x"})
	if code != 502 {
		t.Fatal("sign-in outage", code)
	}
	mode.Store(2)
	check(true, false)
	code, _ = invoke(t, api, admin, "GET", "cloud/settings", nil)
	if code != 502 {
		t.Fatal("unsigned upstream 401", code)
	}
	mode.Store(3)
	code, _ = invoke(t, api, admin, "GET", "cloud/settings", nil)
	if code != 401 {
		t.Fatal("signed expiry", code)
	}
	before := probes.Load()
	check(false, true)
	if probes.Load() != before {
		t.Fatal("re-probed a known expired account")
	}
	refresh("new-token")
	s.recordBambuExpiry(t.Context(), 0, "rotated-token")
	mode.Store(0)
	check(true, false)
}
