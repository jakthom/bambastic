package production

import (
	"archive/zip"
	"context"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

type jobBytes struct {
	path          string
	plate         int64
	subtask, file string
	temp          bool
}

func (s *Service) currentBytes(ctx context.Context, p access.Principal, pr, rp row) (jobBytes, error) {
	k := num(pr["id"])
	j := jobBytes{subtask: val(rp["subtask_id"]), file: val(rp["gcode_file"])}
	run, e := one(ctx, s.db, "SELECT l.plate_id,l.subtask_id,a.file_path,a.filename FROM print_log_entries l JOIN print_archives a ON a.id=l.archive_id WHERE l.printer_id=? AND l.completed_at IS NULL ORDER BY l.id DESC LIMIT 1", k)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return j, e
	}
	if run != nil && val(run["subtask_id"]) != "" && j.subtask != "" && val(run["subtask_id"]) != j.subtask {
		return j, fail(409, "Printer changed jobs")
	}
	j.plate = num(run["plate_id"])
	if j.plate == 0 {
		j.plate = num(rp["plate_idx"])
	}
	if j.plate == 0 {
		if m := plateEntry.FindStringSubmatch(val(rp["param"])); len(m) > 1 {
			j.plate = num(m[1])
		}
	}
	if run != nil && val(run["file_path"]) != "" {
		j.path, e = s.assetPath(run["file_path"])
		if e == nil {
			return j, nil
		}
	}
	name := filepath.Base(j.file)
	if !validFileName(name) || !strings.HasSuffix(strings.ToLower(name), ".3mf") {
		return j, sql.ErrNoRows
	}
	if !p.Has("printers:files") {
		return j, sql.ErrNoRows
	}
	t, e := os.CreateTemp(filepath.Join(s.dir, "production"), "active-")
	if e != nil {
		return j, e
	}
	defer t.Close()
	for _, prefix := range []string{"/", "/cache/", "/model/"} {
		if e = t.Truncate(0); e != nil {
			break
		}
		if _, e = t.Seek(0, 0); e != nil {
			break
		}
		if e = s.ReadPrinterFile(ctx, p, k, prefix+name, t, 1<<30); e == nil {
			j.path = t.Name()
			j.temp = true
			return j, nil
		}
	}
	os.Remove(t.Name())
	return j, sql.ErrNoRows
}
func readMember(z *zip.ReadCloser, name string, limit uint64) ([]byte, error) {
	for _, f := range z.File {
		if f.Name == name && f.UncompressedSize64 <= limit {
			rd, e := f.Open()
			if e != nil {
				return nil, e
			}
			defer rd.Close()
			return io.ReadAll(io.LimitReader(rd, int64(limit)+1))
		}
	}
	return nil, sql.ErrNoRows
}

type objectLayout struct {
	items []row
	box   []float64
	plate int64
}

func loadLayout(z *zip.ReadCloser, want int64) (objectLayout, error) {
	out := objectLayout{items: []row{}, plate: want}
	data, e := readMember(z, "Metadata/slice_info.config", 8<<20)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return out, e
	}
	var doc slicerXML
	if e = xml.Unmarshal(data, &doc); e != nil {
		return out, fail(422, "Invalid plate metadata")
	}
	for i, pl := range doc.Plates {
		n := int64(i + 1)
		for _, m := range pl.Meta {
			if m.Key == "plater_id" && num(m.Value) > 0 {
				n = num(m.Value)
			}
		}
		if want != 0 && n != want {
			continue
		}
		out.plate = n
		var geo struct {
			Bounds  []float64 `json:"bbox_all"`
			Objects []struct {
				Name string
				Box  []float64 `json:"bbox"`
			} `json:"bbox_objects"`
		}
		if data, e := readMember(z, fmt.Sprintf("Metadata/plate_%d.json", n), 8<<20); e == nil {
			json.Unmarshal(data, &geo)
		}
		out.box = geo.Bounds
		boxes := map[string][][]float64{}
		for _, b := range geo.Objects {
			if len(b.Box) >= 4 {
				boxes[b.Name] = append(boxes[b.Name], b.Box)
			}
		}
		for _, o := range pl.Objects {
			id, e := strconv.ParseInt(o.ID, 10, 32)
			if e != nil || id < 0 || flag(o.Skipped) || o.Name == "" {
				continue
			}
			entry := row{"id": id, "name": o.Name, "x": nil, "y": nil, "skipped": false}
			if a := boxes[o.Name]; len(a) > 0 {
				b := a[0]
				entry["x"] = (b[0] + b[2]) / 2
				entry["y"] = (b[1] + b[3]) / 2
				boxes[o.Name] = a[1:]
			}
			out.items = append(out.items, entry)
		}
		break
	}
	return out, nil
}
func (s *Service) jobMedia(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		perm := "printers:read"
		if action == "skip" {
			perm = "printers:control"
		}
		pr, e := s.printerRow(r, perm)
		if e != nil {
			return nil, e
		}
		k := pathID(r)
		s.mu.RLock()
		d := s.devices[k]
		rp := row{}
		var h hardware
		if d != nil {
			h = d.h
			for n, v := range d.report {
				rp[n] = v
			}
		}
		s.mu.RUnlock()
		running := in(val(rp["gcode_state"]), "RUNNING", "PAUSE")
		out := row{"objects": []row{}, "total": 0, "skipped_count": 0, "is_printing": running, "bbox_all": nil}
		if !running && action != "cover" {
			if action == "skip" {
				return nil, fail(409, "No print is running")
			}
			return out, nil
		}
		j, e := s.currentBytes(r.Context(), access.From(r.Context()), pr, rp)
		if e != nil {
			if errors.Is(e, sql.ErrNoRows) && action == "objects" {
				return out, nil
			}
			return nil, e
		}
		if j.temp {
			defer os.Remove(j.path)
		}
		z, e := zip.OpenReader(j.path)
		if e != nil {
			return nil, sql.ErrNoRows
		}
		defer z.Close()
		layout, e := loadLayout(z, j.plate)
		if e != nil {
			return nil, e
		}
		if action == "cover" {
			if layout.plate == 0 {
				layout.plate = 1
			}
			for _, name := range []string{fmt.Sprintf("Metadata/plate_%d.png", layout.plate), fmt.Sprintf("Metadata/plate_%d_small.png", layout.plate)} {
				if data, e := readMember(z, name, 8<<20); e == nil {
					w.Header().Set("Content-Type", "image/png")
					w.Header().Set("Cache-Control", "private, max-age=5")
					_, e = w.Write(data)
					return nil, e
				}
			}
			return nil, sql.ErrNoRows
		}
		skipped := map[int64]bool{}
		if ids, ok := rp["s_obj"].([]any); ok {
			for _, v := range ids {
				skipped[num(v)] = true
			}
		}
		allowed := map[int64]bool{}
		for _, obj := range layout.items {
			n := num(obj["id"])
			allowed[n] = true
			obj["skipped"] = skipped[n]
		}
		out["objects"] = layout.items
		out["total"] = len(layout.items)
		out["skipped_count"] = len(skipped)
		out["bbox_all"] = layout.box
		if action == "objects" {
			return out, nil
		}
		var ids []int64
		if e = json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&ids); e != nil || len(ids) == 0 || len(ids) > 1000 {
			return nil, fail(422, "Select objects on the current plate")
		}
		for _, n := range ids {
			if !allowed[n] {
				return nil, fail(422, "Object is absent from the current plate")
			}
		}
		s.mu.RLock()
		current := s.devices[k]
		valid := current == d && d != nil && h != nil && d.h == h && h.Online() && in(val(d.report["gcode_state"]), "RUNNING", "PAUSE") && val(d.report["subtask_id"]) == j.subtask && val(d.report["gcode_file"]) == j.file
		s.mu.RUnlock()
		if !valid {
			return nil, fail(409, "Print changed before object selection")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if e = h.Publish(ctx, packet("print", "skip_objects", row{"obj_list": ids})); e != nil {
			return nil, fail(502, "Printer did not receive object selection")
		}
		return row{"success": true, "skipped_objects": ids, "message": "Object selection sent to printer"}, nil
	}
}
