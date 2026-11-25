package slicing

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type routeToFixture struct{ target *url.URL }

func (v routeToFixture) RoundTrip(r *http.Request) (*http.Response, error) {
	req := r.Clone(r.Context())
	u := *r.URL
	u.Host = v.target.Host
	u.Scheme = v.target.Scheme
	req.URL = &u
	return http.DefaultTransport.RoundTrip(req)
}
func TestPreviewOwnerAndCachedInheritance(t *testing.T) {
	s, mux := testSlicer(t)
	p := access.Principal{Admin: true}
	var input, output bytes.Buffer
	zipfile := func(b *bytes.Buffer, parts map[string]string) {
		z := zip.NewWriter(b)
		for k, v := range parts {
			e, err := z.Create(k)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.WriteString(e, v)
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
	}
	zipfile(&input, map[string]string{"Metadata/project_settings.config": `{"filament_type":["PLA","PETG"],"filament_colour":["blue","white"]}`})
	zipfile(&output, map[string]string{"Metadata/slice_info.config": `<config><plate><metadata key="index" value="1"/><filament id="2" used_g="1.5" type="PETG" color="white"/></plate></config>`})
	_, e := s.db.Exec("INSERT INTO print_archives(filename,file_path,file_size,status,is_favorite,quantity) VALUES('part.3mf','part.3mf',40,'archive',0,1)")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(s.dir, "part.3mf"), input.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	sidecar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/profiles/") {
			_ = json.NewEncoder(w).Encode(object{"name": "Base material", "filament_type": []string{"PLA"}, "filament_cost": []string{"10"}})
			return
		}
		if strings.HasPrefix(r.URL.Path, "/slice/progress/") {
			if strings.HasSuffix(r.URL.Path, "chosen") {
				t.Error("Client controlled the remote ID")
			}
			io.WriteString(w, `{"stage":"analysis","percent":34}`)
			return
		}
		if r.URL.Path != "/slice" {
			t.Error(r.URL.Path)
			w.WriteHeader(404)
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		defer r.MultipartForm.RemoveAll()
		if r.FormValue("requestId") == "chosen" || r.FormValue("plate") != "1" {
			t.Error("Incorrect sidecar request")
		}
		close(started)
		<-finish
		_, _ = w.Write(output.Bytes())
	}))
	defer sidecar.Close()
	u, _ := url.Parse(sidecar.URL)
	s.client = &http.Client{Transport: routeToFixture{u}}
	ctx := context.Background()
	result := make(chan []map[string]any, 1)
	go func() { v, _ := s.previewFilaments(ctx, p, "archives", 1, 1, "chosen", true); result <- v }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Sidecar was never called")
	}
	status, body := call(t, mux, p, "GET", "/api/v1/slicer/preview-progress/chosen", "")
	if status != 200 || body["stage"] != "analysis" {
		t.Fatal(status, body)
	}
	status, _ = call(t, mux, access.Principal{ID: 42, Admin: true}, "GET", "/api/v1/slicer/preview-progress/chosen", "")
	if status != 404 {
		t.Fatal(status)
	}
	close(finish)
	slots := <-result
	if len(slots) != 2 || slots[0]["used_in_plate"] != false || slots[1]["used_in_plate"] != true {
		t.Fatal(slots)
	}
	child := s.inheritBase(ctx, object{"inherits": "Base material", "name": "custom", "filament_cost": "12"}, "filament", map[string]bool{})
	if child["filament_cost"] != "12" || child["filament_type"] == nil {
		t.Fatal(child)
	}
	status, body = call(t, mux, p, "GET", "/api/v1/local-presets/base-cache/status", "")
	if status != 200 || body["fresh"] != float64(1) {
		t.Fatal(status, body)
	}
	status, body = call(t, mux, p, "POST", "/api/v1/local-presets/base-cache/refresh", "")
	if status != 200 || body["refreshed"] != float64(1) {
		t.Fatal(status, body)
	}
	s.Close()
}
