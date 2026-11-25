package control

import (
	"bambastic/internal/access"
	"bambastic/internal/openbridge"
	"database/sql"
	_ "modernc.org/sqlite"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegisteredGlyphCompatibility(t *testing.T) {
	dir := t.TempDir()
	db, e := sql.Open("sqlite", filepath.Join(dir, "db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	ddl, e := os.ReadFile("../../spec/storage.sql")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(string(ddl)); e != nil {
		t.Fatal(e)
	}
	a, e := access.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	s := New(db, dir, a)
	api, pub := http.NewServeMux(), http.NewServeMux()
	s.registerLinks(api)
	a.Register(pub, api)
	_, e = db.Exec("INSERT INTO external_links(name,url,icon,open_in_new_tab,sort_order) VALUES('Shop','https://example.invalid','link',0,0);INSERT INTO oidc_providers(name,issuer_url,client_id,client_secret,scopes,is_enabled,auto_create_users,auto_link_existing_accounts,icon_url,icon_data,icon_content_type,icon_etag) VALUES('Shop','https://issuer.invalid','x','secret','openid',1,0,0,'https://unrelated.invalid/raw.svg','<svg>raw markup</svg>','image/svg+xml','fixture')")
	if e != nil {
		t.Fatal(e)
	}
	admin := access.Principal{Admin: true}
	request := func(m *http.ServeMux, verb, path, body string, p access.Principal) *httptest.ResponseRecorder {
		r := httptest.NewRequest(verb, "/api/v1/"+path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		m.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
		return w
	}
	if w := request(api, "POST", "external-links/1/icon", `{"icon":"https://unrelated.invalid" }`, admin); w.Code != 422 {
		t.Fatal(w.Code)
	}
	if w := request(api, "POST", "external-links/1/icon", `{"icon":"openbridge:camera"}`, admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(api, "GET", "external-links/1/icon", "", admin); w.Body.String() != openbridge.Render("camera") {
		t.Fatal("Link icon is not the selected bundled glyph")
	}
	if w := request(api, "GET", "external-links/1/icon", "", access.Principal{}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	route := "auth/oidc/providers/1/icon"
	if w := request(pub, "GET", route, "", access.Principal{}); w.Body.String() != openbridge.Render("globe") {
		t.Fatal("Legacy image was served", w.Code)
	}
	if w := request(api, "POST", route+"/refresh", "", admin); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request(api, "DELETE", route, "", admin); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := request(pub, "GET", route, "", access.Principal{}); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
