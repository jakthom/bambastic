package monitoring

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type editJob struct {
	status                  string
	progress                *float64
	message, result, detail string
	cancel                  context.CancelFunc
}

func clean(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func (j *editJob) view() object {
	return object{"status": j.status, "progress": j.progress, "message": clean(j.message), "output_path": clean(j.result), "error": clean(j.detail)}
}
func (s *Service) editor(w http.ResponseWriter, r *http.Request, id int64, rel string) {
	if !access.Require(w, r, "archives:update_all") {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/job") {
		s.mu.Lock()
		defer s.mu.Unlock()
		j := s.edits[id]
		if j == nil {
			httpx.Error(w, 404, "No timelapse processing job")
			return
		}
		if r.Method == "DELETE" && j.status == "running" {
			j.cancel()
			j.status = "cancelled"
		}
		httpx.JSON(w, 200, j.view())
		return
	}
	if !filepath.IsLocal(rel) {
		httpx.Error(w, 404, "No source recording")
		return
	}
	if _, e := exec.LookPath("ffmpeg"); e != nil {
		httpx.Error(w, 503, "Install ffmpeg to edit timelapses")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 65<<20)
	if e := r.ParseMultipartForm(2 << 20); e != nil {
		httpx.Error(w, 422, "Expected timelapse editing options")
		return
	}
	defer r.MultipartForm.RemoveAll()
	number := func(name string, fallback float64) float64 {
		v := r.FormValue(name)
		if v == "" {
			return fallback
		}
		x, e := strconv.ParseFloat(v, 64)
		if e != nil || math.IsNaN(x) || math.IsInf(x, 0) {
			return -1
		}
		return x
	}
	start, end, speed, vol := number("trim_start", 0), number("trim_end", 0), number("speed", 1), number("audio_volume", 1)
	mode := r.FormValue("save_mode")
	if mode == "" {
		mode = "new"
	}
	if start < 0 || end < 0 || end > 0 && end <= start || speed < 0.25 || speed > 4 || vol < 0 || vol > 1 || mode != "replace" && mode != "new" {
		httpx.Error(w, 422, "Invalid trim range, speed, audio volume or save mode")
		return
	}
	name := r.FormValue("output_filename")
	if name == "" {
		name = "timelapse_edited.mp4"
	}
	if filepath.Base(name) != name || !filepath.IsLocal(name) || strings.ContainsAny(name, "/\\\x00") || !strings.EqualFold(filepath.Ext(name), ".mp4") {
		httpx.Error(w, 422, "Choose an MP4 filename")
		return
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 500, "Cannot open media storage")
		return
	}
	src, e := root.Open(rel)
	root.Close()
	if e != nil {
		httpx.Error(w, 404, "Source recording not found")
		return
	}
	dir := filepath.Join(s.dir, "timelapses")
	if e = os.MkdirAll(dir, 0700); e != nil {
		src.Close()
		httpx.Error(w, 500, "Cannot open output folder")
		return
	}
	var audio string
	if file, _, err := r.FormFile("audio"); err == nil {
		tmp, err := os.CreateTemp(dir, ".audio-")
		if err != nil {
			src.Close()
			file.Close()
			httpx.Error(w, 500, "Cannot save soundtrack")
			return
		}
		n, err := io.Copy(tmp, io.LimitReader(file, 64<<20+1))
		file.Close()
		tmp.Close()
		audio = tmp.Name()
		if err != nil || n > 64<<20 {
			os.Remove(audio)
			src.Close()
			httpx.Error(w, 413, "Soundtrack exceeds 64 MiB")
			return
		}
	} else if !errors.Is(err, http.ErrMissingFile) {
		src.Close()
		httpx.Error(w, 422, "Could not read soundtrack")
		return
	}
	s.mu.Lock()
	if j := s.edits[id]; s.closing || j != nil && j.status == "running" {
		s.mu.Unlock()
		src.Close()
		if audio != "" {
			os.Remove(audio)
		}
		httpx.Error(w, 409, "This timelapse is already being processed")
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &editJob{cancel: cancel, status: "running"}
	s.edits[id] = j
	s.wg.Add(1)
	initial := j.view()
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer cancel()
		defer src.Close()
		if audio != "" {
			defer os.Remove(audio)
		}
		key, _ := tokenSecret()
		tmp, e := os.CreateTemp(dir, ".render-*.mp4")
		if e != nil {
			s.finishEdit(j, e, "")
			return
		}
		tmp.Close()
		defer os.Remove(tmp.Name())
		args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
		if start > 0 {
			args = append(args, "-ss", fmt.Sprint(start))
		}
		args = append(args, "-i", "/dev/fd/3")
		if audio != "" {
			args = append(args, "-stream_loop", "-1", "-i", audio, "-map", "0:v:0", "-map", "1:a:0", "-af", fmt.Sprintf("volume=%g", vol), "-shortest")
		} else {
			args = append(args, "-map", "0:v:0", "-map", "0:a?", "-af", fmt.Sprintf("atempo=%g,volume=%g", math.Sqrt(speed), vol)+fmt.Sprintf(",atempo=%g", math.Sqrt(speed)))
		}
		if end > 0 {
			args = append(args, "-t", fmt.Sprint((end-start)/speed))
		}
		args = append(args, "-vf", fmt.Sprintf("setpts=(PTS-STARTPTS)/%g", speed), "-c:v", "libx264", "-c:a", "aac", "-preset", "veryfast", "-movflags", "+faststart", "-progress", "pipe:1", tmp.Name())
		cmd := exec.CommandContext(ctx, "ffmpeg", args...)
		cmd.ExtraFiles = []*os.File{src}
		out, e := cmd.StdoutPipe()
		if e == nil {
			e = cmd.Start()
		}
		if e != nil {
			s.finishEdit(j, errors.New("Cannot start video processing"), "")
			return
		}
		sc := bufio.NewScanner(out)
		for sc.Scan() {
			k, v, _ := strings.Cut(sc.Text(), "=")
			if k == "out_time_us" && end > start {
				us, _ := strconv.ParseFloat(v, 64)
				progress := min(1.0, max(0.0, us/1e6*speed/(end-start)))
				s.mu.Lock()
				j.progress = &progress
				s.mu.Unlock()
			}
		}
		e = cmd.Wait()
		if e != nil {
			if ctx.Err() != nil {
				e = ctx.Err()
			} else {
				e = errors.New("ffmpeg could not process this video or soundtrack")
			}
			s.finishEdit(j, e, "")
			return
		}
		destRel := filepath.Join("timelapses", key+"-"+name)
		dest := filepath.Join(s.dir, destRel)
		s.mu.Lock()
		defer s.mu.Unlock()
		if j.status == "cancelled" || ctx.Err() != nil {
			return
		}
		if e = os.Rename(tmp.Name(), dest); e == nil && mode == "replace" {
			res, err := s.db.Exec("UPDATE print_archives SET timelapse_path=? WHERE id=? AND timelapse_path=? AND deleted_at IS NULL", destRel, id, rel)
			e = err
			if err == nil {
				n, _ := res.RowsAffected()
				if n == 0 {
					e = errors.New("Source recording changed before processing finished")
				}
			}
		}
		if e != nil {
			os.Remove(dest)
			j.status = "failed"
			j.detail = e.Error()
			return
		}
		j.status = "completed"
		j.result = destRel
		j.message = "Timelapse saved"
		done := 1.0
		j.progress = &done
		if mode == "replace" {
			s.removeUnused(context.Background(), rel)
		}
	}()
	httpx.JSON(w, 200, initial)
}
func (s *Service) finishEdit(j *editJob, err error, result string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if j.status == "cancelled" {
		return
	}
	if err != nil {
		j.status = "failed"
		j.detail = err.Error()
	} else {
		j.status = "completed"
		j.result = result
	}
}
