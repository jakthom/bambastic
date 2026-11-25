package production

import (
	"net"
	"net/url"
	"regexp"
	"strings"

	"bambastic/internal/access"
)

func encodeWLED(m row) error {
	raw, ok := m["wled_config"]
	if !ok || raw == nil {
		return nil
	}
	cfg, ok := raw.(map[string]any)
	if !ok {
		return fail(422, "Invalid WLED configuration")
	}
	out := row{"enabled": flag(cfg["enabled"]), "presets": row{}}
	addr := strings.TrimRight(strings.TrimSpace(val(cfg["base_url"])), "/")
	if addr == "" {
		if flag(out["enabled"]) {
			return fail(422, "WLED address required")
		}
	} else {
		u, e := url.Parse(addr)
		if e != nil || !in(u.Scheme, "http", "https") || u.Hostname() == "" || len(addr) > 500 || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return fail(422, "Invalid WLED address")
		}
		if ip := net.ParseIP(u.Hostname()); ip != nil && (ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.String() == "100.100.100.200") {
			return fail(422, "Invalid WLED address")
		}
		out["base_url"] = addr
	}
	if cfg["finished_timeout_seconds"] != nil {
		n, e := bounded(val(cfg["finished_timeout_seconds"]), 0, 86400)
		if e != nil {
			return e
		}
		out["finished_timeout_seconds"] = n
	}
	if v := cfg["presets"]; v != nil {
		ps, ok := v.(map[string]any)
		if !ok {
			return fail(422, "Invalid WLED presets")
		}
		for k, n := range ps {
			if !in(k, "idle", "prepare", "printing", "paused", "finished", "error", "queue_waiting", "filament_problem", "hms_error", "offline") {
				return fail(422, "Unknown WLED state")
			}
			if n != nil {
				v, e := bounded(val(n), 1, 250)
				if e != nil {
					return e
				}
				out["presets"].(row)[k] = v
			}
		}
	}
	m["wled_config"] = jstr(out)
	return nil
}
func showPrinter(v row, p access.Principal) row {
	if v == nil {
		return nil
	}
	delete(v, "access_code")
	if v["plate_detection_roi_w"] != nil {
		roi := row{}
		for _, k := range []string{"x", "y", "w", "h"} {
			roi[k] = v["plate_detection_roi_"+k]
		}
		v["plate_detection_roi"] = roi
	}
	for _, k := range []string{"external_camera_url", "external_camera_snapshot_url"} {
		if !p.Has("printers:update") {
			delete(v, k)
		} else if v[k] != nil {
			v[k] = cameraDisplayURL(val(v[k]))
		}
	}
	if !p.Has("printers:update") {
		delete(v, "wled_config")
		return v
	}
	if raw := v["wled_config"]; raw != nil {
		cfg, _ := unpack(raw).(map[string]any)
		if u, e := url.Parse(val(cfg["base_url"])); e == nil && u.User != nil {
			u.User = nil
			cfg["base_url"] = u.String()
		}
		v["wled_config"] = cfg
	}
	return v
}

var usbCam = regexp.MustCompile(`^/dev/video[0-9]{1,2}$`)
var cameraFields = []string{"external_camera_url", "external_camera_type", "external_camera_snapshot_url", "external_camera_enabled", "camera_rotation", "camera_light_auto", "plate_detection_enabled", "plate_detection_roi_x", "plate_detection_roi_y", "plate_detection_roi_w", "plate_detection_roi_h"}

func cameraDisplayURL(s string) string {
	u, e := url.Parse(s)
	if e != nil {
		return ""
	}
	if u.User != nil {
		u.User = url.UserPassword(u.User.Username(), "***")
	}
	return u.String()
}
func encodeCamera(m, previous row) error {
	for _, k := range []string{"external_camera_url", "external_camera_snapshot_url"} {
		v, present := m[k]
		if !present || v == nil {
			continue
		}
		s := strings.TrimSpace(val(v))
		if s == "" {
			m[k] = nil
			continue
		}
		if old := val(previous[k]); old != "" && s == cameraDisplayURL(old) {
			m[k] = old
			continue
		}
		if k == "external_camera_url" && usbCam.MatchString(s) {
			continue
		}
		u, e := url.Parse(s)
		if e != nil || u.Host == "" || len(s) > 500 || !in(u.Scheme, "http", "https", "rtsp", "rtsps") || k == "external_camera_snapshot_url" && !in(u.Scheme, "http", "https") {
			return fail(422, "Invalid camera URL")
		}
		m[k] = s
	}
	if typ, ok := m["external_camera_type"]; ok && typ != nil && !in(val(typ), "mjpeg", "snapshot", "rtsp", "usb") {
		return fail(422, "Invalid camera type")
	}
	if rot, ok := m["camera_rotation"]; ok && !in(val(rot), "0", "90", "180", "270") {
		return fail(422, "Invalid camera rotation")
	}
	for _, k := range cameraFields {
		if strings.HasPrefix(k, "plate_detection_roi_") {
			delete(m, k)
		}
	}
	if roi, present := m["plate_detection_roi"]; present {
		if roi == nil {
			for _, k := range []string{"x", "y", "w", "h"} {
				m["plate_detection_roi_"+k] = nil
			}
			return nil
		}
		vals, ok := roi.(map[string]any)
		if !ok {
			return fail(422, "Invalid plate detection region")
		}
		for _, k := range []string{"x", "y", "w", "h"} {
			n, ok := decimal(vals[k])
			if !ok || n < 0 || n > 1 || in(k, "w", "h") && n == 0 {
				return fail(422, "Invalid plate detection region")
			}
			m["plate_detection_roi_"+k] = n
		}
		if m["plate_detection_roi_x"].(float64)+m["plate_detection_roi_w"].(float64) > 1.000001 || m["plate_detection_roi_y"].(float64)+m["plate_detection_roi_h"].(float64) > 1.000001 {
			return fail(422, "Plate detection region extends outside the frame")
		}
	}
	return nil
}
