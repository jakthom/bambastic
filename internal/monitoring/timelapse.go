package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (s *Service) registerTimelapses(public, api *http.ServeMux) {
	s.registerDownloads(public, api)
	for _, v := range []string{"scan", "select"} {
		api.HandleFunc("POST /api/v1/archives/{id}/timelapse/"+v, s.scanVideo)
	}
	for _, route := range []string{"GET /api/v1/archives/{id}/timelapse", "GET /api/v1/archives/{id}/runs/{run}/timelapse"} {
		public.HandleFunc(route, s.withMedia("archive", s.timelapse))
		if public != api {
			api.HandleFunc(route, s.timelapse)
		}
	}
	for _, route := range []string{"POST /api/v1/archives/{id}/timelapse/upload", "DELETE /api/v1/archives/{id}/timelapse", "GET /api/v1/archives/{id}/timelapse/info", "GET /api/v1/archives/{id}/timelapse/thumbnails", "GET /api/v1/archives/{id}/timelapse/job", "DELETE /api/v1/archives/{id}/timelapse/job", "POST /api/v1/archives/{id}/timelapse/process"} {
		api.HandleFunc(route, s.timelapse)
	}
}
func (s *Service) timelapse(w http.ResponseWriter, r *http.Request) {
	id := num(r.PathValue("id"))
	p := access.From(r.Context())
	ctx := r.Context()
	var media sql.NullString
	action := "read"
	if strings.HasSuffix(r.URL.Path, "/job") || strings.HasSuffix(r.URL.Path, "/process") {
		action = "update"
	} else if r.Method == "DELETE" {
		action = "delete"
	} else if r.Method == "POST" {
		action = "update"
	}
	asset, e := production.MediaAsset(ctx, s.db, p, "archives", id, action)
	if e != nil {
		httpx.Error(w, 404, "Archive not found")
		return
	}
	id = num(asString(asset["id"]))
	media = sql.NullString{String: asString(asset["timelapse_path"]), Valid: asString(asset["timelapse_path"]) != ""}
	if action == "read" && r.PathValue("run") == "" && !s.mediaVisible(r, p, id, media.String, num(asString(asset["printer_id"]))) {
		httpx.Error(w, 404, "Timelapse not found")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/job") || strings.HasSuffix(r.URL.Path, "/process") {
		s.editor(w, r, id, media.String)
		return
	}
	if r.Method == "POST" && !access.Require(w, r, "archives:update_all") {
		return
	}
	if run := r.PathValue("run"); run != "" {
		var runPrinter sql.NullInt64
		e = s.db.QueryRowContext(ctx, "SELECT printer_id,timelapse_path FROM print_log_entries WHERE archive_id=? AND id=?", id, run).Scan(&runPrinter, &media)
		if e != nil || runPrinter.Valid && !p.AllowsPrinter(runPrinter.Int64) {
			httpx.Error(w, 404, "Run not found")
			return
		}
	}
	if r.Method == "POST" {
		s.uploadVideo(w, r, id)
		return
	}
	if r.Method == "DELETE" {
		if !media.Valid {
			httpx.Error(w, 404, "No timelapse attached")
			return
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE print_archives SET timelapse_path=NULL WHERE id=? AND timelapse_path=?", id, media.String); e != nil {
			httpx.Error(w, 500, "Could not detach timelapse")
			return
		}
		s.removeUnused(ctx, media.String)
		httpx.JSON(w, 200, object{"status": "deleted"})
		return
	}
	if !media.Valid || !filepath.IsLocal(media.String) {
		httpx.Error(w, 404, "Timelapse not found")
		return
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 500, "Archive storage unavailable")
		return
	}
	defer root.Close()
	f, e := root.Open(media.String)
	if e != nil {
		httpx.Error(w, 404, "Timelapse file missing")
		return
	}
	defer f.Close()
	st, e := f.Stat()
	if e != nil || !st.Mode().IsRegular() {
		httpx.Error(w, 404, "Timelapse file unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/info") || strings.HasSuffix(r.URL.Path, "/thumbnails") {
		s.videoInfo(w, r, f, st.Size())
		return
	}
	w.Header().Set("Content-Type", mime.TypeByExtension(filepath.Ext(media.String)))
	w.Header().Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, filepath.Base(media.String), st.ModTime(), f)
}
func (s *Service) uploadVideo(w http.ResponseWriter, r *http.Request, id int64) {
	r.Body = http.MaxBytesReader(w, r.Body, 2<<30+1<<20)
	mr, e := r.MultipartReader()
	if e != nil {
		httpx.Error(w, 422, "Expected a video file")
		return
	}
	folder := filepath.Join(s.dir, "timelapses")
	if e = os.MkdirAll(folder, 0700); e != nil {
		httpx.Error(w, 500, "Cannot access media storage")
		return
	}
	for {
		part, e := mr.NextPart()
		if e != nil {
			httpx.Error(w, 422, "Video file is missing")
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			part.Close()
			continue
		}
		name := filepath.Base(part.FileName())
		ext := strings.ToLower(filepath.Ext(name))
		switch ext {
		case ".mp4", ".mkv", ".avi", ".webm", ".mov":
		default:
			httpx.Error(w, 422, "Unsupported video extension")
			return
		}
		f, e := os.CreateTemp(folder, ".upload-")
		if e != nil {
			httpx.Error(w, 500, "Cannot create video")
			return
		}
		defer os.Remove(f.Name())
		n, e := io.Copy(f, io.LimitReader(part, 2<<30+1))
		part.Close()
		closeErr := f.Close()
		if e != nil || closeErr != nil || n < 8 || n > 2<<30 {
			httpx.Error(w, 422, "Invalid or oversized video")
			return
		}
		key, _ := tokenSecret()
		rel := filepath.Join("timelapses", key+ext)
		abs := filepath.Join(s.dir, rel)
		if e = os.Rename(f.Name(), abs); e != nil {
			httpx.Error(w, 500, "Could not publish video")
			return
		}
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e != nil {
			os.Remove(abs)
			httpx.Error(w, 500, "Could not attach video")
			return
		}
		defer tx.Rollback()
		var old sql.NullString
		e = tx.QueryRowContext(r.Context(), "SELECT timelapse_path FROM print_archives WHERE id=? AND deleted_at IS NULL", id).Scan(&old)
		if e == nil {
			_, e = tx.ExecContext(r.Context(), "UPDATE print_archives SET timelapse_path=? WHERE id=?", rel, id)
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			os.Remove(abs)
			httpx.Error(w, 500, "Could not attach video")
			return
		}
		if old.Valid {
			s.removeUnused(r.Context(), old.String)
		}
		httpx.JSON(w, 200, object{"status": "attached", "filename": name})
		return
	}
}
func (s *Service) removeUnused(ctx context.Context, rel string) {
	if !filepath.IsLocal(rel) || filepath.Dir(rel) != "timelapses" {
		return
	}
	var used int
	e := s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM print_archives WHERE timelapse_path=? UNION ALL SELECT 1 FROM print_log_entries WHERE timelapse_path=?)", rel, rel).Scan(&used)
	if e == nil && used == 0 {
		_ = os.Remove(filepath.Join(s.dir, rel))
	}
}
func videoTool(ctx context.Context, name string, f *os.File, args ...string) ([]byte, error) {
	bin, e := exec.LookPath(name)
	if e != nil {
		return nil, fmt.Errorf("%s is required on the Go server", name)
	}
	if _, e = f.Seek(0, 0); e != nil {
		return nil, e
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.ExtraFiles = []*os.File{f}
	out, e := cmd.Output()
	if e != nil {
		return nil, errors.New("Video tool could not read this recording")
	}
	return out, nil
}
func (s *Service) videoInfo(w http.ResponseWriter, r *http.Request, f *os.File, size int64) {
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	raw, e := videoTool(ctx, "ffprobe", f, "-v", "error", "-show_entries", "stream=codec_type,codec_name,width,height,avg_frame_rate:format=duration", "-of", "json", "/dev/fd/3")
	if e != nil {
		httpx.Error(w, 503, e.Error())
		return
	}
	var probe struct {
		Streams []struct {
			Type   string `json:"codec_type"`
			Codec  string `json:"codec_name"`
			Width  int
			Height int
			Rate   string `json:"avg_frame_rate"`
		}
		Format struct{ Duration string }
	}
	if json.Unmarshal(raw, &probe) != nil {
		httpx.Error(w, 422, "Unreadable video metadata")
		return
	}
	duration, _ := strconv.ParseFloat(probe.Format.Duration, 64)
	info := object{"duration": duration, "file_size": size, "has_audio": false}
	for _, st := range probe.Streams {
		if st.Type == "audio" {
			info["has_audio"] = true
		}
		if st.Type == "video" {
			info["codec"] = st.Codec
			info["width"] = st.Width
			info["height"] = st.Height
			v, w, _ := strings.Cut(st.Rate, "/")
			a, _ := strconv.ParseFloat(v, 64)
			b, _ := strconv.ParseFloat(w, 64)
			if b == 0 {
				b = 1
			}
			info["fps"] = a / b
		}
	}
	if strings.HasSuffix(r.URL.Path, "/info") {
		httpx.JSON(w, 200, info)
		return
	}
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	if count == 0 {
		count = 10
	}
	count = max(1, min(count, 40))
	thumbs := []string{}
	times := []float64{}
	for i := 0; i < count; i++ {
		ts := duration * float64(i) / float64(count)
		b, e := videoTool(ctx, "ffmpeg", f, "-nostdin", "-hide_banner", "-loglevel", "error", "-ss", fmt.Sprintf("%.3f", ts), "-i", "/dev/fd/3", "-frames:v", "1", "-vf", "scale=240:-2", "-vcodec", "mjpeg", "-f", "image2pipe", "pipe:1")
		if e != nil {
			httpx.Error(w, 503, e.Error())
			return
		}
		thumbs = append(thumbs, base64.StdEncoding.EncodeToString(b))
		times = append(times, ts)
	}
	httpx.JSON(w, 200, object{"thumbnails": thumbs, "timestamps": times})
}
