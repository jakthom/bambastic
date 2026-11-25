package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/image/draw"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

type plateRef struct {
	id           int64
	label, stamp string
	data         []byte
}
type plateROI struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}
type plateStart struct {
	id, run int64
	at      time.Time
}

func (s *Service) initPlates() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS bb_plate_ref(id INTEGER PRIMARY KEY AUTOINCREMENT,printer_id INTEGER NOT NULL REFERENCES printers(id) ON DELETE CASCADE,label TEXT NOT NULL,stamp TEXT NOT NULL,jpeg BLOB NOT NULL);CREATE INDEX IF NOT EXISTS bb_plate_ref_device ON bb_plate_ref(printer_id,id);CREATE TABLE IF NOT EXISTS bb_plate_ref_imported(printer_id INTEGER PRIMARY KEY REFERENCES printers(id) ON DELETE CASCADE)`)
	return err
}

// Read the upstream five-slot calibration format once per printer. Recording
// the import also prevents a deleted calibration from reappearing on restart.
func (s *Service) importPlateRefs(ctx context.Context, id int64) error {
	var n int
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM bb_plate_ref_imported WHERE printer_id=?", id).Scan(&n)
	if err != nil || n > 0 {
		return err
	}
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		return err
	}
	defer root.Close()
	base := fmt.Sprintf("plate_calibration/printer_%d_", id)
	var meta struct {
		References map[string]struct{ Label, Timestamp string }
	}
	if f, e := root.Open(base + "metadata.json"); e == nil {
		_ = json.NewDecoder(io.LimitReader(f, 65536)).Decode(&meta)
		f.Close()
	}
	refs := []plateRef{}
	for n := 0; n < 5; n++ {
		f, e := root.Open(fmt.Sprintf("%sref_%d.jpg", base, n))
		if e != nil {
			continue
		}
		b, e := io.ReadAll(io.LimitReader(f, 10_000_001))
		f.Close()
		if e != nil || len(b) > 10_000_000 {
			continue
		}
		if _, e = plateImage(b); e != nil {
			continue
		}
		m := meta.References[strconv.Itoa(n)]
		refs = append(refs, plateRef{label: m.Label, stamp: m.Timestamp, data: b})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bb_plate_ref_imported(printer_id) VALUES(?)", id)
	if err != nil {
		return err
	}
	if count, _ := res.RowsAffected(); count == 1 {
		for _, ref := range refs {
			if _, err = tx.ExecContext(ctx, "INSERT INTO bb_plate_ref(printer_id,label,stamp,jpeg) VALUES(?,?,?,?)", id, ref.label, ref.stamp, ref.data); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
func (s *Service) plateRefs(ctx context.Context, id int64) ([]plateRef, error) {
	if e := s.importPlateRefs(ctx, id); e != nil {
		return nil, e
	}
	rows, e := s.db.QueryContext(ctx, "SELECT id,label,stamp,jpeg FROM bb_plate_ref WHERE printer_id=? ORDER BY id", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	refs := []plateRef{}
	for rows.Next() {
		var ref plateRef
		if e = rows.Scan(&ref.id, &ref.label, &ref.stamp, &ref.data); e != nil {
			return nil, e
		}
		refs = append(refs, ref)
	}
	return refs, rows.Err()
}
func (s *Service) registerPlates(public, api *http.ServeMux) {
	base := "/api/v1/printers/{id}/camera/"
	for _, route := range []string{"GET check-plate", "POST plate-detection/calibrate", "GET plate-detection/status", "GET plate-detection/references", "PUT plate-detection/references/{index}", "DELETE plate-detection/references/{index}"} {
		m, p, _ := strings.Cut(route, " ")
		api.HandleFunc(m+" "+base+p, s.plates)
	}
	route := "GET " + base + "plate-detection/references/{index}/thumbnail"
	public.HandleFunc(route, s.withMedia("camera", s.plates))
	if public != api {
		api.HandleFunc(route, s.plates)
	}
}
func plateImage(b []byte) (image.Image, error) {
	c, e := jpeg.DecodeConfig(bytes.NewReader(b))
	if e != nil || c.Width < 2 || c.Height < 2 || int64(c.Width)*int64(c.Height) > 16_000_000 {
		return nil, errors.New("Invalid calibration image dimensions")
	}
	return jpeg.Decode(bytes.NewReader(b))
}
func region(img image.Image, r plateROI) image.Rectangle {
	b := img.Bounds()
	x := b.Min.X + int(float64(b.Dx())*r.X)
	y := b.Min.Y + int(float64(b.Dy())*r.Y)
	return image.Rect(x, y, x+max(1, int(float64(b.Dx())*r.W)), y+max(1, int(float64(b.Dy())*r.H))).Intersect(b)
}
func blurPass(v []float64, w, h int, sigma float64, horizontal bool) []float64 {
	sigma = min(sigma, 16)
	radius := max(1, int(math.Ceil(3*sigma)))
	kernel := make([]float64, 2*radius+1)
	sum := 0.
	for i := -radius; i <= radius; i++ {
		g := math.Exp(-float64(i*i) / (2 * sigma * sigma))
		kernel[i+radius] = g
		sum += g
	}
	out := make([]float64, len(v))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			var val float64
			for d := -radius; d <= radius; d++ {
				px, py := x, y
				if horizontal {
					px = max(0, min(w-1, x+d))
				} else {
					py = max(0, min(h-1, y+d))
				}
				val += v[py*w+px] * kernel[d+radius]
			}
			out[y*w+x] = val / sum
		}
	}
	return out
}
func plateTemplate(img image.Image, r plateROI) []float64 {
	const w, h = 128, 96
	rect := region(img, r)
	g := image.NewGray(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(g, g.Bounds(), img, rect, draw.Src, nil)
	values := make([]float64, len(g.Pix))
	for i, v := range g.Pix {
		values[i] = float64(v)
	}
	values = blurPass(values, w, h, max(.5, 8.*w/float64(rect.Dx())), true)
	values = blurPass(values, w, h, max(.5, 8.*h/float64(rect.Dy())), false)
	lo, hi := 255., 0.
	for _, v := range values {
		lo = min(lo, v)
		hi = max(hi, v)
	}
	for i, v := range values {
		if hi-lo > .5 {
			values[i] = (v - lo) / (hi - lo)
		} else {
			values[i] = 0
		}
	}
	return values
}
func (s *Service) roi(ctx context.Context, id int64) plateROI {
	d := plateROI{.15, .35, .70, .55}
	var x, y, w, h sql.NullFloat64
	if e := s.db.QueryRowContext(ctx, "SELECT plate_detection_roi_x,plate_detection_roi_y,plate_detection_roi_w,plate_detection_roi_h FROM printers WHERE id=?", id).Scan(&x, &y, &w, &h); e == nil && x.Valid && y.Valid && w.Valid && h.Valid && x.Float64 >= 0 && y.Float64 >= 0 && w.Float64 > 0 && h.Float64 > 0 && x.Float64+w.Float64 <= 1 && y.Float64+h.Float64 <= 1 {
		d = plateROI{x.Float64, y.Float64, w.Float64, h.Float64}
	}
	return d
}
func (s *Service) plateFrame(r *http.Request, v source) ([]byte, error) {
	if r.URL.Query().Has("use_external") {
		use, e := strconv.ParseBool(r.URL.Query().Get("use_external"))
		if e != nil {
			return nil, errors.New("Invalid camera selection")
		}
		if use && !v.external {
			return nil, errors.New("External camera is disabled")
		}
		if use != v.external {
			v.external = use
			return s.isolatedFrame(r.Context(), v)
		}
	}
	if v.external && v.snapshotURL != "" {
		v.externalURL = v.snapshotURL
		v.externalType = "snapshot"
		return s.isolatedFrame(r.Context(), v)
	}
	return s.firstFrame(r.Context(), v)
}
func (s *Service) checkPlate(r *http.Request, v source, refs []plateRef) object {
	roi := s.roi(r.Context(), v.id)
	out := object{"is_empty": false, "confidence": 0., "difference_percent": 0., "light_warning": false, "has_debug_image": false, "needs_calibration": len(refs) == 0, "reference_count": len(refs), "max_references": 5, "roi": roi, "failure_reason": nil}
	if s.farm != nil {
		states, e := s.farm.InitialStatus(r.Context(), access.From(r.Context()))
		if e == nil {
			for _, st := range states {
				if num(asString(st["id"])) == v.id && st["chamber_light"] == false {
					out["light_warning"] = true
					break
				}
			}
		}
	}
	if len(refs) == 0 {
		out["failure_reason"] = "not_calibrated"
		out["message"] = "Calibrate using an empty build plate"
		return out
	}
	b, e := s.plateFrame(r, v)
	if e != nil {
		out["failure_reason"] = "capture_failed"
		out["message"] = e.Error()
		return out
	}
	img, e := plateImage(b)
	if e != nil {
		out["failure_reason"] = "capture_failed"
		out["message"] = "Unreadable camera frame"
		return out
	}
	current := plateTemplate(img, roi)
	best := math.Inf(1)
	for _, ref := range refs {
		img, e := plateImage(ref.data)
		if e != nil {
			continue
		}
		pixels := plateTemplate(img, roi)
		var d float64
		for i, x := range current {
			d += math.Abs(x - pixels[i])
		}
		best = min(best, d*100/float64(len(current)))
	}
	if math.IsInf(best, 1) {
		out["needs_calibration"] = true
		out["failure_reason"] = "not_calibrated"
		out["message"] = "Calibration images could not be read"
		return out
	}
	empty := best < 1
	out["difference_percent"] = math.Round(best*100) / 100
	out["is_empty"] = empty
	out["confidence"] = min(1., best/2)
	out["message"] = "Objects detected on plate"
	if empty {
		out["message"] = "Plate appears empty"
		out["confidence"] = max(0., 1-best)
	}
	if r.URL.Query().Get("include_debug_image") == "true" {
		debug := scaled(img, 640)
		rect := region(debug, roi)
		c := color.NRGBA{0, 190, 200, 255}
		if !empty {
			c = color.NRGBA{255, 90, 50, 255}
		}
		for y := rect.Min.Y; y < rect.Max.Y; y++ {
			debug.Set(rect.Min.X, y, c)
			debug.Set(rect.Max.X-1, y, c)
		}
		for x := rect.Min.X; x < rect.Max.X; x++ {
			debug.Set(x, rect.Min.Y, c)
			debug.Set(x, rect.Max.Y-1, c)
		}
		var b bytes.Buffer
		if jpeg.Encode(&b, debug, nil) == nil {
			out["has_debug_image"] = true
			out["debug_image_url"] = "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(b.Bytes())
		}
	}
	return out
}
func (s *Service) plates(w http.ResponseWriter, r *http.Request) {
	perm := "camera:view"
	if r.Method != "GET" {
		perm = "printers:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	v, e := s.device(r)
	if e != nil {
		httpx.Error(w, 404, "Printer not found")
		return
	}
	refs, e := s.plateRefs(r.Context(), v.id)
	if e != nil {
		httpx.Error(w, 500, "Cannot read calibration references")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/check-plate") {
		httpx.JSON(w, 200, s.checkPlate(r, v, refs))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/status") {
		msg := "Calibrate using an empty build plate"
		if len(refs) > 0 {
			msg = fmt.Sprintf("Calibrated with %d of 5 references", len(refs))
		}
		httpx.JSON(w, 200, object{"available": true, "calibrated": len(refs) > 0, "reference_count": len(refs), "max_references": 5, "message": msg})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/references") {
		out := []object{}
		for i, ref := range refs {
			out = append(out, object{"index": i, "label": ref.label, "timestamp": ref.stamp, "has_image": true, "thumbnail_url": fmt.Sprintf("/api/v1/printers/%d/camera/plate-detection/references/%d/thumbnail", v.id, i)})
		}
		httpx.JSON(w, 200, object{"references": out, "max_references": 5})
		return
	}
	label := r.URL.Query().Get("label")
	if len(label) > 256 {
		httpx.Error(w, 422, "Reference label is too long")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/calibrate") {
		frame, e := s.plateFrame(r, v)
		if e != nil {
			httpx.JSON(w, 200, object{"success": false, "message": e.Error(), "index": -1})
			return
		}
		if _, e = plateImage(frame); e != nil {
			httpx.Error(w, 422, e.Error())
			return
		}
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e != nil {
			httpx.Error(w, 500, "Cannot save reference")
			return
		}
		defer tx.Rollback()
		_, e = tx.ExecContext(r.Context(), "INSERT INTO bb_plate_ref(printer_id,label,stamp,jpeg) VALUES(?,?,?,?)", v.id, label, time.Now().UTC().Format(time.RFC3339), frame)
		if e == nil {
			_, e = tx.ExecContext(r.Context(), "DELETE FROM bb_plate_ref WHERE printer_id=? AND id NOT IN (SELECT id FROM bb_plate_ref WHERE printer_id=? ORDER BY id DESC LIMIT 5)", v.id, v.id)
		}
		var n int
		if e == nil {
			e = tx.QueryRowContext(r.Context(), "SELECT count(*) FROM bb_plate_ref WHERE printer_id=?", v.id).Scan(&n)
		}
		if e != nil || tx.Commit() != nil {
			httpx.Error(w, 409, "Cannot commit reference")
			return
		}
		httpx.JSON(w, 200, object{"success": true, "index": n - 1, "message": "Calibration saved"})
		return
	}
	index, e := strconv.Atoi(r.PathValue("index"))
	if e != nil || index < 0 || index >= len(refs) {
		httpx.Error(w, 404, "Reference not found")
		return
	}
	ref := refs[index]
	if r.Method == "GET" {
		img, e := plateImage(ref.data)
		if e != nil {
			httpx.Error(w, 422, "Invalid reference image")
			return
		}
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Content-Type", "image/jpeg")
		_ = jpeg.Encode(w, scaled(img, 150), nil)
		return
	}
	var res sql.Result
	if r.Method == "PUT" {
		res, e = s.db.ExecContext(r.Context(), "UPDATE bb_plate_ref SET label=? WHERE id=? AND printer_id=?", label, ref.id, v.id)
	} else {
		res, e = s.db.ExecContext(r.Context(), "DELETE FROM bb_plate_ref WHERE id=? AND printer_id=?", ref.id, v.id)
	}
	if e != nil {
		httpx.Error(w, 500, "Could not update reference")
		return
	}
	if n, _ := res.RowsAffected(); n != 1 {
		httpx.Error(w, 409, "Reference changed")
		return
	}
	httpx.JSON(w, 200, object{"success": true, "index": index, "label": label, "message": "Reference updated"})
}

// Publish queues print-start observations after the production transaction commits.
func (s *Service) Publish(event map[string]any) {
	if event["type"] != "print_start" {
		return
	}
	data, _ := json.Marshal(event["data"])
	var d struct {
		Run int64 `json:"run_id"`
	}
	if json.Unmarshal(data, &d) != nil || d.Run <= 0 {
		return
	}
	e := plateStart{id: num(asString(event["printer_id"])), run: d.Run, at: time.Now()}
	if e.id <= 0 {
		return
	}
	select {
	case s.plateStarts <- e:
	default:
	}
}
func (s *Service) runPlateChecks(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-s.plateStarts:
			s.onPlateStart(ctx, e)
		}
	}
}
func (s *Service) onPlateStart(ctx context.Context, event plateStart) {
	farm, ok := s.farm.(interface {
		OnPlateInspection(context.Context, int64, int64, string, string, bool, string) error
	})
	if !ok {
		return
	}
	var enabled bool
	var task sql.NullString
	if s.db.QueryRowContext(ctx, "SELECT p.plate_detection_enabled,r.subtask_id FROM print_log_entries r JOIN printers p ON p.id=r.printer_id WHERE r.id=? AND p.id=? AND r.completed_at IS NULL", event.run, event.id).Scan(&enabled, &task) != nil || !enabled {
		return
	}
	var name string
	states, e := s.farm.InitialStatus(ctx, access.Principal{Admin: true})
	if e != nil {
		return
	}
	for _, v := range states {
		if num(asString(v["id"])) == event.id && v["connected"] == true && num(asString(v["layer_num"])) <= 1 && asString(v["subtask_id"]) == task.String {
			name = asString(v["subtask_name"])
			break
		}
	}
	if name == "" || time.Since(event.at) > 40*time.Second {
		return
	}
	r, _ := http.NewRequestWithContext(access.With(ctx, access.Principal{Admin: true}), "GET", "/", nil)
	r.SetPathValue("id", fmt.Sprint(event.id))
	source, e := s.device(r)
	if e != nil {
		return
	}
	refs, e := s.plateRefs(ctx, event.id)
	if e != nil {
		return
	}
	result := s.checkPlate(r, source, refs)
	reason := asString(result["failure_reason"])
	occupied := reason == "" && result["is_empty"] == false
	if (occupied || reason != "") && time.Since(event.at) < 60*time.Second {
		_ = farm.OnPlateInspection(ctx, event.id, event.run, task.String, name, occupied, reason)
	}
}
