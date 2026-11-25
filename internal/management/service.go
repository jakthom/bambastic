// Package management implements inventory, project and ledger services shared by the web and dispatch layers.
package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

type Service struct {
	ConfigureNativeAssignment func(context.Context, int64, string) bool
	SealStationSecret         func(string) string
	OpenStationSecret         func(string) (string, error)
	MaterialSlots             func(int64) (map[string]any, bool)
	colorsURL                 string
	data                      string
	projectFiles              ProjectFiles
	db                        *sql.DB
	emit                      func(map[string]any)
}

func (s *Service) SetEventSink(sink func(map[string]any)) { s.emit = sink }

type row map[string]any
type problem struct {
	code int
	msg  string
}

func (e *problem) Error() string { return e.msg }
func bad(msg string) error       { return &problem{400, msg} }
func invalid(msg string) error   { return &problem{422, msg} }
func notfound() error            { return &problem{404, "Record not found"} }
func denied() error              { return &problem{403, "Permission denied"} }

type request struct {
	*http.Request
	tx             *sql.Tx
	events         *[]row
	remoteEndpoint string // resolved before a background worker performs remote I/O without a DB transaction
}

func (r request) event(v row) { *r.events = append(*r.events, v) }

type download struct {
	mime, filename string
	bytes          []byte
}

type afterCommit func() any

type response struct {
	status int
	data   any
}
type handler func(request) (any, error)

func New(db *sql.DB) (*Service, error) {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS bb_slot_config(assignment_id INTEGER PRIMARY KEY REFERENCES spool_assignment(id) ON DELETE CASCADE, token TEXT NOT NULL, owner INTEGER NOT NULL,key_id INTEGER NOT NULL,serial TEXT NOT NULL,phase TEXT NOT NULL DEFAULT 'pending',expected_type TEXT NOT NULL DEFAULT '',expected_color TEXT NOT NULL DEFAULT '',expected_preset TEXT NOT NULL DEFAULT '', retries INTEGER NOT NULL DEFAULT 0,due INTEGER NOT NULL DEFAULT 0)`); err != nil {
		return nil, err
	}
	if err := initializeStation(db); err != nil {
		return nil, err
	}
	if err := initializeMaterialRuns(db); err != nil {
		return nil, err
	}
	if err := initializeCatalogs(db); err != nil {
		return nil, err
	}
	return &Service{db: db}, nil
}
func (s *Service) Register(m *http.ServeMux) {
	s.inventory(m)
	s.inventoryCSV(m)
	s.materialRoutes(m)
	s.labelRoutes(m)
	s.projects(m)
	s.finance(m)
	s.kiosk(m)
	s.spoolman(m)
	s.remoteFlows(m)
}
func (s *Service) route(m *http.ServeMux, method, path, perm string, h handler) {
	m.HandleFunc(method+" /api/v1/"+path, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		actor := access.From(r.Context())
		allowed := actor.Admin || perm == ""
		for _, p := range strings.Fields(perm) {
			if actor.Has(p) {
				allowed = true
			}
		}
		if !allowed {
			writeError(w, denied())
			return
		}
		tx, err := s.db.BeginTx(r.Context(), nil)
		if err != nil {
			writeError(w, err)
			return
		}
		defer tx.Rollback()
		events := []row{}
		v, err := h(request{Request: r, tx: tx, events: &events})
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			writeError(w, err)
			return
		}
		if committed, ok := v.(afterCommit); ok {
			v = committed()
		}
		if s.emit != nil {
			if method != "GET" && strings.HasPrefix(path, "inventory/") {
				s.emit(row{"type": "inventory_changed"})
			}
			for _, event := range events {
				s.emit(event)
			}
		}
		if file, ok := v.(download); ok {
			w.Header().Set("Content-Type", file.mime)
			if file.filename != "" {
				w.Header().Set("Content-Disposition", "attachment; filename=\""+file.filename+"\"")
			}
			_, _ = w.Write(file.bytes)
			return
		}
		if withStatus, ok := v.(response); ok {
			w.WriteHeader(withStatus.status)
			v = withStatus.data
		}
		json.NewEncoder(w).Encode(v)
	})
}
func writeError(w http.ResponseWriter, err error) {
	code, msg := 500, "Database operation failed"
	var e *problem
	var conflict *tagConflict
	switch {
	case errors.As(err, &conflict):
		w.WriteHeader(409)
		json.NewEncoder(w).Encode(row{"detail": row{"code": "tag_already_linked", "field": conflict.field, "spool_id": conflict.holder, "message": conflict.Error()}})
		return
	case errors.As(err, &e):
		code, msg = e.code, e.msg
	case errors.Is(err, sql.ErrNoRows):
		code, msg = 404, "Record not found"
	case strings.Contains(err.Error(), "constraint failed"):
		code, msg = 409, "Record conflicts with an existing record or reference"
	default:
		log.Printf("management: %v", err)
	}
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(row{"detail": msg})
}
func (r request) id() int64 { v, _ := strconv.ParseInt(r.PathValue("id"), 10, 64); return v }
func (r request) body() (row, error) {
	var data row
	dec := json.NewDecoder(io.LimitReader(r.Body, 4<<20))
	if err := dec.Decode(&data); err != nil || data == nil {
		return nil, invalid("Expected a JSON object")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return nil, invalid("Invalid JSON")
	}
	return data, nil
}
func num(x any) float64 {
	switch v := x.(type) {
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case float64:
		return v
	case string:
		n, _ := strconv.ParseFloat(v, 64)
		return n
	case bool:
		if v {
			return 1
		}
	}
	return 0
}
func str(x any) string {
	if x == nil {
		return ""
	}
	return fmt.Sprint(x)
}
func truth(x any) bool { return x == true || num(x) != 0 || x == "true" }
func now() string      { return time.Now().UTC().Format(time.RFC3339Nano) }
func obj(fields row, allowed string) row {
	x := row{}
	for _, k := range strings.Fields(allowed) {
		if v, ok := fields[k]; ok {
			x[k] = v
		}
	}
	return x
}
func defaultTo(r row, k string, v any) {
	if _, ok := r[k]; !ok {
		r[k] = v
	}
}
func sortedKeys(r row) []string {
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
func placeholders(n int) string { return strings.TrimRight(strings.Repeat("?,", n), ",") }
func args(r row, keys []string) []any {
	a := make([]any, len(keys))
	for i, k := range keys {
		v := r[k]
		switch v.(type) {
		case []any, map[string]any, row:
			raw, _ := json.Marshal(v)
			v = string(raw)
		}
		a[i] = v
	}
	return a
}
func insert(tx *sql.Tx, table string, r row) (int64, error) {
	keys := sortedKeys(r)
	if len(keys) == 0 {
		return 0, invalid("No fields supplied")
	}
	v, err := tx.Exec("INSERT INTO "+table+" ("+strings.Join(keys, ",")+") VALUES ("+placeholders(len(keys))+")", args(r, keys)...)
	if err != nil {
		return 0, err
	}
	return v.LastInsertId()
}
func update(tx *sql.Tx, table string, id int64, r row) error {
	if len(r) == 0 {
		_, e := one(tx, "SELECT id FROM "+table+" WHERE id=?", id)
		return e
	}
	keys := sortedKeys(r)
	a := args(r, keys)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = k + "=?"
	}
	return affect(tx, "UPDATE "+table+" SET "+strings.Join(vals, ",")+" WHERE id=?", append(a, id)...)
}
func affect(tx *sql.Tx, q string, params ...any) error {
	res, e := tx.Exec(q, params...)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if n == 0 && e == nil {
		return notfound()
	}
	return e
}
func list(tx *sql.Tx, q string, params ...any) ([]row, error) {
	rs, e := tx.Query(q, params...)
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
		v := make([]any, len(cols))
		p := make([]any, len(cols))
		for i := range v {
			p[i] = &v[i]
		}
		if e = rs.Scan(p...); e != nil {
			return nil, e
		}
		item := row{}
		for i, c := range cols {
			val := v[i]
			if val != nil {
				switch x := val.(type) {
				case []byte:
					val = string(x)
				case time.Time:
					val = x.UTC().Format(time.RFC3339Nano)
				}
				switch strings.ToUpper(c.DatabaseTypeName()) {
				case "BOOLEAN":
					val = truth(val)
				case "JSON":
					if st, ok := val.(string); ok {
						var decoded any
						if json.Unmarshal([]byte(st), &decoded) == nil {
							val = decoded
						}
					}
				}
			}
			item[c.Name()] = val
		}
		out = append(out, item)
	}
	return out, rs.Err()
}
func one(tx *sql.Tx, q string, params ...any) (row, error) {
	v, e := list(tx, q, params...)
	if e != nil {
		return nil, e
	}
	if len(v) == 0 {
		return nil, sql.ErrNoRows
	}
	return v[0], nil
}
func get(tx *sql.Tx, t string, id int64) (row, error) {
	return one(tx, "SELECT * FROM "+t+" WHERE id=?", id)
}
func count(tx *sql.Tx, query string, args ...any) (int, error) {
	var n int
	e := tx.QueryRow(query, args...).Scan(&n)
	return n, e
}
func intID(x any) int64 {
	v := num(x)
	n := int64(v)
	if n <= 0 || float64(n) != v {
		return 0
	}
	return n
}
func nullID(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func setting(ctx context.Context, tx *sql.Tx, key, def string) string {
	var v string
	if tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v) == nil {
		return v
	}
	return def
}
