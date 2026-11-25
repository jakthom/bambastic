package monitoring

import (
	"bambastic/internal/access"
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCalibrationImportRotateAndCameraComparison(t *testing.T) {
	s, _, api := mediaFixture(t, &inspectingFarm{state: "RUNNING", task: "23"})
	p := access.Principal{ID: 1, Admin: true}
	var changed atomic.Bool
	cam := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(testJPEG(changed.Load()))
	}))
	defer cam.Close()
	cameraFixture(t, s, cam.URL)
	root := filepath.Join(s.dir, "plate_calibration")
	if e := os.Mkdir(root, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, "printer_1_ref_0.jpg"), testJPEG(false), 0600); e != nil {
		t.Fatal(e)
	}
	base := "/api/v1/printers/1/camera/"
	ref := base + "plate-detection/"
	w := mediaDo(api, "GET", ref+"status", p)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"reference_count":1`) {
		t.Fatal(w.Code, w.Body.String())
	}
	thumb := mediaDo(api, "GET", ref+"references/0/thumbnail", p)
	if thumb.Code != 200 || !jpegFrame(thumb.Body.Bytes()) {
		t.Fatal(thumb.Code)
	}
	for n := 0; n < 6; n++ {
		w = mediaDo(api, "POST", ref+"calibrate?label=Shop", p)
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"success":true`) {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = mediaDo(api, "GET", ref+"status", p)
	if !strings.Contains(w.Body.String(), `"reference_count":5`) {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "GET", base+"check-plate", p)
	if !strings.Contains(w.Body.String(), `"is_empty":true`) {
		t.Fatal(w.Code, w.Body.String())
	}
	changed.Store(true)
	w = mediaDo(api, "GET", base+"check-plate?include_debug_image=true", p)
	var result struct {
		Empty      bool    `json:"is_empty"`
		Difference float64 `json:"difference_percent"`
		Debug      string  `json:"debug_image_url"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Empty || result.Difference < 1 || !strings.HasPrefix(result.Debug, "data:image/jpeg;") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "PUT", ref+"references/0?label=New", p)
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	for n := 0; n < 5; n++ {
		w = mediaDo(api, "DELETE", ref+"references/0", p)
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	w = mediaDo(api, "GET", base+"check-plate", p)
	if !strings.Contains(w.Body.String(), `"needs_calibration":true`) {
		t.Fatal(w.Body.String())
	}
	w = mediaDo(api, "GET", ref+"status", access.Principal{ID: 1, PrinterIDs: map[int64]bool{2: true}, Permissions: map[string]bool{"camera:view": true}})
	if w.Code != 404 {
		t.Fatal(w.Code)
	}
}
func TestLogoUploadAndOverlayScope(t *testing.T) {
	s, pub, api := mediaFixture(t, &mockFarm{})
	p := access.Principal{ID: 1, Admin: true}
	var media bytes.Buffer
	_ = png.Encode(&media, image.NewRGBA(image.Rect(0, 0, 650, 320)))
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, _ := mw.CreateFormFile("file", "logo.png")
	_, _ = part.Write(media.Bytes())
	_ = mw.Close()
	r := httptest.NewRequest("POST", "/api/v1/settings/overlay-logo", &body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = mediaDo(api, "GET", "/api/v1/settings/overlay-logo", p)
	cfg, e := png.DecodeConfig(w.Body)
	if e != nil || cfg.Width != 512 {
		t.Fatal(cfg, e)
	}
	mint := func(scope string) string {
		r := httptest.NewRequest("POST", "/api/v1/auth/tokens", strings.NewReader(`{"name":"screen","scope":"`+scope+`","expires_in_days":1}`))
		w := httptest.NewRecorder()
		api.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
		var o struct{ Token string }
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &o) != nil {
			t.Fatal(w.Code, w.Body.String())
		}
		return o.Token
	}
	wall := mint("camwall")
	overlay := mint("overlay")
	if w = mediaDo(pub, "GET", "/api/v1/overlay-branding/logo?token="+wall, p); w.Code != 401 {
		t.Fatal("wrong display scope", w.Code)
	}
	if w = mediaDo(pub, "GET", "/api/v1/overlay-branding/logo?token="+overlay, p); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = mediaDo(api, "DELETE", "/api/v1/settings/overlay-logo", p); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w = mediaDo(pub, "GET", "/api/v1/overlay-branding/logo?token="+overlay, p); w.Code != 404 {
		t.Fatal(w.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	s.Run(ctx)
}
