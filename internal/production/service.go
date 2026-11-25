// Package production manages printable assets, the dispatch queue and physical executions.
package production

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
)

type row map[string]any
type database interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}
type Service struct {
	ResolveSlotPreset        func(context.Context, access.Principal, string, string, string) (map[string]any, error)
	SpoolmanMaterialIdentity func(context.Context, int64) (map[string]any, error)
	externalPending          map[string]externalIntent
	preview                  func(context.Context, access.Principal, string, int64, int, string, bool) ([]map[string]any, error)
	auth                     *access.Service
	jobStops                 map[string]context.CancelFunc
	db                       *sql.DB
	dir                      string
	mu                       sync.RWMutex
	sink                     func(map[string]any)
	resolve                  func(context.Context, int64) (access.Principal, error)
	devices                  map[int64]*liveDevice
	open                     openDevice
	heatMu                   sync.Mutex
	firmwareMu               sync.Mutex
	firmwareTransport        http.RoundTripper
	openCard                 func(context.Context, row) (card, error)
	probePort                func(context.Context, string, string, string, bool) error
	dispatch                 sync.Mutex
	dryMu                    sync.Mutex
	wake                     chan struct{}
}

func New(db *sql.DB, dataDir string) (*Service, error) {
	dir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(filepath.Join(dir, "production", "objects"), 0700); err != nil {
		return nil, err
	}
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS bb_external_commits(id TEXT PRIMARY KEY);
CREATE TABLE IF NOT EXISTS bb_reclaim(path TEXT PRIMARY KEY);
CREATE TRIGGER IF NOT EXISTS bb_library_thumbnail_retired AFTER UPDATE OF thumbnail_path ON library_files WHEN OLD.thumbnail_path IS NOT NULL AND OLD.thumbnail_path IS NOT NEW.thumbnail_path BEGIN INSERT OR IGNORE INTO bb_reclaim(path) VALUES(OLD.thumbnail_path); END;
CREATE TRIGGER IF NOT EXISTS bb_library_thumbnail_removed AFTER DELETE ON library_files WHEN OLD.thumbnail_path IS NOT NULL BEGIN INSERT OR IGNORE INTO bb_reclaim(path) VALUES(OLD.thumbnail_path); END;
CREATE TABLE IF NOT EXISTS bb_requests(actor INTEGER NOT NULL, request_key TEXT NOT NULL, fingerprint TEXT NOT NULL, result TEXT NOT NULL, PRIMARY KEY(actor,request_key));
CREATE TABLE IF NOT EXISTS bb_attempts(queue_id INTEGER PRIMARY KEY, printer_id INTEGER NOT NULL, name TEXT NOT NULL, phase TEXT NOT NULL, subtask TEXT, run_id INTEGER, updated_at TEXT DEFAULT CURRENT_TIMESTAMP);
CREATE UNIQUE INDEX IF NOT EXISTS bb_active_device ON bb_attempts(printer_id) WHERE phase IN ('uploading','sent','running');
CREATE TABLE IF NOT EXISTS bb_versions(id INTEGER PRIMARY KEY,file_id INTEGER NOT NULL REFERENCES library_files(id) ON DELETE CASCADE, filename TEXT NOT NULL,file_path TEXT NOT NULL,file_type TEXT NOT NULL,file_size INTEGER NOT NULL,file_hash TEXT,file_metadata TEXT,created_by_id INTEGER,created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
CREATE TABLE IF NOT EXISTS bb_photos(kind TEXT NOT NULL,owner_id INTEGER NOT NULL,filename TEXT NOT NULL,path TEXT NOT NULL,PRIMARY KEY(kind,owner_id,filename));
CREATE TABLE IF NOT EXISTS bb_review(queue_id INTEGER PRIMARY KEY, required INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_submission_target(queue_id INTEGER PRIMARY KEY REFERENCES print_queue(id) ON DELETE CASCADE,printer_id INTEGER);
CREATE TABLE IF NOT EXISTS bb_printer_trust(printer_id INTEGER PRIMARY KEY, fingerprint TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bb_claimed_intake(queue_id INTEGER PRIMARY KEY REFERENCES print_queue(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS bb_idle_watch(queue_id INTEGER PRIMARY KEY,first INTEGER NOT NULL,last INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_device_cooldown(printer_id INTEGER PRIMARY KEY,failures INTEGER NOT NULL,until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_queue_history_hidden(queue_id INTEGER PRIMARY KEY REFERENCES print_queue(id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS bb_file_jobs(job_id TEXT PRIMARY KEY,printer_id INTEGER NOT NULL,owner INTEGER NOT NULL,key_id INTEGER NOT NULL,paths TEXT NOT NULL,filename TEXT NOT NULL,as_zip INTEGER NOT NULL,state TEXT NOT NULL,requested INTEGER NOT NULL,successful INTEGER NOT NULL DEFAULT 0,failed INTEGER NOT NULL DEFAULT 0,credential TEXT,expires INTEGER NOT NULL,message TEXT);
CREATE TABLE IF NOT EXISTS bb_file_tokens(hash TEXT PRIMARY KEY,job_id TEXT NOT NULL REFERENCES bb_file_jobs(job_id) ON DELETE CASCADE,expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_dry_progress(dry_id INTEGER PRIMARY KEY REFERENCES scheduled_dryings(id) ON DELETE CASCADE,key_id INTEGER,remaining INTEGER,changed_at INTEGER,observed INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS bb_queue_actor(queue_id INTEGER PRIMARY KEY REFERENCES print_queue(id) ON DELETE CASCADE,key_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_production_events(id INTEGER PRIMARY KEY AUTOINCREMENT,kind TEXT NOT NULL,printer_id INTEGER,queue_item_id INTEGER,run_id INTEGER,archive_id INTEGER,awaiting_plate_clear INTEGER);
CREATE TRIGGER IF NOT EXISTS bb_event_queue_insert AFTER INSERT ON print_queue BEGIN INSERT INTO bb_production_events(kind,printer_id,queue_item_id,archive_id) VALUES('queue_item_added',new.printer_id,new.id,new.archive_id); END;
CREATE TRIGGER IF NOT EXISTS bb_event_run_delete AFTER DELETE ON print_log_entries BEGIN INSERT INTO bb_production_events(kind,printer_id,run_id,archive_id) VALUES('run_discarded',old.printer_id,old.id,old.archive_id); END;
CREATE TRIGGER IF NOT EXISTS bb_event_plate_state AFTER UPDATE OF awaiting_plate_clear ON printers WHEN new.awaiting_plate_clear IS NOT old.awaiting_plate_clear BEGIN INSERT INTO bb_production_events(kind,printer_id,awaiting_plate_clear) VALUES('plate_clear_state',new.id,new.awaiting_plate_clear); END;
CREATE TABLE IF NOT EXISTS bb_keep_warm(printer_id INTEGER PRIMARY KEY,queue_id INTEGER NOT NULL,bed INTEGER NOT NULL,expires INTEGER NOT NULL,released INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS bb_heat_release_wait(printer_id INTEGER NOT NULL,purpose INTEGER NOT NULL,queue_id INTEGER NOT NULL,until_unix INTEGER NOT NULL,PRIMARY KEY(printer_id,purpose));
CREATE TABLE IF NOT EXISTS bb_heater_owners(printer_id INTEGER PRIMARY KEY,queue_id INTEGER NOT NULL,bed INTEGER NOT NULL,chamber INTEGER NOT NULL,expires INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_firmware_cache(model TEXT PRIMARY KEY,version TEXT NOT NULL,until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_firmware_catalog(model TEXT PRIMARY KEY,versions TEXT NOT NULL,until INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_firmware_jobs(printer_id INTEGER PRIMARY KEY REFERENCES printers(id) ON DELETE CASCADE,model TEXT NOT NULL,owner INTEGER NOT NULL,key_id INTEGER NOT NULL,status TEXT NOT NULL,progress INTEGER NOT NULL DEFAULT 0,message TEXT NOT NULL,error TEXT,firmware_filename TEXT NOT NULL,firmware_version TEXT NOT NULL,url TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bb_run_sequence(singleton INTEGER PRIMARY KEY,last_id INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS bb_captures(run_id INTEGER PRIMARY KEY,archive_id INTEGER NOT NULL,printer_id INTEGER NOT NULL,remote_name TEXT NOT NULL,tries INTEGER NOT NULL DEFAULT 0,due INTEGER NOT NULL DEFAULT 0,last_error TEXT);
CREATE TABLE IF NOT EXISTS bb_external_run(printer_id INTEGER PRIMARY KEY, run_id INTEGER NOT NULL, name TEXT NOT NULL, subtask TEXT);
`)
	if err != nil {
		return nil, err
	}
	farm := &Service{db: db, dir: dir, devices: map[int64]*liveDevice{}, jobStops: map[string]context.CancelFunc{}, wake: make(chan struct{}, 1), open: openLAN, probePort: defaultProbe}
	if err = farm.recoverExternal(); err != nil {
		return nil, err
	}
	return farm, farm.reclaimLibrary(context.Background())
}
func (s *Service) SetEventSink(f func(map[string]any)) { s.mu.Lock(); s.sink = f; s.mu.Unlock() }
func (s *Service) SetUserResolver(f func(context.Context, int64) (access.Principal, error)) {
	s.mu.Lock()
	s.resolve = f
	s.mu.Unlock()
}
func (s *Service) emit(v row) {
	s.mu.RLock()
	f := s.sink
	s.mu.RUnlock()
	if f != nil {
		f(v)
	}
}
func (s *Service) kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

type failure struct {
	code    int
	message string
}

func (e failure) Error() string  { return e.message }
func fail(c int, m string) error { return failure{c, m} }

type endpoint func(http.ResponseWriter, *http.Request) (any, error)

func wrap(fn endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, e := fn(w, r)
		if e != nil {
			var h failure
			code := 500
			msg := "Internal server error"
			if errors.As(e, &h) {
				code = h.code
				msg = h.message
			} else if errors.Is(e, sql.ErrNoRows) {
				code = 404
				msg = "Not found"
			} else {
				log.Printf("production request %s %s: %v", r.Method, r.URL.Path, e)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(row{"detail": msg})
			return
		}
		if v != nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(v)
		}
	}
}
func input(w http.ResponseWriter, r *http.Request) (row, error) {
	var v row
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	d.UseNumber()
	if err := d.Decode(&v); err != nil || v == nil {
		return nil, fail(422, "Expected a JSON object")
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return nil, fail(422, "Unexpected trailing JSON")
	}
	return v, nil
}
func records(ctx context.Context, d database, q string, args ...any) ([]row, error) {
	rs, e := d.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	defer rs.Close()
	cols, e := rs.ColumnTypes()
	if e != nil {
		return nil, e
	}
	out := []row{}
	for rs.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if e = rs.Scan(ptrs...); e != nil {
			return nil, e
		}
		m := row{}
		for i, k := range cols {
			v := vals[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if t, ok := v.(time.Time); ok {
				v = t.UTC().Format(time.RFC3339)
			}
			if v != nil && strings.EqualFold(k.DatabaseTypeName(), "BOOLEAN") {
				v = flag(v)
			}
			m[k.Name()] = v
		}
		out = append(out, m)
	}
	return out, rs.Err()
}
func one(ctx context.Context, d database, q string, args ...any) (row, error) {
	rows, e := records(ctx, d, q, args...)
	if e != nil {
		return nil, e
	}
	if len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	return rows[0], nil
}
func insert(ctx context.Context, d database, table string, m row) (int64, error) {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	args := make([]any, len(keys))
	for i, k := range keys {
		args[i] = m[k]
	}
	qs := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	res, e := d.ExecContext(ctx, "INSERT INTO "+table+" ("+strings.Join(keys, ",")+") VALUES ("+qs+")", args...)
	if e != nil {
		return 0, e
	}
	return res.LastInsertId()
}
func num(x any) int64 {
	switch v := x.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	case float64:
		return int64(v)
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	}
	return 0
}
func val(x any) string {
	if x == nil {
		return ""
	}
	return fmt.Sprint(x)
}
func flag(x any) bool { return x == true || val(x) == "1" || val(x) == "true" }
func id(x int64) any {
	if x == 0 {
		return nil
	}
	return x
}
func pathID(r *http.Request) int64 { return num(r.PathValue("id")) }
func jstr(v any) string            { b, _ := json.Marshal(v); return string(b) }
func unpack(v any) any {
	var x any
	if e := json.Unmarshal([]byte(val(v)), &x); e != nil {
		return nil
	}
	return x
}
func uuid() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func permitted(r *http.Request, perm string) error {
	if !access.From(r.Context()).Has(perm) {
		return fail(403, "Permission denied")
	}
	return nil
}
func owned(p access.Principal, resource, action string, owner int64) bool {
	return p.Has(resource+":"+action+"_all") || (owner > 0 && owner == p.ID && p.Has(resource+":"+action+"_own")) || p.Admin
}
func scope(p access.Principal, printer int64) bool { return printer == 0 || p.AllowsPrinter(printer) }
func sourceTable(kind string) string {
	if kind == "library" {
		return "library_files"
	}
	return "print_archives"
}
func sourceResource(kind string) string {
	if kind == "library" {
		return "library"
	}
	return "archives"
}

// Asset paths are relative to the application data root.
func (s *Service) assetPath(v any) (string, error) {
	p := val(v)
	if !filepath.IsLocal(p) {
		return "", fail(404, "Asset unavailable")
	}
	return filepath.Join(s.dir, p), nil
}
func (s *Service) Register(m *http.ServeMux) {
	route := func(method, path string, f endpoint) { m.HandleFunc(method+" /api/v1"+path, wrap(f)) }
	for _, p := range []string{"/printers", "/printers/{$}"} {
		route("GET", p, s.printers)
		route("POST", p, s.addPrinter)
	}
	route("GET", "/printers/{id}", s.readPrinter)
	route("PATCH", "/printers/{id}", s.editPrinter)
	route("DELETE", "/printers/{id}", s.deletePrinter)
	route("GET", "/printers/{id}/status", s.status)
	route("POST", "/printers/{id}/clear-plate", s.clearPlate)
	for _, action := range []string{"pause", "resume", "stop"} {
		route("POST", "/printers/{id}/print/"+action, func(w http.ResponseWriter, r *http.Request) (any, error) {
			r.SetPathValue("action", action)
			return s.control(w, r)
		})
	}
	route("GET", "/bulk/printer-statuses", s.bulkStatus)
	s.firmwareRoutes(route)
	route("GET", "/firmware/updates", s.firmware)
	route("GET", "/firmware/updates/{id}", s.firmware)
	s.automationRoutes(route)
	s.operatorRoutes(route)
	s.calibrationRoutes(route)
	s.storageRoutes(route)
	s.dryRoutes(route)
	s.slotRoutes(route)
	route("GET", "/printers/{id}/inventory-remain", s.stockView)
	route("GET", "/printers/available-filaments", s.fleetMaterials("materials"))
	route("GET", "/printers/developer-mode-warnings", s.fleetMaterials("warnings"))
	s.jobRoutes(route)
	s.assetRoutes(route)
	s.archiveAdminRoutes(route)
	s.archiveRoutes(route)
	s.revisionRoutes(route)
	s.libraryImportRoutes(route)
	s.libraryAdminRoutes(route)
	s.libraryRoutes(route)
	s.modelRoutes(route)
	s.variantRoutes(route)
	s.queueRoutes(route)
	s.historyRoutes(route)
}
