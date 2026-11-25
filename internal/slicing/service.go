// Package slicing runs operator-requested slice jobs against the configured HTTP sidecar.
package slicing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type object = map[string]any
type Cloud interface {
	CloudProfiles(context.Context, access.Principal, string) (map[string][]object, string)
	ResolvePreset(context.Context, access.Principal, string, string, string) (object, error)
}

func (s *Service) AttachCloud(c Cloud) { s.cloud = c }

type Service struct {
	previews map[string]previewRef
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	closing  bool
	cloud    Cloud
	farm     Farm
	db       *sql.DB
	dir      string
	mu       sync.Mutex
	next     int64
	jobs     map[int64]*job
	slots    chan struct{}
	client   *http.Client
}
type job struct {
	principal         access.Principal
	libraryOutput     bool
	id, owner         int64
	status, kind      string
	source            sourceFile
	created           time.Time
	started, finished *time.Time
	progress, result  object
	statusCode        int
	detail            string
}
type sourceFile struct {
	id                                      int64
	name, path                              string
	folderID, projectID, printerID, ownerID sql.NullInt64
}

func New(db *sql.DB, dataDir string) (*Service, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(dir, "slices"), 0700); err != nil {
		return nil, err
	}
	if err = recoverRuns(db); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{ctx: ctx, cancel: cancel, db: db, dir: dir, previews: map[string]previewRef{}, jobs: make(map[int64]*job), slots: make(chan struct{}, 2), client: &http.Client{}}, nil
}
func (s *Service) Register(mux *http.ServeMux) {
	s.pipelineRoutes(mux)
	mux.HandleFunc("GET /api/v1/slicer/preview-progress/{request}", s.previewProgress)
	for _, route := range []string{"GET /api/v1/local-presets/base-cache/status", "POST /api/v1/local-presets/base-cache/refresh", "POST /api/v1/local-presets/reclassify"} {
		mux.HandleFunc(route, s.cacheHTTP)
	}
	const base = "/api/v1/"
	mux.HandleFunc("POST "+base+"library/files/{id}/slice", s.submit("library_file"))
	mux.HandleFunc("POST "+base+"archives/{id}/slice", s.submit("archive"))
	mux.HandleFunc("GET "+base+"slice-jobs/{id}", s.poll)
	mux.HandleFunc("GET "+base+"slicer/presets", s.unified)
	mux.HandleFunc("GET "+base+"slicer/preset-values", s.values)
	mux.HandleFunc("GET "+base+"slicer/printer-models", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(printerModels)
	})
	mux.HandleFunc("GET "+base+"local-presets/{$}", s.listPresets)
	mux.HandleFunc("GET "+base+"local-presets/{id}", s.getPreset)
	mux.HandleFunc("POST "+base+"local-presets/{$}", s.savePreset)
	mux.HandleFunc("PUT "+base+"local-presets/{id}", s.savePreset)
	mux.HandleFunc("DELETE "+base+"local-presets/{id}", s.deletePreset)
	mux.HandleFunc("POST "+base+"local-presets/import", s.importPresets)
}
func (s *Service) endpoint(ctx context.Context) (engine, base string, err error) {
	engine, err = access.Setting(ctx, s.db, "preferred_slicer", "bambu_studio")
	if err != nil {
		return
	}
	var key, env, def string
	switch engine {
	case "bambu_studio":
		key, env, def = "bambu_studio_api_url", "BAMBU_STUDIO_API_URL", "http://localhost:3001"
	case "orcaslicer":
		key, env, def = "orcaslicer_api_url", "SLICER_API_URL", "http://localhost:3003"
	default:
		return engine, "", errors.New("Choose a supported slicer in settings")
	}
	if v := os.Getenv(env); v != "" {
		def = v
	}
	base, err = access.Setting(ctx, s.db, key, def)
	if err != nil {
		return
	}
	if strings.TrimSpace(base) == "" {
		base = def
	}
	u, e := url.Parse(base)
	if e != nil || u.Host == "" || u.Scheme != "http" && u.Scheme != "https" {
		return engine, "", errors.New("Configure a valid HTTP slicer URL in settings")
	}
	return engine, strings.TrimRight(base, "/"), nil
}
func (s *Service) sourceFor(ctx context.Context, p access.Principal, kind, id string) (sourceFile, int, error) {
	src := sourceFile{}
	var e error
	if kind == "library_file" {
		e = s.db.QueryRowContext(ctx, "SELECT id,filename,file_path,folder_id,project_id,created_by_id FROM library_files WHERE id=? AND deleted_at IS NULL", id).Scan(&src.id, &src.name, &src.path, &src.folderID, &src.projectID, &src.ownerID)
	} else {
		e = s.db.QueryRowContext(ctx, "SELECT id,filename,COALESCE(NULLIF(source_3mf_path,''),file_path),printer_id,project_id,created_by_id FROM print_archives WHERE id=? AND deleted_at IS NULL", id).Scan(&src.id, &src.name, &src.path, &src.printerID, &src.projectID, &src.ownerID)
	}
	family := "library"
	if kind == "archive" {
		family = "archives"
	}
	owns := p.ID != 0 && src.ownerID.Valid && p.ID == src.ownerID.Int64
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return src, 500, errors.New("Could not read the slice source")
	}
	if e != nil || !p.Has(family+":read_all") && !(owns && p.Has(family+":read_own")) || kind == "archive" && src.printerID.Valid && !p.AllowsPrinter(src.printerID.Int64) {
		return src, 404, errors.New("Slice source not found")
	}
	if kind == "archive" && !p.Has("archives:reprint_all") && !(owns && p.Has("archives:reprint_own")) {
		return src, 403, errors.New("Archive reprint permission required")
	}
	return src, 200, nil
}
func (s *Service) submit(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := access.From(r.Context())
		if !access.Require(w, r, "library:upload") {
			return
		}
		src, code, e := s.sourceFor(r.Context(), p, kind, r.PathValue("id"))
		if e != nil {
			httpx.Error(w, code, e.Error())
			return
		}
		ext := strings.ToLower(filepath.Ext(src.name))
		if ext != ".stl" && ext != ".3mf" {
			httpx.Error(w, 400, "Source must be an STL or 3MF file")
			return
		}
		var req sliceRequest
		if !httpx.Decode(w, r, &req) {
			return
		}
		engine, base, e := s.endpoint(r.Context())
		if e != nil {
			httpx.Error(w, 400, e.Error())
			return
		}
		root, e := os.OpenRoot(s.dir)
		if e != nil {
			httpx.Error(w, 500, "Cannot open storage")
			return
		}
		defer root.Close()
		f, e := root.Open(src.path)
		if e != nil {
			httpx.Error(w, 404, "Slice source missing on disk")
			return
		}
		profiles, e := s.profiles(r.Context(), req, engine, ext, f)
		if e != nil {
			f.Close()
			httpx.Error(w, 422, e.Error())
			return
		}
		j, e := s.startJob(p, src, kind, false, req, profiles, base, f, nil)
		if e != nil {
			httpx.Error(w, 429, e.Error())
			return
		}
		httpx.JSON(w, 202, object{"job_id": j.id, "status_url": fmt.Sprintf("/api/v1/slice-jobs/%d", j.id)})
	}
}
func (s *Service) poll(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	p := access.From(r.Context())
	s.mu.Lock()
	j := s.jobs[id]
	if j == nil || (j.finished != nil && time.Since(*j.finished) > 30*time.Minute) || !p.Has("library:read_all") && !(p.ID > 0 && p.ID == j.owner && p.Has("library:read_own")) {
		s.mu.Unlock()
		httpx.Error(w, 404, "Slice job not found or expired")
		return
	}
	out := object{"job_id": j.id, "source_id": j.source.id, "source_name": j.source.name, "kind": j.kind, "status": j.status, "created_at": j.created, "started_at": j.started, "completed_at": j.finished, "progress": j.progress}
	if j.result != nil {
		out["result"] = j.result
	}
	if j.detail != "" {
		out["error_status"] = j.statusCode
		out["error_detail"] = j.detail
	}
	s.mu.Unlock()
	httpx.JSON(w, 200, out)
}
func (s *Service) jsonCall(ctx context.Context, method, address string, payload any, dest any) (int, error) {
	var content string
	if payload != nil {
		b, e := json.Marshal(payload)
		if e != nil {
			return 0, e
		}
		content = string(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, address, strings.NewReader(content))
	if e != nil {
		return 0, e
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, e := s.client.Do(req)
	if e != nil {
		return 0, e
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return res.StatusCode, fmt.Errorf("sidecar returned %s", res.Status)
	}
	return res.StatusCode, json.NewDecoder(http.MaxBytesReader(nil, res.Body, 8<<20)).Decode(dest)
}

func (s *Service) startJob(actor access.Principal, src sourceFile, kind string, libraryOutput bool, req sliceRequest, profiles recipe, base string, f *os.File, complete func(object, error)) (*job, error) {
	s.mu.Lock()
	if s.closing {
		s.mu.Unlock()
		f.Close()
		return nil, errors.New("Slicer service is shutting down")
	}
	now := time.Now().UTC()
	for id, j := range s.jobs {
		if j.finished != nil && now.Sub(*j.finished) > 30*time.Minute {
			delete(s.jobs, id)
		}
	}
	active := 0
	for _, j := range s.jobs {
		if j.finished == nil {
			active++
		}
	}
	if active >= 32 {
		s.mu.Unlock()
		f.Close()
		return nil, errors.New("Slice queue is full")
	}
	s.next++
	j := &job{id: s.next, owner: actor.ID, principal: actor, source: src, created: now, status: "pending", kind: kind, libraryOutput: libraryOutput}
	s.jobs[j.id] = j
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer f.Close()
		s.slots <- struct{}{}
		defer func() { <-s.slots }()
		now := time.Now().UTC()
		s.mu.Lock()
		j.started = &now
		j.status = "running"
		s.mu.Unlock()
		result, code, err := s.slice(j, req, profiles, base, f)
		done := time.Now().UTC()
		s.mu.Lock()
		j.finished = &done
		if err != nil {
			j.status = "failed"
			j.statusCode = code
			j.detail = err.Error()
		} else {
			j.status = "completed"
			j.result = result
		}
		s.mu.Unlock()
		if complete != nil {
			complete(result, err)
		}
	}()
	return j, nil
}

// Close cancels active sidecar requests and waits for their result transactions.
func (s *Service) Close() { s.mu.Lock(); s.closing = true; s.cancel(); s.mu.Unlock(); s.wg.Wait() }
