package operations

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPrivateEventPhotoAndDelivery(t *testing.T) {
	f := setup(t)
	dir := t.TempDir()
	pub := http.NewServeMux()
	f.s.RegisterPhotos(pub, dir, nil)
	var frame bytes.Buffer
	_ = jpeg.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil)
	name, e := f.s.savePhoto(frame.Bytes(), "plate_not_empty")
	if e != nil {
		t.Fatal(e)
	}
	path := "/api/v1/notifications/photos/" + name
	get := func(p string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		pub.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		return w
	}
	w := get(path)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), frame.Bytes()) || w.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatal(w.Code)
	}
	if get("/api/v1/notifications/photos/plate.jpg").Code != 404 {
		t.Fatal("Accepted a guessable name")
	}
	var payload obj
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
			t.Error(e)
		}
	}))
	defer hook.Close()
	if e = f.s.deliver(context.Background(), "webhook", obj{"webhook_url": hook.URL}, "Plate", "Occupied", "plate_not_empty", 1, &deliveryPhoto{frame.Bytes(), "https://farm.test" + path}); e != nil || payload["image_url"] != "https://farm.test"+path {
		t.Fatal(e, payload)
	}
	if e = os.Chtimes(filepath.Join(dir, "notification_photos", name), time.Now().Add(-80*time.Hour), time.Now().Add(-80*time.Hour)); e != nil {
		t.Fatal(e)
	}
	if get(path).Code != 404 {
		t.Fatal("Expired photo served")
	}
}
