// Package operations connects printer lifecycle events to configured power and notification devices.
package operations

import (
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
)

type obj = map[string]any
type Service struct {
	photoDir       string
	capture        photoCapture
	EventSink      func(map[string]any)
	InventoryRelay interface {
		ConnectionStatus() (enabled, connected bool)
		TryPublish(map[string]any) bool
	}
	MQTTStatus    func(context.Context, int64) (map[string]any, error)
	plugDiscovery plugDiscovery
	AccountMail   func(context.Context, string, string, string) error
	ReadSecret    func(context.Context, string) (string, error)
	db            *sql.DB
	wake          chan struct{}
	mu            sync.RWMutex
	telemetry     map[int64]reading
}

func New(db *sql.DB) (*Service, error) {
	_, e := db.Exec(`CREATE TABLE IF NOT EXISTS bambastic_account_mail(run_id INTEGER NOT NULL REFERENCES print_log_entries(id) ON DELETE CASCADE,event_type TEXT NOT NULL,title TEXT NOT NULL,message TEXT NOT NULL,state TEXT NOT NULL,PRIMARY KEY(run_id,event_type));
 CREATE TABLE IF NOT EXISTS bambastic_notification_delivery(
 id INTEGER PRIMARY KEY,provider_id INTEGER NOT NULL REFERENCES notification_providers(id) ON DELETE CASCADE,event_type TEXT NOT NULL,printer_id INTEGER,run_id INTEGER,title TEXT NOT NULL,message TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending', created_at DATETIME DEFAULT CURRENT_TIMESTAMP);
 CREATE TABLE IF NOT EXISTS bambastic_notification_photos(delivery_id INTEGER PRIMARY KEY REFERENCES bambastic_notification_delivery(id) ON DELETE CASCADE,name TEXT NOT NULL);
 CREATE UNIQUE INDEX IF NOT EXISTS bambastic_notification_run ON bambastic_notification_delivery(provider_id,event_type,run_id) WHERE run_id IS NOT NULL;
 CREATE TABLE IF NOT EXISTS bambastic_power_actions(id INTEGER PRIMARY KEY,plug_id INTEGER NOT NULL REFERENCES smart_plugs(id) ON DELETE CASCADE,run_id INTEGER NOT NULL,action TEXT NOT NULL,state TEXT NOT NULL DEFAULT 'pending', UNIQUE(plug_id,run_id,action));
 CREATE TABLE IF NOT EXISTS bambastic_digest_days(provider_id INTEGER PRIMARY KEY REFERENCES notification_providers(id) ON DELETE CASCADE,day TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_app_message_rate(sender TEXT NOT NULL,created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP);
 CREATE TABLE IF NOT EXISTS bambastic_energy_samples(run_id INTEGER NOT NULL REFERENCES print_log_entries(id) ON DELETE CASCADE,boundary TEXT NOT NULL,queued_at DATETIME DEFAULT CURRENT_TIMESTAMP,state TEXT NOT NULL DEFAULT 'pending',PRIMARY KEY(run_id,boundary));`)
	if e != nil {
		return nil, e
	}
	if e = initTemplates(db); e != nil {
		return nil, e
	}
	return &Service{db: db, wake: make(chan struct{}, 1), telemetry: map[int64]reading{}}, nil
}
func (s *Service) Register(m *http.ServeMux) {
	s.plugs(m)
	s.notifications(m)
	s.sensors(m)
	s.wled(m)
	s.discoverPlugs(m)
}

type fault struct {
	n int
	s string
}

func (e fault) Error() string    { return e.s }
func fail(n int, s string) error { return fault{n, s} }

type action func(*http.Request) (any, error)

func (s *Service) route(m *http.ServeMux, method, path, permission string, h action) {
	m.HandleFunc(method+" /api/v1/"+path, func(w http.ResponseWriter, r *http.Request) {
		actor := access.From(r.Context())
		needed := permission
		if permission == "notifications:send" && actor.KeyID == 0 {
			needed = "notifications:update"
		}
		if !actor.Has(needed) {
			httpx.Error(w, 403, "Permission denied")
			return
		}
		v, e := h(r)
		if e != nil {
			var f fault
			switch {
			case errors.As(e, &f):
				httpx.Error(w, f.n, f.s)
			case errors.Is(e, sql.ErrNoRows):
				httpx.Error(w, 404, "Record not found")
			case strings.Contains(e.Error(), "constraint failed"):
				httpx.Error(w, 409, "Record conflicts with an existing reference")
			default:
				log.Printf("operations: database operation failed (%T)", e)
				httpx.Error(w, 500, "Database operation failed")
			}
			return
		}
		httpx.JSON(w, 200, v)
	})
}
func num(v any) float64 { n, _ := strconv.ParseFloat(fmt.Sprint(v), 64); return n }
func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func flag(v any) bool          { return v == true || str(v) == "1" || str(v) == "true" }
func id(r *http.Request) int64 { n, _ := strconv.ParseInt(r.PathValue("id"), 10, 64); return n }
func permitted(r *http.Request, p any) bool {
	return p == nil || access.From(r.Context()).AllowsPrinter(int64(num(p)))
}
func (s *Service) list(ctx context.Context, q string, params ...any) ([]obj, error) {
	rs, e := s.db.QueryContext(ctx, q, params...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(rs)
}
func (s *Service) get(ctx context.Context, t string, key any) (obj, error) {
	v, e := s.list(ctx, "SELECT * FROM "+t+" WHERE id=?", key)
	if e != nil {
		return nil, e
	}
	if len(v) == 0 {
		return nil, sql.ErrNoRows
	}
	return v[0], nil
}
func (s *Service) scoped(r *http.Request, t string) (obj, error) {
	v, e := s.get(r.Context(), t, id(r))
	if e == nil && !permitted(r, v["printer_id"]) {
		return nil, sql.ErrNoRows
	}
	return v, e
}
func save(ctx context.Context, db interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, table string, id int64, fields obj) (int64, error) {
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var args []any
	for _, k := range keys {
		v := fields[k]
		switch v.(type) {
		case map[string]any, []any:
			b, e := json.Marshal(v)
			if e != nil {
				return 0, e
			}
			v = string(b)
		}
		args = append(args, v)
	}
	if id == 0 {
		r, e := db.ExecContext(ctx, "INSERT INTO "+table+"("+strings.Join(keys, ",")+") VALUES("+strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")+")", args...)
		if e != nil {
			return 0, e
		}
		return r.LastInsertId()
	}
	if len(keys) == 0 {
		return id, nil
	}
	_, e := db.ExecContext(ctx, "UPDATE "+table+" SET "+strings.Join(keys, "=?,")+"=? WHERE id=?", append(args, id)...)
	return id, e
}

//go:embed inputs.json
var definitions []byte

type field struct {
	Type                                                 string
	Default                                              any
	Enum                                                 []any
	AnyOf                                                []field
	Minimum, Maximum, ExclusiveMinimum, ExclusiveMaximum *float64
	MinLength, MaxLength                                 *int
}
type model struct {
	Required   []string
	Properties map[string]field
}

var schemas = func() map[string]model {
	m := map[string]model{}
	if e := json.Unmarshal(definitions, &m); e != nil {
		panic(e)
	}
	return m
}()

func valid(f field, x any) bool {
	if len(f.AnyOf) > 0 {
		for _, a := range f.AnyOf {
			if valid(a, x) {
				return true
			}
		}
		return false
	}
	if len(f.Enum) > 0 {
		for _, v := range f.Enum {
			if str(v) == str(x) {
				return true
			}
		}
		return false
	}
	switch f.Type {
	case "null":
		return x == nil
	case "boolean":
		_, ok := x.(bool)
		return ok
	case "object":
		_, ok := x.(map[string]any)
		return ok
	case "array":
		_, ok := x.([]any)
		return ok
	case "string":
		s, ok := x.(string)
		n := utf8.RuneCountInString(s)
		return ok && (f.MinLength == nil || n >= *f.MinLength) && (f.MaxLength == nil || n <= *f.MaxLength)
	case "number", "integer":
		n, ok := x.(float64)
		return ok && (f.Type != "integer" || math.Trunc(n) == n) && (f.Minimum == nil || n >= *f.Minimum) && (f.Maximum == nil || n <= *f.Maximum) && (f.ExclusiveMinimum == nil || n > *f.ExclusiveMinimum) && (f.ExclusiveMaximum == nil || n < *f.ExclusiveMaximum)
	}
	return true
}
func input(r *http.Request, name string) (obj, error) {
	var d obj
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<20))
	if e := dec.Decode(&d); e != nil || d == nil {
		return nil, fail(422, "Expected a JSON object")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, fail(422, "Invalid JSON")
	}
	m := schemas[name]
	for _, k := range m.Required {
		if d[k] == nil {
			return nil, fail(422, k+" is required")
		}
	}
	out := obj{}
	for k, f := range m.Properties {
		v, ok := d[k]
		if !ok {
			if f.Default != nil && !strings.HasSuffix(name, "Update") {
				out[k] = f.Default
			}
			continue
		}
		if !valid(f, v) {
			return nil, fail(422, "Invalid "+k)
		}
		out[k] = v
	}
	return out, nil
}
func setting(ctx context.Context, db *sql.DB, k string) string {
	v, _ := access.Setting(ctx, db, k, "")
	return v
}
func splitURL(raw string) (*url.URL, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fail(422, "Invalid service URL")
	}
	return u, nil
}
func send(ctx context.Context, method, endpoint string, headers map[string]string, body []byte, out any) error {
	if _, e := splitURL(endpoint); e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if e != nil {
		return fail(422, "Invalid service request")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, e := lanhttp.Client.Do(req)
	if e != nil {
		return fail(503, "Device or service is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fail(502, fmt.Sprintf("Service returned HTTP %d", resp.StatusCode))
	}
	if out != nil && resp.StatusCode != 204 {
		if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out) != nil {
			return fail(502, "Service returned invalid JSON")
		}
	}
	return nil
}
func sub(v any) obj {
	if m, ok := v.(map[string]any); ok {
		return m
	}
	var m obj
	if v != nil && reflect.TypeOf(v).Kind() == reflect.Map {
		b, _ := json.Marshal(v)
		if json.Unmarshal(b, &m) == nil {
			return m
		}
	}
	if json.Unmarshal([]byte(str(v)), &m) == nil {
		return m
	}
	return obj{}
}
func objectCopy(v obj) obj {
	out := obj{}
	for k, x := range v {
		out[k] = x
	}
	return out
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }
