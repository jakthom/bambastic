// Package postgres provides the native PostgreSQL storage backend for Bambastic.
package postgres

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
)

var validSchema = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

func config(dsn, schema string) (*pgx.ConnConfig, error) {
	if !validSchema.MatchString(schema) || strings.HasPrefix(schema, "pg_") || schema == "information_schema" {
		return nil, errors.New("invalid PostgreSQL schema")
	}
	cfg, e := pgx.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("invalid PostgreSQL connection settings")
	}
	cfg.RuntimeParams["search_path"] = pgx.Identifier{schema}.Sanitize() + ", pg_catalog"
	cfg.RuntimeParams["TimeZone"] = "UTC"
	return cfg, nil
}

// Raw opens an unadapted pgx stdlib pool for administrative export and import.
func Raw(ctx context.Context, dsn, schema string) (*sql.DB, error) {
	cfg, e := config(dsn, schema)
	if e != nil {
		return nil, e
	}
	db := stdlib.OpenDB(*cfg)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}
func Open(ctx context.Context, dsn, schema string) (*sql.DB, error) {
	cfg, e := config(dsn, schema)
	if e != nil {
		return nil, e
	}
	cfg.DefaultQueryExecMode = pgx.QueryExecModeSimpleProtocol
	c := &connector{cfg: cfg, schema: schema}
	db := sql.OpenDB(c)
	db.SetMaxOpenConns(12)
	if e = db.PingContext(ctx); e != nil {
		db.Close()
		return nil, e
	}
	return db, nil
}

type Lease struct {
	c      *pgx.Conn
	done   chan struct{}
	cancel context.CancelFunc
}

func Acquire(ctx context.Context, dsn, schema string) (*Lease, error) {
	cfg, e := config(dsn, schema)
	if e != nil {
		return nil, e
	}
	c, e := pgx.ConnectConfig(ctx, cfg)
	if e != nil {
		return nil, e
	}
	var ok bool
	e = c.QueryRow(ctx, "SELECT pg_try_advisory_lock(hashtextextended(current_database()||$1, 44171))", schema).Scan(&ok)
	if e != nil || !ok {
		c.Close(ctx)
		if e == nil {
			e = errors.New("another Bambastic instance owns the PostgreSQL scheduler lease")
		}
		return nil, e
	}
	life, cancel := context.WithCancel(ctx)
	l := &Lease{c: c, done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(l.done)
		defer c.Close(context.Background())
		timer := time.NewTicker(2 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-life.Done():
				return
			case <-timer.C:
				probe, stop := context.WithTimeout(life, 3*time.Second)
				err := c.Ping(probe)
				stop()
				if err != nil {
					return
				}
			}
		}
	}()
	return l, nil
}
func (l *Lease) Done() <-chan struct{} { return l.done }
func (l *Lease) Close() error          { l.cancel(); <-l.done; return nil }

type connector struct {
	cfg     *pgx.ConnConfig
	schema  string
	mu      sync.Mutex
	tables  map[string]table
	logical map[[2]uint32]string
}
type table struct {
	columns, pk []string
	id          bool
}
type unsupportedDriver struct{}

func (unsupportedDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("use postgres.Open")
}
func (c *connector) Driver() driver.Driver { return unsupportedDriver{} }
func (c *connector) Connect(ctx context.Context) (driver.Conn, error) {
	db, e := pgx.ConnectConfig(ctx, c.cfg.Copy())
	if e != nil {
		return nil, e
	}
	return &connection{pg: db, owner: c}, nil
}

type connection struct {
	pg    *pgx.Conn
	owner *connector
	tx    bool
}

func (c *connection) Close() error                   { return c.pg.Close(context.Background()) }
func (c *connection) Ping(ctx context.Context) error { return c.pg.Ping(ctx) }
func (c *connection) IsValid() bool                  { return !c.pg.IsClosed() }
func (c *connection) ResetSession(ctx context.Context) error {
	if c.pg.IsClosed() {
		return driver.ErrBadConn
	}
	if c.pg.PgConn().TxStatus() != 'I' {
		if _, e := c.pg.Exec(ctx, "ROLLBACK"); e != nil {
			return driver.ErrBadConn
		}
	}
	c.tx = false
	return nil
}
func (c *connection) Prepare(q string) (driver.Stmt, error) { return &stmt{c, q}, nil }
func (c *connection) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}
func (c *connection) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	begin := "BEGIN"
	switch sql.IsolationLevel(opts.Isolation) {
	case sql.LevelDefault, sql.LevelReadCommitted:
	case sql.LevelSerializable:
		begin += " ISOLATION LEVEL SERIALIZABLE"
	case sql.LevelRepeatableRead:
		begin += " ISOLATION LEVEL REPEATABLE READ"
	default:
		return nil, errors.New("unsupported isolation level")
	}
	if opts.ReadOnly {
		begin += " READ ONLY"
	}
	if _, e := c.pg.Exec(ctx, begin); e != nil {
		return nil, e
	}
	if !opts.ReadOnly {
		if _, e := c.pg.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_database()||current_schema(), 72651))"); e != nil {
			c.pg.Exec(context.Background(), "ROLLBACK")
			return nil, e
		}
	}
	c.tx = true
	return &transaction{c}, nil
}

type transaction struct{ c *connection }

func (t *transaction) Commit() error {
	t.c.tx = false
	tag, e := t.c.pg.Exec(context.Background(), "COMMIT")
	if e == nil && tag.String() == "ROLLBACK" {
		return errors.New("transaction rolled back")
	}
	return e
}
func (t *transaction) Rollback() error {
	t.c.tx = false
	_, e := t.c.pg.Exec(context.Background(), "ROLLBACK")
	return e
}

type stmt struct {
	c *connection
	q string
}

func (s *stmt) Close() error  { return nil }
func (s *stmt) NumInput() int { return -1 }
func named(a []driver.Value) []driver.NamedValue {
	v := make([]driver.NamedValue, len(a))
	for i, x := range a {
		v[i] = driver.NamedValue{Ordinal: i + 1, Value: x}
	}
	return v
}
func (s *stmt) Exec(a []driver.Value) (driver.Result, error) {
	return s.c.ExecContext(context.Background(), s.q, named(a))
}
func (s *stmt) Query(a []driver.Value) (driver.Rows, error) {
	return s.c.QueryContext(context.Background(), s.q, named(a))
}
func (s *stmt) ExecContext(ctx context.Context, a []driver.NamedValue) (driver.Result, error) {
	return s.c.ExecContext(ctx, s.q, a)
}
func (s *stmt) QueryContext(ctx context.Context, a []driver.NamedValue) (driver.Rows, error) {
	return s.c.QueryContext(ctx, s.q, a)
}
func args(values []driver.NamedValue) []any {
	res := make([]any, len(values))
	for i, v := range values {
		res[i] = v.Value
		if b, ok := v.Value.(bool); ok {
			if b {
				res[i] = "1"
			} else {
				res[i] = "0"
			}
		}
	}
	return res
}

type result struct {
	id, count int64
	has       bool
}

func (r result) LastInsertId() (int64, error) {
	if !r.has {
		return 0, errors.New("statement has no inserted integer identity")
	}
	return r.id, nil
}
func (r result) RowsAffected() (int64, error) { return r.count, nil }

func (c *connection) metadata(ctx context.Context, t string) (table, error) {
	c.owner.mu.Lock()
	m, ok := c.owner.tables[t]
	c.owner.mu.Unlock()
	if ok {
		return m, nil
	}
	rows, e := c.pg.Query(ctx, `SELECT a.attname,coalesce(a.attnum=ANY(i.indkey),false),a.atttypid IN ('int8'::regtype,'int4'::regtype) FROM pg_attribute a JOIN pg_class r ON a.attrelid=r.oid JOIN pg_namespace n ON n.oid=r.relnamespace LEFT JOIN pg_index i ON i.indrelid=r.oid AND i.indisprimary WHERE n.nspname=current_schema() AND r.relname=$1 AND a.attnum>0 AND NOT a.attisdropped ORDER BY a.attnum`, t)
	if e != nil {
		return m, e
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var pk, numeric bool
		if e = rows.Scan(&name, &pk, &numeric); e != nil {
			return m, e
		}
		m.columns = append(m.columns, name)
		if pk {
			m.pk = append(m.pk, name)
			if name == "id" && numeric {
				m.id = true
			}
		}
	}
	if e = rows.Err(); e != nil {
		return m, e
	}
	if len(m.columns) > 0 {
		c.owner.mu.Lock()
		if c.owner.tables == nil {
			c.owner.tables = map[string]table{}
		}
		c.owner.tables[t] = m
		c.owner.mu.Unlock()
	}
	return m, nil
}

func (c *connection) guard(ctx context.Context, write bool) (func(error) error, error) {
	if c.tx {
		if _, e := c.pg.Exec(ctx, "SAVEPOINT bambastic_stmt"); e != nil {
			return nil, e
		}
		return func(err error) error {
			if err != nil {
				_, _ = c.pg.Exec(context.Background(), "ROLLBACK TO SAVEPOINT bambastic_stmt")
			}
			_, e := c.pg.Exec(context.Background(), "RELEASE SAVEPOINT bambastic_stmt")
			if err != nil {
				return err
			}
			return e
		}, nil
	}
	if write {
		tx, e := c.BeginTx(ctx, driver.TxOptions{})
		if e != nil {
			return nil, e
		}
		return func(err error) error {
			if err != nil {
				tx.Rollback()
				return err
			}
			return tx.Commit()
		}, nil
	}
	return func(err error) error { return err }, nil
}
func (c *connection) ExecContext(ctx context.Context, q string, values []driver.NamedValue) (driver.Result, error) {
	parts := statements(q)
	finish, e := c.guard(ctx, true)
	if e != nil {
		return nil, e
	}
	r := result{}
	for _, part := range parts {
		if ddlStmt(part) {
			var sqls []string
			sqls, e = ddlCommands(part, false)
			if e != nil {
				break
			}
			for _, d := range sqls {
				var tag pgconn.CommandTag
				tag, e = c.pg.Exec(ctx, d)
				r.count += tag.RowsAffected()
				if e != nil {
					break
				}
			}
			if e != nil {
				break
			}
			continue
		}
		var query string
		var identity bool
		query, identity, e = c.query(ctx, part, true)
		if e != nil {
			break
		}
		if identity {
			rows, err := c.pg.Query(ctx, query, args(values)...)
			if err != nil {
				e = err
				break
			}
			for rows.Next() {
				e = rows.Scan(&r.id)
				if e != nil {
					break
				}
				r.has = true
				r.count++
			}
			rows.Close()
			if e == nil {
				e = rows.Err()
			}
		} else {
			var tag pgconn.CommandTag
			tag, e = c.pg.Exec(ctx, query, args(values)...)
			r.count += tag.RowsAffected()
		}
		if e != nil {
			break
		}
	}
	return r, finish(e)
}
func (c *connection) QueryContext(ctx context.Context, q string, values []driver.NamedValue) (driver.Rows, error) {
	ts := lex(q)
	if len(ts) == 0 {
		return nil, errors.New("empty query")
	}
	first := strings.ToLower(ts[0].text)
	write := first != "select" && first != "with" && first != "show"
	if first == "with" {
		for _, t := range ts[1:] {
			if t.is("insert") || t.is("update") || t.is("delete") || t.is("merge") {
				write = true
				break
			}
		}
	}
	finish, e := c.guard(ctx, write)
	if e != nil {
		return nil, e
	}
	query, _, e := c.query(ctx, q, false)
	if e != nil {
		return nil, finish(e)
	}
	r, e := c.pg.Query(ctx, query, args(values)...)
	if e != nil {
		return nil, finish(e)
	}
	c.owner.mu.Lock()
	logical := c.owner.logical
	c.owner.mu.Unlock()
	return &rows{r: r, finish: finish, logical: logical}, nil
}

type rows struct {
	logical map[[2]uint32]string
	r       pgx.Rows
	finish  func(error) error
	done    bool
	err     error
}

func (r *rows) ColumnTypeDatabaseTypeName(index int) string {
	f := r.r.FieldDescriptions()[index]
	if hint := r.logical[[2]uint32{f.TableOID, uint32(f.TableAttributeNumber)}]; hint != "" {
		return hint
	}
	switch f.DataTypeOID {
	case pgtype.BoolOID:
		return "BOOLEAN"
	case pgtype.Int8OID, pgtype.Int4OID, pgtype.Int2OID:
		return "INTEGER"
	case pgtype.NumericOID:
		return "NUMERIC"
	case pgtype.Float4OID, pgtype.Float8OID:
		return "FLOAT"
	case pgtype.TimestamptzOID, pgtype.TimestampOID:
		return "DATETIME"
	case pgtype.ByteaOID:
		return "BLOB"
	case pgtype.JSONOID, pgtype.JSONBOID:
		return "JSON"
	case pgtype.TextOID, pgtype.VarcharOID:
		return "TEXT"
	}
	return ""
}
func (r *rows) Columns() []string {
	fs := r.r.FieldDescriptions()
	n := make([]string, len(fs))
	for i, f := range fs {
		n[i] = f.Name
	}
	return n
}
func (r *rows) Close() error {
	if !r.done {
		r.done = true
		r.r.Close()
		r.err = r.finish(r.r.Err())
	}
	return r.err
}
func (r *rows) Next(dst []driver.Value) error {
	if !r.r.Next() {
		if e := r.Close(); e != nil {
			return e
		}
		return io.EOF
	}
	values, e := r.r.Values()
	if e != nil {
		return e
	}
	for i, v := range values {
		switch n := v.(type) {
		case int16:
			dst[i] = int64(n)
		case int32:
			dst[i] = int64(n)
		case float32:
			dst[i] = float64(n)
		case bool:
			if n {
				dst[i] = int64(1)
			} else {
				dst[i] = int64(0)
			}
		case pgtype.Numeric:
			x, e := n.Value()
			if e != nil {
				return e
			}
			f, e := strconv.ParseFloat(fmt.Sprint(x), 64)
			if e != nil {
				return e
			}
			dst[i] = f
		case time.Time:
			dst[i] = n.UTC()
		default:
			dst[i] = v
		}
	}
	return nil
}
