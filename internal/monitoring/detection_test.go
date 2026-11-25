package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/operations"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type inspectingFarm struct {
	state, task string
	actions     []string
}

func (f *inspectingFarm) InitialStatus(context.Context, access.Principal) ([]map[string]any, error) {
	return []object{{"id": 1, "state": f.state, "subtask_name": "shop", "subtask_id": f.task, "connected": true}}, nil
}
func (f *inspectingFarm) OnDetection(ctx context.Context, id int64, task, name, mode, provider, verdict string) error {
	f.actions = append(f.actions, mode)
	return nil
}
func testJPEG(light bool) []byte {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			v := uint8(x + y)
			if light && x > 25 && x < 42 && y > 30 && y < 46 {
				v = 255
			}
			img.Set(x, y, color.RGBA{v, v, v, 255})
		}
	}
	var b bytes.Buffer
	_ = jpeg.Encode(&b, img, nil)
	return b.Bytes()
}
func putML(t *testing.T, s *Service, k, v string) {
	t.Helper()
	_, e := s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", k, v)
	if e != nil {
		t.Fatal(e)
	}
}
func cameraFixture(t *testing.T, s *Service, target string) {
	t.Helper()
	_, err := s.db.Exec("CREATE TABLE IF NOT EXISTS bb_printer_trust(printer_id INTEGER,fingerprint TEXT); UPDATE printers SET external_camera_enabled=1,external_camera_url=?,external_camera_type='snapshot' WHERE id=1", target)
	if err != nil {
		t.Fatal(err)
	}
}

type replaceTransport struct{ to *url.URL }

func (tr replaceTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Scheme = tr.to.Scheme
	u.Host = tr.to.Host
	copy.URL = &u
	return http.DefaultTransport.RoundTrip(copy)
}
func TestObicoOneShotFramesAndSmoothing(t *testing.T) {
	farm := &inspectingFarm{state: "RUNNING", task: "42"}
	s, pub, api := mediaFixture(t, farm)
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG(false))
	}))
	defer cam.Close()
	cameraFixture(t, s, cam.URL)
	server := httptest.NewServer(pub)
	defer server.Close()
	used := 0
	ml := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer shared" {
			w.WriteHeader(401)
			return
		}
		if r.URL.Path == "/hc/" {
			_, _ = io.WriteString(w, "ok")
			return
		}
		if img := r.URL.Query().Get("img"); img != "" {
			res, e := http.Get(img)
			if e != nil {
				t.Error(e)
				w.WriteHeader(500)
				return
			}
			b, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if res.StatusCode != 200 || !bytes.Equal(b, testJPEG(false)) {
				t.Error("wrong camera payload")
			}
			res, e = http.Get(img)
			if e != nil {
				t.Error(e)
				return
			}
			res.Body.Close()
			if res.StatusCode != 404 {
				t.Error("callback was reusable")
			}
			used++
		}
		_, _ = io.WriteString(w, `{"detections":[["failure",0.8,[]]]}`)
	}))
	defer ml.Close()
	for k, v := range map[string]string{"obico_enabled": "true", "obico_ml_url": ml.URL, "obico_ml_token": s.auth.SealSecret("shared"), "bambuddy_internal_url": server.URL, "obico_action": "pause", "obico_enabled_printers": "[1]"} {
		putML(t, s, k, v)
	}
	c, e := s.detectorSettings(context.Background(), "obico")
	if e != nil {
		t.Fatal(e)
	}
	jobs := map[int64]*inspection{}
	s.inspectFarm(context.Background(), c, jobs, map[int64]*rolling{})
	if used != 1 || s.detectors.status["obico"][1]["class"] != "safe" || len(farm.actions) != 0 {
		t.Fatal(used, s.detectors.status, farm.actions)
	}
	reader := access.Principal{ID: 1, Permissions: map[string]bool{"printers:read": true}, PrinterIDs: map[int64]bool{2: true}}
	w := mediaDo(api, "GET", "/api/v1/obico/printer-status", reader)
	if w.Code != 200 || strings.Contains(w.Body.String(), "frame_count") || strings.Contains(w.Body.String(), ml.URL) {
		t.Fatal(w.Code, w.Body.String())
	}
	bad := httptest.NewRequest("POST", "/api/v1/obico/test-connection", strings.NewReader(`{"url":"http://changed.invalid"}`))
	w = httptest.NewRecorder()
	api.ServeHTTP(w, bad.WithContext(access.With(bad.Context(), access.Principal{Admin: true})))
	if w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	i := &inspection{long: &rolling{}}
	for n := 0; n < 32; n++ {
		i.classify(0, "medium")
	}
	var verdict string
	for n := 0; n < 7; n++ {
		_, verdict = i.classify(3, "medium")
	}
	if verdict != "failure" {
		t.Fatal(verdict)
	}
}
func TestGadgetContextProcessingAndJobGuard(t *testing.T) {
	farm := &inspectingFarm{state: "RUNNING", task: "31"}
	s, _, _ := mediaFixture(t, farm)
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG(false))
	}))
	defer cam.Close()
	cameraFixture(t, s, cam.URL)
	badHost := true
	swap := false
	calls := 0
	gadget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "known" {
			t.Error("missing destination-scoped key")
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/createcontext") {
			target := "https://trusted.octoeverywhere.com/process"
			if badHost {
				target = "https://unrelated.invalid/process"
			}
			_ = json.NewEncoder(w).Encode(object{"ContextId": "fixture", "ProcessRequestUrl": target, "FallbackProcessRequestUrl": target})
			return
		}
		file, _, e := r.FormFile("image")
		if e != nil {
			t.Error(e)
			w.WriteHeader(400)
			return
		}
		b, _ := io.ReadAll(file)
		file.Close()
		if !bytes.Equal(b, testJPEG(false)) {
			t.Error("camera bytes missing")
		}
		if swap {
			farm.task = "changed"
		}
		calls++
		_, _ = io.WriteString(w, `{"PrintQuality":2,"WarningSuggested":true,"PauseSuggested":true,"NextProcessIntervalSec":{"Minimum":23}}`)
	}))
	defer gadget.Close()
	u, _ := url.Parse(gadget.URL)
	s.detectors.client = &http.Client{Transport: replaceTransport{u}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for k, v := range map[string]string{"octoeverywhere_enabled": "true", "octoeverywhere_api_key": s.auth.SealSecret("known"), "octoeverywhere_action": "pause"} {
		putML(t, s, k, v)
	}
	c, _ := s.detectorSettings(context.Background(), "octoeverywhere")
	jobs := map[int64]*inspection{}
	base := map[int64]*rolling{}
	s.inspectFarm(context.Background(), c, jobs, base)
	if calls != 0 || s.detectors.status[c.kind][1]["class"] != "error" {
		t.Fatal(calls, s.detectors.status)
	}
	badHost = false
	swap = true
	jobs[1].next = time.Time{}
	s.inspectFarm(context.Background(), c, jobs, base)
	if calls != 1 || len(farm.actions) > 0 {
		t.Fatal("stale print action", calls, farm.actions)
	}
	swap = false
	s.inspectFarm(context.Background(), c, jobs, base)
	if calls != 2 || len(farm.actions) != 1 || farm.actions[0] != "pause" || time.Until(jobs[1].next) < 22*time.Second {
		t.Fatal(calls, farm.actions, jobs)
	}
	putML(t, s, "obico_enabled", "true")
	c, _ = s.detectorSettings(context.Background(), c.kind)
	if c.enabled {
		t.Fatal("simultaneous providers enabled")
	}
}

func TestDetectorOffRechecksActiveRunAndUsesAssignedOutlet(t *testing.T) {
	ctx := context.Background()
	farm := &inspectingFarm{state: "PAUSED", task: "31"}
	s, _, _ := mediaFixture(t, farm)
	cameraFixture(t, s, "http://127.0.0.1")
	ops, e := operations.New(s.db)
	if e != nil {
		t.Fatal(e)
	}
	api := http.NewServeMux()
	ops.Register(api)
	s.SetDetectorPowerOff(ops.PowerOffForDetection)
	var calls atomic.Int32
	plug := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != "/power/off" {
			t.Error(r.Method, r.URL.Path)
		}
		calls.Add(1)
		w.WriteHeader(200)
	}))
	defer plug.Close()
	makeOutlet := func(name string, controls bool) {
		b, _ := json.Marshal(object{"name": name, "plug_type": "rest", "printer_id": 1, "enabled": true, "controls_printer_power": controls, "rest_off_url": plug.URL + "/power/off", "rest_off_method": "POST"})
		r := httptest.NewRequest("POST", "/api/v1/smart-plugs/", bytes.NewReader(b))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r.WithContext(access.With(r.Context(), access.Principal{Admin: true})))
		if w.Code < 200 || w.Code >= 300 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	makeOutlet("Camera accessory", false)
	makeOutlet("Printer outlet", true)
	if _, e = s.db.Exec("INSERT INTO print_log_entries(archive_id,printer_id,status,subtask_id) VALUES(1,1,'printing','31')"); e != nil {
		t.Fatal(e)
	}
	for k, v := range map[string]string{"obico_enabled": "true", "obico_action": "pause_and_off", "obico_enabled_printers": "[1]"} {
		putML(t, s, k, v)
	}
	c, _ := s.detectorSettings(ctx, "obico")
	i := inspection{task: "31", name: "shop", config: c.fingerprint(), run: s.activePrint(ctx, 1, "31")}
	if e = s.detectorPower(ctx, c, 1, &i); e != nil || calls.Load() != 1 {
		t.Fatal(e, calls.Load())
	}
	farm.task = "32"
	if e = s.detectorPower(ctx, c, 1, &i); e == nil || calls.Load() != 1 {
		t.Fatal("Powered off a replacement job", e)
	}
	farm.task = "31"
	putML(t, s, "obico_action", "notify")
	if e = s.detectorPower(ctx, c, 1, &i); e == nil || calls.Load() != 1 {
		t.Fatal("Powered off after reconfiguration")
	}
	putML(t, s, "obico_action", "pause_and_off")
	_, _ = s.db.Exec("UPDATE print_log_entries SET completed_at=CURRENT_TIMESTAMP WHERE id=?", i.run)
	if e = s.detectorPower(ctx, c, 1, &i); e == nil || calls.Load() != 1 {
		t.Fatal("Powered off after run ended")
	}
}
func TestGadgetResizesOversizedFrames(t *testing.T) {
	original := testJPEG(false)
	large := append(append([]byte{}, original...), make([]byte, 6<<20)...)
	prepared, e := gadgetFrame(large)
	if e != nil || len(prepared) > 6<<20 {
		t.Fatal(e, len(prepared))
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(prepared))
	if e != nil || cfg.Width > 2048 || cfg.Height > 2048 {
		t.Fatal(cfg, e)
	}
	if _, e = gadgetFrame(bytes.Repeat([]byte("x"), 6<<20+1)); e == nil {
		t.Fatal("Invalid oversized camera image accepted")
	}
}
