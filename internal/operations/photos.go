package operations

import (
	"bambastic/internal/httpx"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type photoCapture func(context.Context, int64, int64, string) ([]byte, error)
type deliveryPhoto struct {
	jpeg []byte
	url  string
}

var photoName = regexp.MustCompile(`^[a-z0-9_]{1,80}_[0-9]{8}_[0-9]{6}_[A-Za-z0-9_-]{32}\.jpg$`)
var eventName = regexp.MustCompile(`^[a-z0-9_]{1,80}$`)

func (s *Service) RegisterPhotos(public *http.ServeMux, data string, capture func(context.Context, int64, int64, string) ([]byte, error)) {
	s.photoDir = data
	s.capture = capture
	public.HandleFunc("GET /api/v1/notifications/photos/{filename}", func(w http.ResponseWriter, r *http.Request) {
		b, e := s.readPhoto(r.PathValue("filename"))
		if e != nil {
			httpx.Error(w, 404, "Photo not found")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(b)
	})
}
func (s *Service) readPhoto(name string) ([]byte, error) {
	if s.photoDir == "" || !photoName.MatchString(name) {
		return nil, os.ErrNotExist
	}
	root, e := os.OpenRoot(s.photoDir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.Open(filepath.Join("notification_photos", name))
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() || st.Size() > 8<<20 || time.Since(st.ModTime()) > 72*time.Hour {
		return nil, os.ErrNotExist
	}
	return io.ReadAll(io.LimitReader(f, 8<<20))
}
func (s *Service) savePhoto(data []byte, event string) (string, error) {
	if !eventName.MatchString(event) || len(data) == 0 || len(data) > 8<<20 || s.photoDir == "" {
		return "", errors.New("Invalid notification photo")
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(data))
	if e != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 {
		return "", errors.New("Invalid camera JPEG")
	}
	root, e := os.OpenRoot(s.photoDir)
	if e != nil {
		return "", e
	}
	defer root.Close()
	if e = root.MkdirAll("notification_photos", 0700); e != nil {
		return "", e
	}
	token := make([]byte, 24)
	if _, e = rand.Read(token); e != nil {
		return "", e
	}
	name := fmt.Sprintf("%s_%s_%s.jpg", event, time.Now().UTC().Format("20060102_150405"), base64.RawURLEncoding.EncodeToString(token))
	rel := filepath.Join("notification_photos", name)
	f, e := root.OpenFile(rel, os.O_EXCL|os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		return "", e
	}
	_, e = f.Write(data)
	ce := f.Close()
	if e != nil || ce != nil {
		root.Remove(rel)
		return "", errors.New("Cannot save notification image")
	}
	if dir, e := root.Open("notification_photos"); e == nil {
		entries, _ := dir.ReadDir(-1)
		dir.Close()
		for _, entry := range entries {
			if st, e := entry.Info(); e == nil && photoName.MatchString(st.Name()) && time.Since(st.ModTime()) > 72*time.Hour {
				root.Remove(filepath.Join("notification_photos", st.Name()))
			}
		}
	}
	return name, nil
}
func (s *Service) deliveryImage(ctx context.Context, v, p obj) *deliveryPhoto {
	if !flag(p["attach_photo"]) || s.photoDir == "" {
		return nil
	}
	var name string
	_ = s.db.QueryRowContext(ctx, "SELECT name FROM bambastic_notification_photos WHERE delivery_id=?", v["id"]).Scan(&name)
	var b []byte
	var err error
	if name != "" {
		b, err = s.readPhoto(name)
		if err != nil {
			return nil
		}
	} else {
		if s.capture == nil || num(v["printer_id"]) <= 0 {
			return nil
		}
		var fresh bool
		_ = s.db.QueryRowContext(ctx, "SELECT unixepoch('now')-unixepoch(created_at)<90 FROM bambastic_notification_delivery WHERE id=?", v["id"]).Scan(&fresh)
		if !fresh {
			return nil
		}
		switch str(v["event_type"]) {
		case "print_start", "print_complete", "print_failed", "print_stopped", "first_layer_complete", "plate_not_empty", "ai_failure_detection", "printer_error":
		default:
			return nil
		}
		b, err = s.capture(ctx, int64(num(v["printer_id"])), int64(num(v["run_id"])), str(v["event_type"]))
		if err != nil || len(b) == 0 {
			return nil
		}
		name, err = s.savePhoto(b, str(v["event_type"]))
		if err != nil {
			return nil
		}
		_, _ = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO bambastic_notification_photos(delivery_id,name) SELECT id,? FROM bambastic_notification_delivery WHERE (id=? OR (? IS NOT NULL AND run_id=? AND event_type=? AND state='pending'))", name, v["id"], v["run_id"], v["run_id"], v["event_type"])
	}
	base := strings.TrimRight(setting(ctx, s.db, "external_url"), "/")
	imageURL := ""
	if u, e := url.Parse(base); e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" && u.User == nil {
		imageURL = base + "/api/v1/notifications/photos/" + name
	}
	return &deliveryPhoto{b, imageURL}
}
func photoMultipart(fields obj, key string, image []byte) ([]byte, string, error) {
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	for k, v := range fields {
		if v != nil {
			if e := w.WriteField(k, str(v)); e != nil {
				return nil, "", e
			}
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="photo.jpg"`, key))
	h.Set("Content-Type", "image/jpeg")
	part, e := w.CreatePart(h)
	if e != nil {
		return nil, "", e
	}
	if _, e = part.Write(image); e != nil {
		return nil, "", e
	}
	if e = w.Close(); e != nil {
		return nil, "", e
	}
	return b.Bytes(), w.FormDataContentType(), nil
}
