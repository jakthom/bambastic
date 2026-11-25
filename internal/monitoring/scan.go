package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

type fileFarm interface {
	ListPrinterFiles(context.Context, access.Principal, int64, string) ([]map[string]any, error)
	ReadPrinterFile(context.Context, access.Principal, int64, string, io.Writer, int64) error
}
type printerVideo struct {
	Name  string     `json:"name"`
	Path  string     `json:"path"`
	Size  int64      `json:"size"`
	Mtime *time.Time `json:"mtime"`
}

var videoDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}`)

func mediaTime(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, err := time.Parse(layout, asString(v)); err == nil {
			return t
		}
	}
	return time.Time{}
}
func videoListing(ctx context.Context, farm fileFarm, p access.Principal, id int64, dir string) ([]printerVideo, error) {
	files, err := farm.ListPrinterFiles(ctx, p, id, dir)
	var out []printerVideo
	for _, row := range files {
		name := asString(row["name"])
		ext := strings.ToLower(path.Ext(name))
		if (ext != ".mp4" && ext != ".avi") || strings.ContainsAny(name, "/\\\r\n\x00") || row["is_dir"] == true || name == "" {
			continue
		}
		v := printerVideo{Name: name, Path: path.Join(dir, name), Size: num(asString(row["size"]))}
		if t := mediaTime(row["mtime"]); !t.IsZero() {
			v.Mtime = &t
		}
		out = append(out, v)
	}
	return out, err
}
func selectVideo(files []printerVideo, claimed map[string]bool, asset object) (*printerVideo, []printerVideo) {
	eligible := []printerVideo{}
	var baseline []string
	hasBaseline := json.Unmarshal([]byte(asString(asset["timelapse_baseline"])), &baseline) == nil && baseline != nil
	old := map[string]bool{}
	for _, n := range baseline {
		old[n] = true
	}
	for _, v := range files {
		if !old[v.Name] && !claimed[strings.TrimSuffix(v.Name, path.Ext(v.Name))] {
			eligible = append(eligible, v)
		}
	}
	if hasBaseline {
		if len(eligible) == 1 {
			return &eligible[0], eligible
		}
		if len(eligible) > 0 {
			return nil, eligible
		}
		return nil, files
	}
	base := strings.ToLower(path.Base(asString(asset["filename"])))
	base = strings.TrimSuffix(strings.TrimSuffix(base, ".3mf"), ".gcode")
	for i, v := range eligible {
		if len(base) > 0 && strings.Contains(strings.ToLower(v.Name), base) {
			return &eligible[i], files
		}
	}
	start := mediaTime(asset["started_at"])
	if !start.IsZero() {
		type near struct {
			idx   int
			delta time.Duration
		}
		choices := []near{}
		for i, v := range eligible {
			stamp, err := time.Parse("2006-01-02_15-04-05", videoDate.FindString(v.Name))
			if err != nil {
				continue
			}
			best := time.Duration(1<<63 - 1)
			for _, h := range []int{0, 8, -8, 7, -7, 1, -1} {
				delta := stamp.Add(-time.Duration(h) * time.Hour).Sub(start).Abs()
				if delta < best {
					best = delta
				}
			}
			if best <= 4*time.Hour {
				choices = append(choices, near{i, best})
			}
		}
		sort.Slice(choices, func(i, j int) bool { return choices[i].delta < choices[j].delta })
		if len(choices) == 1 || len(choices) > 1 && choices[1].delta-choices[0].delta >= 15*time.Minute {
			return &eligible[choices[0].idx], files
		}
	}
	end := mediaTime(asset["completed_at"])
	if end.IsZero() {
		end = mediaTime(asset["created_at"])
	}
	if !end.IsZero() {
		var best *printerVideo
		limit := 2 * time.Hour
		for i, v := range eligible {
			if v.Mtime != nil {
				delta := v.Mtime.Sub(end).Abs()
				if delta < limit {
					limit = delta
					best = &eligible[i]
				}
			}
		}
		if best != nil {
			return best, files
		}
		if age := time.Since(end); len(eligible) == 1 && age >= 0 && age < time.Hour {
			return &eligible[0], files
		}
	}
	return nil, files
}
func (s *Service) scanVideo(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "archives:update_all") || !access.Require(w, r, "printers:files") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	asset, err := production.MediaAsset(ctx, s.db, p, "archives", num(r.PathValue("id")), "update")
	if err != nil {
		httpx.Error(w, 404, "Archive not found")
		return
	}
	id := num(asString(asset["id"]))
	pid := num(asString(asset["printer_id"]))
	if pid == 0 {
		httpx.Error(w, 400, "Archive has no associated printer")
		return
	}
	current := asString(asset["timelapse_path"])
	manual := strings.HasSuffix(r.URL.Path, "/select")
	if current != "" && !manual {
		httpx.JSON(w, 200, object{"status": "exists", "message": "Timelapse already attached"})
		return
	}
	farm, ok := s.farm.(fileFarm)
	if !ok {
		httpx.Error(w, 503, "Printer storage unavailable")
		return
	}
	list := []printerVideo{}
	reachable := false
	for _, dir := range []string{"/timelapse", "/timelapse/video", "/record", "/recording"} {
		files, e := videoListing(ctx, farm, p, pid, dir)
		if e == nil {
			reachable = true
			list = append(list, files...)
		}
		if ctx.Err() != nil {
			return
		}
	}
	if !reachable {
		httpx.Error(w, 503, "Cannot reach the printer's file service")
		return
	}
	var file *printerVideo
	if manual {
		wanted := r.URL.Query().Get("filename")
		for i, v := range list {
			if v.Name == wanted {
				file = &list[i]
				break
			}
		}
		if file == nil {
			httpx.Error(w, 404, "Selected video is unavailable")
			return
		}
	} else {
		rows, e := s.db.QueryContext(ctx, "SELECT timelapse_path FROM print_archives WHERE id<>? AND printer_id=? AND timelapse_path IS NOT NULL UNION SELECT timelapse_path FROM print_log_entries WHERE archive_id<>? AND printer_id=? AND timelapse_path IS NOT NULL", id, pid, id, pid)
		if e != nil {
			httpx.Error(w, 500, "Cannot read archive history")
			return
		}
		claimed := map[string]bool{}
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				break
			}
			name := path.Base(n)
			claimed[strings.TrimSuffix(name, path.Ext(name))] = true
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			httpx.Error(w, 500, "Cannot read archive history")
			return
		}
		file, list = selectVideo(list, claimed, asset)
		if file == nil {
			sort.SliceStable(list, func(i, j int) bool {
				return list[i].Mtime != nil && (list[j].Mtime == nil || list[i].Mtime.After(*list[j].Mtime))
			})
			httpx.JSON(w, 200, object{"status": "not_found", "message": "Choose a recording from the printer", "available_files": list})
			return
		}
	}
	if file.Size <= 0 || file.Size > 2<<30 {
		httpx.Error(w, 422, "Invalid or oversized recording")
		return
	}
	key, _ := tokenSecret()
	folder := filepath.Join("timelapses", key)
	root, err := os.OpenRoot(s.dir)
	if err != nil {
		httpx.Error(w, 500, "Media storage unavailable")
		return
	}
	defer root.Close()
	if err = root.MkdirAll(folder, 0700); err != nil {
		httpx.Error(w, 500, "Cannot create media directory")
		return
	}
	committed := false
	defer func() {
		if !committed {
			_ = root.RemoveAll(folder)
		}
	}()
	rel := filepath.Join(folder, file.Name)
	out, err := root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		httpx.Error(w, 500, "Cannot create media file")
		return
	}
	err = farm.ReadPrinterFile(ctx, p, pid, file.Path, out, file.Size)
	stat, se := out.Stat()
	ce := out.Close()
	if err != nil || se != nil || ce != nil || stat.Size() != file.Size {
		httpx.Error(w, 502, "Could not retrieve the full recording")
		return
	}
	// Re-read the remote listing after the pull to avoid retaining a growing file.
	again, err := videoListing(ctx, farm, p, pid, path.Dir(file.Path))
	settled := false
	for _, v := range again {
		if v.Name == file.Name && v.Size == file.Size && (v.Mtime == nil || file.Mtime == nil || v.Mtime.Equal(*file.Mtime)) {
			settled = true
			break
		}
	}
	if err != nil || !settled {
		httpx.Error(w, 409, "The recording is still changing")
		return
	}
	if _, err = production.MediaAsset(ctx, s.db, p, "archives", id, "update"); err != nil {
		httpx.Error(w, 404, "Archive not found")
		return
	}
	result, err := s.db.ExecContext(ctx, "UPDATE print_archives SET timelapse_path=? WHERE id=? AND printer_id=? AND coalesce(timelapse_path,'')=? AND deleted_at IS NULL", rel, id, pid, current)
	if err != nil {
		httpx.Error(w, 500, "Cannot attach recording")
		return
	}
	n, _ := result.RowsAffected()
	if n != 1 {
		httpx.Error(w, 409, "Archive changed during the transfer")
		return
	}
	committed = true
	if current != "" {
		s.removeUnused(ctx, current)
	}
	httpx.JSON(w, 200, object{"status": "attached", "message": "Timelapse attached", "filename": file.Name})
}
