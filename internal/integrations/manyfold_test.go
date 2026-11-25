package integrations

import (
	"bambastic/internal/testdb"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"bambastic/internal/access"
	"bambastic/internal/production"
)

func fixture(t *testing.T) (*Service, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db := testdb.Open(t)
	auth, e := access.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	farm, e := production.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(db, dir, auth, farm)
	if e != nil {
		t.Fatal(e)
	}
	h := http.NewServeMux()
	s.Register(h)
	return s, h
}
func invoke(t *testing.T, h *http.ServeMux, p access.Principal, m, path string, body object) (int, object) {
	t.Helper()
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(m, "/api/v1/"+path, strings.NewReader(string(b)))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	var o object
	if w.Code != 204 {
		if e := json.Unmarshal(w.Body.Bytes(), &o); e != nil {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	return w.Code, o
}
func TestManyfoldRealOAuthAndImport(t *testing.T) {
	s, m := fixture(t)
	var signIns, downloads atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/oauth/token" {
			if r.FormValue("client_secret") != "fixture-only" || r.FormValue("scope") != "public read" {
				http.Error(w, "credentials", 400)
				return
			}
			signIns.Add(1)
			fmt.Fprint(w, `{"access_token":"example-token","expires_in":3600}`)
			return
		}
		if r.Header.Get("Authorization") != "Bearer example-token" {
			http.Error(w, "Unauthorized", 401)
			return
		}
		if !strings.Contains(r.URL.Path, "/raw/") && r.Header.Get("Accept") != "application/vnd.manyfold.v0+json" {
			http.Error(w, "Accept", 406)
			return
		}
		switch r.URL.Path {
		case "/models":
			fmt.Fprint(w, `{"member":[{"@id":"https://unused.example/models/42","name":"Clamp"}],"totalItems":1,"view":{}}`)
		case "/models/42":
			fmt.Fprint(w, `{"name":"Clamp","hasPart":[{"@id":"https://unused.example/models/42/model_files/7","name":"Clamp STL","encodingFormat":"model/stl"}]}`)
		case "/models/42/model_files/7":
			fmt.Fprint(w, `{"contentUrl":"https://untrusted.example/models/42/raw/Workshop/clamp.stl","encodingFormat":"model/stl","name":"Clamp"}`)
		case "/models/42/raw/Workshop/clamp.stl":
			downloads.Add(1)
			fmt.Fprint(w, "solid clamp\nendsolid clamp\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	admin := access.Principal{Admin: true}
	config := object{"url": remote.URL, "client_id": "fixture", "client_secret": "fixture-only"}
	code, v := invoke(t, m, admin, "POST", "manyfold/config/test", config)
	if code != 200 || intVal(v["model_count"]) != 1 {
		t.Fatal(code, v)
	}
	code, v = invoke(t, m, admin, "PUT", "manyfold/config", config)
	if code != 200 || v["has_client_secret"] != true {
		t.Fatal(code, v)
	}
	var encrypted string
	if e := s.db.QueryRow("SELECT value FROM settings WHERE key='manyfold_client_secret'").Scan(&encrypted); e != nil || encrypted == "fixture-only" {
		t.Fatal("secret not encrypted", e)
	}
	code, v = invoke(t, m, admin, "GET", "manyfold/models/42", nil)
	if code != 200 || v["name"] != "Clamp" {
		t.Fatal(code, v)
	}
	body := object{"model_id": "42", "file_id": "7"}
	code, v = invoke(t, m, admin, "POST", "manyfold/import", body)
	if code != 200 || v["was_existing"] != false || v["filename"] != "clamp.stl" {
		t.Fatal(code, v)
	}
	code, v = invoke(t, m, admin, "POST", "manyfold/import", body)
	if code != 200 || v["was_existing"] != true || signIns.Load() != 1 || downloads.Load() != 1 {
		t.Fatal(code, v, signIns.Load(), downloads.Load())
	}
	var source string
	if e := s.db.QueryRowContext(context.Background(), "SELECT source_url FROM library_files WHERE id=1").Scan(&source); e != nil || source != "manyfold:42/7" {
		t.Fatal(source, e)
	}
	code, _ = invoke(t, m, access.Principal{Permissions: map[string]bool{"manyfold:view": true}}, "POST", "manyfold/import", body)
	if code != 403 {
		t.Fatal("read permission imported a file")
	}
}
