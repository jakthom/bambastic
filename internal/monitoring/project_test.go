package monitoring

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProjectSourceReplacementAndPrivateImages(t *testing.T) {
	s, pub, api := mediaFixture(t, &mockFarm{})
	admin := access.Principal{ID: 1, Admin: true}
	var original bytes.Buffer
	z := zip.NewWriter(&original)
	model, _ := z.Create("3D/3dmodel.model")
	io.WriteString(model, `<model><metadata name="Title">Old title</metadata><resources><object id="1"/></resources><build/></model>`)
	picture, _ := z.Create("Auxiliaries/Model Pictures/shop.jpg")
	_, _ = picture.Write(testJPEG(false))
	private, _ := z.Create("Metadata/machine.json")
	io.WriteString(private, "private")
	_ = z.Close()
	if err := os.WriteFile(filepath.Join(s.dir, "engine.3mf"), original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PATCH", "/api/v1/archives/1/project-page", strings.NewReader(`{"title":"Lamps & cases","description":"<b>Functional</b>"}`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r.WithContext(access.With(r.Context(), admin)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Lamps") {
		t.Fatal(w.Code, w.Body.String())
	}
	var updated string
	if s.db.QueryRow("SELECT file_path FROM print_archives WHERE id=1").Scan(&updated) != nil || updated == "engine.3mf" {
		t.Fatal("File was not replaced")
	}
	b, _ := os.ReadFile(filepath.Join(s.dir, "engine.3mf"))
	if !bytes.Equal(b, original.Bytes()) {
		t.Fatal("Shared original was overwritten")
	}
	w = mediaDo(api, "GET", "/api/v1/archives/1/project-page", admin)
	var info struct {
		Title    string
		Pictures []object `json:"model_pictures"`
	}
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Title != "Lamps & cases" || len(info.Pictures) != 1 {
		t.Fatal(w.Body.String())
	}
	_, _ = s.db.Exec("INSERT INTO settings(key,value) VALUES('auth_enabled','true') ON CONFLICT(key) DO UPDATE SET value='true'")
	tok, e := s.auth.Mint(context.Background(), admin, "media", time.Minute, "")
	if e != nil {
		t.Fatal(e)
	}
	imgURL := asString(info.Pictures[0]["url"])
	if w = mediaDo(pub, "GET", imgURL, admin); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w = mediaDo(pub, "GET", imgURL+"?token="+tok, admin); w.Code != 200 || !bytes.Equal(w.Body.Bytes(), testJPEG(false)) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w = mediaDo(pub, "GET", "/api/v1/archives/1/project-image/Metadata%2Fmachine.json?token="+tok, admin); w.Code != 404 {
		t.Fatal(w.Code)
	}
}
