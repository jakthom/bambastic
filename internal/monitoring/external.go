package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bytes"
	"context"
	"errors"
	"fmt"
	"image/jpeg"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

var videoDevice = regexp.MustCompile(`^/dev/video([0-9]{1,2})$`)

func usbDevice(raw string) (string, error) {
	n := videoDevice.FindStringSubmatch(raw)
	if runtime.GOOS != "linux" || len(n) != 2 {
		return "", errors.New("Linux V4L2 camera unavailable")
	}
	id, _ := strconv.Atoi(n[1])
	name := fmt.Sprintf("/dev/video%d", id)
	st, e := os.Lstat(name)
	if e != nil || st.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("USB camera device unavailable")
	}
	return name, nil
}
func (s *Service) usbList(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "printers:read") {
		return
	}
	out := []object{}
	devices, _ := filepath.Glob("/dev/video*")
	for _, p := range devices {
		dev, e := usbDevice(p)
		if e != nil {
			continue
		}
		base := filepath.Base(dev)
		name := base
		if src, e := os.ReadFile(filepath.Join("/sys/class/video4linux", base, "name")); e == nil && len(src) > 0 {
			name = strings.TrimSpace(string(src))
		}
		out = append(out, object{"device": dev, "name": name, "capabilities": []string{"V4L2"}})
	}
	sort.Slice(out, func(i, j int) bool { return asString(out[i]["device"]) < asString(out[j]["device"]) })
	httpx.JSON(w, 200, object{"cameras": out})
}
func (s *Service) isolatedFrame(ctx context.Context, v source) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	var first []byte
	// The reader and callback run in this goroutine. Cancellation immediately
	// releases the device/socket/ffmpeg stream after the first complete frame.
	err := s.readCamera(ctx, v, func(b []byte) {
		if first == nil {
			first = b
			cancel()
		}
	})
	if len(first) > 0 {
		return first, nil
	}
	if err == nil {
		err = errors.New("Camera did not return a frame")
	}
	return nil, err
}
func (s *Service) testExternal(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "camera:view") {
		return
	}
	p := access.From(r.Context())
	v, e := s.device(r)
	if e != nil {
		httpx.Error(w, 404, "Printer not found")
		return
	}
	input := struct {
		URL  string `json:"url"`
		Kind string `json:"camera_type"`
	}{r.URL.Query().Get("url"), r.URL.Query().Get("camera_type")}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &input) {
		return
	}
	if input.URL == "" || input.Kind == "" {
		httpx.Error(w, 422, "Camera URL and type are required")
		return
	}
	if input.URL != v.externalURL || input.Kind != v.externalType {
		if !p.Has("printers:update") {
			httpx.Error(w, 403, "Printer configuration permission required to test another camera")
			return
		}
	}
	switch input.Kind {
	case "usb":
		if _, e = usbDevice(input.URL); e != nil {
			httpx.JSON(w, 200, object{"success": false, "error": e.Error()})
			return
		}
	case "rtsp", "snapshot", "mjpeg":
		u, e := url.Parse(input.URL)
		ok := e == nil && u.Hostname() != "" && (input.Kind == "rtsp" && (u.Scheme == "rtsp" || u.Scheme == "rtsps") || input.Kind != "rtsp" && (u.Scheme == "http" || u.Scheme == "https"))
		if !ok {
			httpx.Error(w, 422, "Unsupported camera URL")
			return
		}
	default:
		httpx.Error(w, 422, "Unsupported camera type")
		return
	}
	var frame []byte
	if v.external && v.externalURL == input.URL && v.externalType == input.Kind {
		frame, e = s.firstFrame(r.Context(), v)
	} else {
		v.external = true
		v.externalURL = input.URL
		v.externalType = input.Kind
		frame, e = s.isolatedFrame(r.Context(), v)
	}
	if e != nil {
		httpx.JSON(w, 200, object{"success": false, "error": e.Error()})
		return
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(frame))
	if e != nil {
		httpx.JSON(w, 200, object{"success": false, "error": "Cannot decode the camera image"})
		return
	}
	httpx.JSON(w, 200, object{"success": true, "resolution": fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)})
}

// NotificationSnapshot captures only the named printer and fences the image by
// the relevant run, both before and after potentially slow camera access.
func (s *Service) NotificationSnapshot(ctx context.Context, id, run int64, event string) ([]byte, error) {
	valid := func() bool {
		if run == 0 {
			return event == "printer_error" || event == "plate_not_empty" || event == "ai_failure_detection"
		}
		var latest int64
		err := s.db.QueryRowContext(ctx, "SELECT id FROM print_log_entries WHERE printer_id=? ORDER BY coalesce(started_at,created_at) DESC,id DESC LIMIT 1", id).Scan(&latest)
		return err == nil && latest == run
	}
	if !valid() {
		return nil, errors.New("Notification print changed")
	}
	r, _ := http.NewRequestWithContext(access.With(ctx, access.Principal{Admin: true}), "GET", "/", nil)
	r.SetPathValue("id", fmt.Sprint(id))
	v, e := s.device(r)
	if e != nil {
		return nil, e
	}
	b, e := s.firstFrame(ctx, v)
	if e != nil {
		return nil, e
	}
	if !valid() {
		return nil, errors.New("Notification print changed")
	}
	return b, nil
}
