package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/testdb"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type mediaFarm struct {
	mockFarm
	videos   map[string]string
	changing bool
	read     bool
}

func (f *mediaFarm) ListPrinterFiles(ctx context.Context, p access.Principal, id int64, dir string) ([]map[string]any, error) {
	var out []map[string]any
	if dir == "/timelapse/video" {
		for k, v := range f.videos {
			size := len(v)
			if f.read && f.changing {
				size++
			}
			out = append(out, object{"name": k, "size": size, "mtime": "2024-01-02T03:04:05Z"})
		}
	}
	return out, nil
}
func (f *mediaFarm) ReadPrinterFile(ctx context.Context, p access.Principal, id int64, name string, w io.Writer, limit int64) error {
	f.read = true
	_, e := io.WriteString(w, f.videos[filepath.Base(name)])
	return e
}
func mediaFixture(t *testing.T, f Farm) (*Service, *http.ServeMux, *http.ServeMux) {
	t.Helper()
	dir := t.TempDir()
	db := testdb.Open(t)
	auth, e := access.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO printers(name,serial_number,ip_address,access_code,model,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES('P1','SIM','127.0.0.1','fixture','P1',1,1,0,0,0,0,0,0,0);INSERT INTO users(username,password_hash,role,auth_source,is_active) VALUES('operator','unused','admin','local',1);INSERT INTO print_archives(filename,file_path,file_size,status,is_favorite,quantity,printer_id,created_by_id,source_3mf_path,timelapse_path,timelapse_baseline) VALUES('engine.3mf','engine.3mf',8,'archived',0,1,1,1,'source.3mf','video.mp4','[]')")
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"engine.3mf", "source.3mf", "video.mp4"} {
		if e = os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); e != nil {
			t.Fatal(e)
		}
	}
	s, e := New(db, dir, auth, f)
	if e != nil {
		t.Fatal(e)
	}
	public, api := http.NewServeMux(), http.NewServeMux()
	s.Register(public, api)
	public.Handle("/api/", auth.Protect(api))
	return s, public, api
}
func mediaDo(m *http.ServeMux, method, path string, p access.Principal) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest(method, path, nil)
	m.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	return w
}
func TestDownloadCredentialScopeAndReplay(t *testing.T) {
	s, pub, api := mediaFixture(t, &mediaFarm{})
	admin := access.Principal{ID: 1, Admin: true}
	base := "/api/v1/archives/1/"
	issue := func(route string) string {
		t.Helper()
		w := mediaDo(api, "POST", base+route, admin)
		var o struct{ Token string }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &o) != nil || o.Token == "" {
			t.Fatal(w.Code, w.Body.String())
		}
		return o.Token
	}
	tok := issue("slicer-token")
	get := func(s string) *httptest.ResponseRecorder { return mediaDo(pub, "GET", s, access.Principal{}) }
	for _, route := range []string{base + "source-dl/" + tok + "/s.3mf", "/api/v1/archives/2/dl/" + tok + "/x.3mf"} {
		if w := get(route); w.Code != 403 {
			t.Fatal(route, w.Code)
		}
	}
	for i := 0; i < 2; i++ {
		if w := get(base + "dl/" + tok + "/anything.3mf"); w.Code != 200 || w.Body.String() != "engine.3mf" {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if e := os.WriteFile(filepath.Join(s.dir, "engine.3mf"), []byte("replacement"), 0600); e != nil {
		t.Fatal(e)
	}
	if w := get(base + "dl/" + tok + "/engine.3mf"); w.Code != 403 {
		t.Fatal("stale link", w.Code)
	}
	if w := mediaDo(api, "POST", base+"slicer-token", access.Principal{ID: 2, Permissions: map[string]bool{"archives:read_own": true}}); w.Code != 404 {
		t.Fatal(w.Code)
	}
	tok = issue("media-download-token")
	var wg sync.WaitGroup
	counts := make(chan int, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); counts <- get(base + "media/dl/" + tok + "/video.mp4").Code }()
	}
	wg.Wait()
	close(counts)
	success := 0
	for c := range counts {
		if c == 200 {
			success++
		} else if c != 403 {
			t.Fatal(c)
		}
	}
	if success != 1 {
		t.Fatal("one-shot retrievals", success)
	}
}
func TestScanBaselineAmbiguityAndGrowingRecording(t *testing.T) {
	farm := &mediaFarm{videos: map[string]string{"old.mp4": "oldmovie", "a.mp4": "firstmovie", "b.avi": "othermovie"}}
	s, _, api := mediaFixture(t, farm)
	p := access.Principal{ID: 1, Admin: true}
	if _, err := s.db.Exec(`UPDATE print_archives SET timelapse_path=NULL,timelapse_baseline='["old.mp4"]'`); err != nil {
		t.Fatal(err)
	}
	url := "/api/v1/archives/1/timelapse/"
	w := mediaDo(api, "POST", url+"scan", p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "not_found") || strings.Contains(w.Body.String(), "old.mp4") {
		t.Fatal(w.Code, w.Body.String())
	}
	farm.changing = true
	if w = mediaDo(api, "POST", url+"select?filename=b.avi", p); w.Code != 409 {
		t.Fatal(w.Code, w.Body.String())
	}
	farm.changing = false
	w = mediaDo(api, "POST", url+"select?filename=b.avi", p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "attached") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "GET", strings.TrimSuffix(url, "/"), p)
	if w.Code != 200 || w.Body.String() != "othermovie" {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "POST", url+"scan", p)
	if !strings.Contains(w.Body.String(), "exists") {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestEditRealVideo(t *testing.T) {
	for _, cmd := range []string{"ffmpeg", "ffprobe"} {
		if _, e := exec.LookPath(cmd); e != nil {
			t.Skip(cmd + " unavailable")
		}
	}
	s, _, api := mediaFixture(t, &mediaFarm{})
	p := access.Principal{ID: 1, Admin: true}
	run := exec.Command("ffmpeg", "-y", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=blue:s=32x32:d=1", "-c:v", "libx264", filepath.Join(s.dir, "video.mp4"))
	if b, e := run.CombinedOutput(); e != nil {
		t.Fatal(e, string(b))
	}
	var body bytes.Buffer
	m := multipart.NewWriter(&body)
	_ = m.WriteField("save_mode", "replace")
	_ = m.WriteField("speed", "2")
	_ = m.Close()
	r := httptest.NewRequest("POST", "/api/v1/archives/1/timelapse/process", &body)
	r.Header.Set("Content-Type", m.FormDataContentType())
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code >= 300 {
		t.Fatal(w.Code, w.Body.String())
	}
	s.wg.Wait()
	w = mediaDo(api, "GET", "/api/v1/archives/1/timelapse/job", p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "completed") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "GET", "/api/v1/archives/1/timelapse/info", p)
	var info struct {
		Duration float64
		Width    int
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &info) != nil || info.Duration >= .9 || info.Width != 32 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "GET", "/api/v1/archives/1/timelapse/thumbnails?count=2", p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "timestamps") {
		t.Fatal(w.Code, w.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	s.Run(ctx)
}
