package slicing

import (
	"archive/zip"
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type previewRef struct {
	remote, base, kind string
	asset              int64
	expiry             time.Time
}

var previewName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,150}$`)

func previewKey(p access.Principal, request string) string {
	return fmt.Sprintf("%d/%d/%s", p.ID, p.KeyID, request)
}
func (s *Service) previewProgress(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	s.mu.Lock()
	v, ok := s.previews[previewKey(p, r.PathValue("request"))]
	s.mu.Unlock()
	if !ok || time.Now().After(v.expiry) {
		httpx.Error(w, 404, "No active preview")
		return
	}
	if _, e := production.MediaAsset(r.Context(), s.db, p, v.kind, v.asset, "read"); e != nil {
		httpx.Error(w, 404, "No active preview")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var snap object
	code, e := s.jsonCall(ctx, "GET", v.base+"/slice/progress/"+v.remote, nil, &snap)
	if e != nil {
		if code == 404 {
			httpx.Error(w, 404, "Progress unavailable")
		} else {
			httpx.Error(w, 503, "Slicer progress unavailable")
		}
		return
	}
	httpx.JSON(w, 200, snap)
}
func previewUsage(z *zip.Reader, plate int) ([]map[string]any, bool) {
	for _, f := range z.File {
		if f.Name != "Metadata/slice_info.config" || f.UncompressedSize64 > 8<<20 {
			continue
		}
		r, e := f.Open()
		if e != nil {
			return nil, false
		}
		var doc struct {
			Plates []struct {
				Meta []struct {
					Key   string `xml:"key,attr"`
					Value string `xml:"value,attr"`
				} `xml:"metadata"`
				Filaments []struct {
					ID     int     `xml:"id,attr"`
					Type   string  `xml:"type,attr"`
					Color  string  `xml:"color,attr"`
					Grams  float64 `xml:"used_g,attr"`
					Meters float64 `xml:"used_m,attr"`
					Preset string  `xml:"tray_info_idx,attr"`
				} `xml:"filament"`
			} `xml:"plate"`
		}
		e = xml.NewDecoder(io.LimitReader(r, 8<<20)).Decode(&doc)
		r.Close()
		if e != nil {
			return nil, false
		}
		for n, p := range doc.Plates {
			id := n + 1
			for _, m := range p.Meta {
				if (m.Key == "index" || m.Key == "plater_id") && number(m.Value) > 0 {
					id = int(number(m.Value))
				}
			}
			if id != plate {
				continue
			}
			out := []map[string]any{}
			for _, f := range p.Filaments {
				if f.ID > 0 {
					out = append(out, object{"slot_id": f.ID, "type": f.Type, "color": f.Color, "used_grams": f.Grams, "used_meters": f.Meters, "tray_info_idx": f.Preset, "used_in_plate": true})
				}
			}
			return out, true
		}
	}
	return nil, false
}
func allSlots(settings object, used []map[string]any, known bool) []map[string]any {
	asList := func(v any) []any {
		if a, ok := v.([]any); ok {
			return a
		}
		if str, ok := v.(string); ok && str != "" {
			return []any{str}
		}
		return nil
	}
	types, colors, ids := asList(settings["filament_type"]), asList(settings["filament_colour"]), asList(settings["filament_ids"])
	n := max(len(types), len(colors), len(ids))
	entries := map[int64]object{}
	for _, v := range used {
		slot := number(v["slot_id"])
		if slot > 0 {
			entries[slot] = v
		}
	}
	get := func(v []any, i int) string {
		if i < len(v) {
			return asText(v[i])
		}
		return ""
	}
	for i := 0; i < n; i++ {
		key := int64(i + 1)
		if entries[key] == nil {
			entries[key] = object{"slot_id": key, "type": get(types, i), "color": get(colors, i), "tray_info_idx": get(ids, i), "used_grams": 0, "used_meters": 0, "used_in_plate": !known}
		}
	}
	out := make([]map[string]any, 0, len(entries))
	for _, v := range entries {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return number(out[i]["slot_id"]) < number(out[j]["slot_id"]) })
	return out
}
func (s *Service) previewFilaments(ctx context.Context, p access.Principal, kind string, id int64, plate int, reqID string, full bool) ([]map[string]any, error) {
	asset, e := production.MediaAsset(ctx, s.db, p, kind, id, "read")
	if e != nil {
		return nil, e
	}
	name, rel := asText(asset["filename"]), asText(asset["file_path"])
	if !strings.EqualFold(filepath.Ext(name), ".3mf") {
		return nil, nil
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		return nil, e
	}
	defer root.Close()
	f, e := root.Open(rel)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil {
		return nil, e
	}
	z, e := zip.NewReader(f, st.Size())
	if e != nil {
		return nil, e
	}
	settings := projectSettings(z)
	used, known := previewUsage(z, plate)
	if !known && settings != nil {
		used, e = s.analyzeFilaments(ctx, p, kind, id, plate, reqID, name, f)
		known = e == nil
	}
	if full || !known {
		return allSlots(settings, used, known), nil
	}
	return used, nil
}
func (s *Service) analyzeFilaments(request context.Context, p access.Principal, kind string, id int64, plate int, reqID, name string, source *os.File) ([]map[string]any, error) {
	_, base, e := s.endpoint(request)
	if e != nil {
		return nil, e
	}
	remote := idString()
	key := previewKey(p, reqID)
	named := previewName.MatchString(reqID)
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		return nil, errors.New("Slice service is stopping")
	}
	if named {
		for k, v := range s.previews {
			if time.Now().After(v.expiry) {
				delete(s.previews, k)
			}
		}
		if len(s.previews) >= 256 {
			s.mu.Unlock()
			return nil, errors.New("Too many preview jobs")
		}
		s.previews[key] = previewRef{remote, base, kind, id, time.Now().Add(20 * time.Minute)}
	}
	s.wg.Add(1)
	s.mu.Unlock()
	defer s.wg.Done()
	defer func() {
		if named {
			s.mu.Lock()
			if v := s.previews[key]; v.remote == remote {
				v.expiry = time.Now().Add(20 * time.Second)
				s.previews[key] = v
			}
			s.mu.Unlock()
		}
	}()
	ctx, stop := context.WithTimeout(request, 15*time.Minute)
	defer stop()
	release := context.AfterFunc(s.ctx, stop)
	defer release()
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	j := &job{source: sourceFile{name: name}}
	res, e := s.transfer(ctx, j, sliceRequest{Plate: &plate}, recipe{embedded: true}, source, base, remote)
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return nil, errors.New("Preview failed")
	}
	tmp, e := os.CreateTemp(filepath.Join(s.dir, "slices"), ".preview-")
	if e != nil {
		return nil, e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, e := io.Copy(tmp, io.LimitReader(res.Body, 2<<30+1))
	if e != nil || n > 2<<30 {
		return nil, errors.New("Invalid sliced preview")
	}
	z, e := zip.NewReader(tmp, n)
	if e != nil {
		return nil, e
	}
	out, ok := previewUsage(z, plate)
	if !ok {
		return nil, errors.New("No filament assessment for the selected plate")
	}
	return out, nil
}
