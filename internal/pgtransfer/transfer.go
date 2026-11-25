package pgtransfer

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func scanArgs(data []any) []any {
	a := make([]any, len(data))
	for i := range data {
		a[i] = &data[i]
	}
	return a
}
func copyRows(ctx context.Context, src, dst *sql.Tx, from, to catalog, p copyPlan, pg bool) error {
	n := len(p.source)
	cols := make([]string, n)
	for i, v := range p.source {
		cols[i] = quote(v.name)
	}
	rs, e := src.QueryContext(ctx, "SELECT "+strings.Join(cols, ",")+" FROM "+from.name(p.from.name))
	if e != nil {
		return e
	}
	defer rs.Close()
	insert := "INSERT INTO " + to.name(p.to.name) + "(" + strings.Join(cols, ",") + ") "
	if pg && p.to.serial != "" {
		insert += "OVERRIDING SYSTEM VALUE "
	}
	insert += "VALUES "
	// Bound both argument count and binary memory while retaining throughput.
	batch := min(128, 16000/n)
	if batch < 1 {
		return fmt.Errorf("too many columns in %s", p.to.name)
	}
	var args []any
	tuples := []string{}
	size := 0
	v := make([]any, n)
	ptr := scanArgs(v)
	flush := func() error {
		if len(args) == 0 {
			return nil
		}
		_, err := dst.ExecContext(ctx, insert+strings.Join(tuples, ","), args...)
		args = nil
		tuples = nil
		size = 0
		return err
	}
	for rs.Next() {
		if e = rs.Scan(ptr...); e != nil {
			return e
		}
		qs := make([]string, n)
		for i, x := range v {
			a, err := convert(x, p.target[i], pg)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", p.to.name, p.target[i].name, err)
			}
			args = append(args, a)
			if pg {
				qs[i] = fmt.Sprintf("$%d", len(args))
			} else {
				qs[i] = "?"
			}
			switch t := a.(type) {
			case []byte:
				size += len(t)
			case string:
				size += len(t)
			default:
				size += 16
			}
		}
		tuples = append(tuples, "("+strings.Join(qs, ",")+")")
		if len(tuples) >= batch || size >= 4<<20 {
			if e = flush(); e != nil {
				return fmt.Errorf("copy table %s: %w", p.to.name, e)
			}
		}
	}
	if e = rs.Err(); e != nil {
		return e
	}
	if e = flush(); e != nil {
		return fmt.Errorf("copy table %s: %w", p.to.name, e)
	}
	return nil
}
func highwater(ctx context.Context, tx *sql.Tx, t *table, pg bool) (int64, error) {
	var n sql.NullInt64
	var e error
	if pg && t.serial != "" {
		var sequence string
		var increment int64
		e = tx.QueryRowContext(ctx, `SELECT format('%I.%I', n.nspname,c.relname),s.seqincrement FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_sequence s ON s.seqrelid=c.oid WHERE c.oid=$1::regclass`, t.serial).Scan(&sequence, &increment)
		if e != nil {
			return 0, e
		}
		if increment <= 0 {
			return 0, fmt.Errorf("unsupported sequence increment for %s", t.name)
		}
		var called bool
		e = tx.QueryRowContext(ctx, "SELECT last_value,is_called FROM "+sequence).Scan(&n, &called)
		if !called {
			n.Int64 = max(int64(0), n.Int64-increment)
		}
	} else if !pg {
		if t.auto {
			e = tx.QueryRowContext(ctx, "SELECT seq FROM main.sqlite_sequence WHERE name=?", t.name).Scan(&n)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return 0, e
			}
		}
		var saved bool
		if e = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE type='table' AND name=?)", portableSequences).Scan(&saved); e != nil {
			return 0, e
		}
		if saved {
			var floor sql.NullInt64
			e = tx.QueryRowContext(ctx, "SELECT last_value FROM main."+quote(portableSequences)+" WHERE table_name=?", t.name).Scan(&floor)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return 0, e
			}
			return max(n.Int64, floor.Int64), nil
		}
	}
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil
	}
	return n.Int64, e
}
func align(ctx context.Context, src, dst *sql.Tx, to catalog, p copyPlan, pg bool) error {
	if pg && p.to.serial == "" || !pg && (p.from.serial == "" || p.to.serialColumn == "") {
		return nil
	}
	floor, e := highwater(ctx, src, p.from, !pg)
	if e != nil {
		return e
	}
	var n sql.NullInt64
	if e = dst.QueryRowContext(ctx, "SELECT MAX("+quote(p.to.serialColumn)+") FROM "+to.name(p.to.name)).Scan(&n); e != nil {
		return e
	}
	floor = max(floor, n.Int64)
	if pg {
		// ALTER SEQUENCE RESTART is transactional, unlike setval(). A failed
		// validation after copying therefore cannot advance the staging generation.
		var seq, minval, increment string
		e = dst.QueryRowContext(ctx, `SELECT format('%I.%I',n.nspname,c.relname),s.seqmin::text,s.seqincrement::text FROM pg_class c JOIN pg_sequence s ON s.seqrelid=c.oid JOIN pg_namespace n ON n.oid=c.relnamespace WHERE c.oid=$1::regclass`, p.to.serial).Scan(&seq, &minval, &increment)
		if e != nil {
			return e
		}
		var next int64
		e = dst.QueryRowContext(ctx, "SELECT greatest($1::numeric,$2::numeric+$3::numeric)::bigint", minval, floor, increment).Scan(&next)
		if e != nil {
			return e
		}
		_, e = dst.ExecContext(ctx, "ALTER SEQUENCE "+seq+" RESTART WITH "+fmt.Sprint(next))
		return e
	}
	if _, e = dst.ExecContext(ctx, "INSERT INTO main."+quote(portableSequences)+"(table_name,last_value) VALUES(?,?)", p.to.name, floor); e != nil {
		return e
	}
	if !p.to.auto {
		return nil
	}
	if _, e = dst.ExecContext(ctx, "DELETE FROM main.sqlite_sequence WHERE name=?", p.to.name); e != nil {
		return e
	}
	if floor > 0 {
		_, e = dst.ExecContext(ctx, "INSERT INTO main.sqlite_sequence(name,seq) VALUES(?,?)", p.to.name, floor)
	}
	return e
}
func copyTables(ctx context.Context, src, dst *sql.Tx, from, to catalog, pg bool) error {
	ps, e := plans(ctx, src, from, to)
	if e != nil {
		return e
	}
	for _, p := range ps {
		if _, e = dst.ExecContext(ctx, "DELETE FROM "+to.name(p.to.name)); e != nil {
			return fmt.Errorf("clear table %s: %w", p.to.name, e)
		}
	}
	for _, p := range ps {
		if e = copyRows(ctx, src, dst, from, to, p, pg); e != nil {
			return e
		}
		if e = align(ctx, src, dst, to, p, pg); e != nil {
			return fmt.Errorf("restore ID sequence for %s: %w", p.to.name, e)
		}
	}
	return nil
}

// Export reads PostgreSQL under repeatable-read and atomically fills an empty
// initialized SQLite destination. FTS and application change triggers are
// reconstructed, not replayed as new application events.
func Export(ctx context.Context, rawPG, sqlite *sql.DB) error {
	src, e := rawPG.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if e != nil {
		return e
	}
	defer src.Rollback()
	from, e := pgCatalog(ctx, src)
	if e != nil {
		return e
	}
	if len(from.tables) == 0 {
		return errors.New("PostgreSQL source schema is empty")
	}
	conn, e := sqlite.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	var prior int
	if e = conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&prior); e != nil {
		return e
	}
	if _, e = conn.ExecContext(ctx, "PRAGMA foreign_keys=OFF"); e != nil {
		return e
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(c, fmt.Sprintf("PRAGMA foreign_keys=%d", prior))
	}()
	dst, e := conn.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer dst.Rollback()
	to, e := sqliteCatalog(ctx, dst)
	if e != nil {
		return e
	}
	if e = checkDestination(ctx, dst, to); e != nil {
		return e
	}
	rs, e := dst.QueryContext(ctx, "SELECT name,sql FROM main.sqlite_schema WHERE type='trigger' AND sql IS NOT NULL ORDER BY rowid")
	if e != nil {
		return e
	}
	type trigger struct{ name, ddl string }
	var restore []trigger
	for rs.Next() {
		var n, ddl string
		if e = rs.Scan(&n, &ddl); e != nil {
			rs.Close()
			return e
		}
		restore = append(restore, trigger{n, ddl})
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return e
	}
	for _, tr := range restore {
		if _, e = dst.ExecContext(ctx, "DROP TRIGGER main."+quote(tr.name)); e != nil {
			return e
		}
	}
	if _, e = dst.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS main."+quote(portableSequences)+"(table_name TEXT PRIMARY KEY, last_value INTEGER NOT NULL)"); e != nil {
		return e
	}
	if _, e = dst.ExecContext(ctx, "DELETE FROM main."+quote(portableSequences)); e != nil {
		return e
	}
	if e = copyTables(ctx, src, dst, from, to, false); e != nil {
		return e
	}
	var fts bool
	if e = dst.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM main.sqlite_schema WHERE name='archive_fts' AND type='table')").Scan(&fts); e != nil {
		return e
	}
	if fts {
		if _, e = dst.ExecContext(ctx, "INSERT INTO archive_fts(archive_fts) VALUES('rebuild')"); e != nil {
			return fmt.Errorf("rebuild archive search: %w", e)
		}
	}
	for _, tr := range restore {
		if _, e = dst.ExecContext(ctx, tr.ddl); e != nil {
			return e
		}
	}
	rs, e = dst.QueryContext(ctx, "PRAGMA foreign_key_check")
	if e != nil {
		return e
	}
	if rs.Next() {
		var t, parent string
		var row any
		var index int
		_ = rs.Scan(&t, &row, &parent, &index)
		rs.Close()
		return fmt.Errorf("foreign key check failed for %s referencing %s", t, parent)
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return e
	}
	if e = src.Commit(); e != nil {
		return e
	}
	return dst.Commit()
}

// Import validates and copies SQLite into a fresh PG generation. Running an
// import against a populated generation, including a previously imported
// generation, returns an error before replacing any data.
func Import(ctx context.Context, sqlite, rawPG *sql.DB) error {
	src, e := sqlite.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return e
	}
	defer src.Rollback()
	from, e := sqliteCatalog(ctx, src)
	if e != nil {
		return e
	}
	if len(from.tables) == 0 {
		return errors.New("SQLite source has no application tables")
	}
	dst, e := rawPG.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if e != nil {
		return e
	}
	defer dst.Rollback()
	to, e := pgCatalog(ctx, dst)
	if e != nil {
		return e
	}
	if len(to.tables) == 0 {
		return errors.New("PostgreSQL generation has not been initialized")
	}
	marker := to.name(Marker)
	if _, e = dst.ExecContext(ctx, "CREATE TABLE IF NOT EXISTS "+marker+"(one INTEGER PRIMARY KEY CHECK(one=1), imported BOOLEAN NOT NULL)"); e != nil {
		return e
	}
	// Lock in deterministic order so a concurrent initializer cannot pass the
	// empty-generation guard while rows are being replaced.
	targets := []string{marker}
	for _, n := range names(to) {
		targets = append(targets, to.name(n))
	}
	if _, e = dst.ExecContext(ctx, "LOCK TABLE "+strings.Join(targets, ",")+" IN ACCESS EXCLUSIVE MODE"); e != nil {
		return e
	}
	var imported bool
	if e = dst.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+marker+" WHERE imported)").Scan(&imported); e != nil {
		return e
	}
	if imported {
		return errors.New("PostgreSQL generation already contains an imported snapshot")
	}
	if e = checkDestination(ctx, dst, to); e != nil {
		return e
	}
	restore, e := preparePG(ctx, dst, to)
	if e != nil {
		return e
	}
	if e = copyTables(ctx, src, dst, from, to, true); e != nil {
		return e
	}
	if _, e = dst.ExecContext(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); e != nil {
		return fmt.Errorf("restore foreign key validation failed: %w", e)
	}
	for _, stmt := range restore {
		if _, e = dst.ExecContext(ctx, stmt); e != nil {
			return e
		}
	}
	if _, e = dst.ExecContext(ctx, "INSERT INTO "+marker+"(one,imported) VALUES(1,true) ON CONFLICT(one) DO UPDATE SET imported=true"); e != nil {
		return e
	}
	if e = src.Commit(); e != nil {
		return e
	}
	return dst.Commit()
}
func preparePG(ctx context.Context, tx *sql.Tx, c catalog) ([]string, error) {
	// Muting user triggers avoids duplicate outbox and ledger activity. Foreign
	// keys stay installed. Temporarily defer immediate constraints and validate
	// in the same transaction, including self-references and mutual references.
	var crossGeneration bool
	if e := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_constraint k JOIN pg_class a ON a.oid=k.conrelid JOIN pg_class b ON b.oid=k.confrelid JOIN pg_namespace na ON na.oid=a.relnamespace JOIN pg_namespace nb ON nb.oid=b.relnamespace WHERE k.contype='f' AND (na.nspname=$1)<>(nb.nspname=$1))`, c.schema).Scan(&crossGeneration); e != nil {
		return nil, e
	}
	if crossGeneration {
		return nil, fmt.Errorf("foreign key crosses PostgreSQL generation boundary")
	}
	rs, e := tx.QueryContext(ctx, `SELECT r.relname,t.tgname,t.tgenabled::text FROM pg_trigger t JOIN pg_class r ON r.oid=t.tgrelid JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=$1 AND NOT t.tgisinternal AND t.tgenabled<>'D' ORDER BY r.relname,t.tgname`, c.schema)
	if e != nil {
		return nil, e
	}
	var change, restore []string
	for rs.Next() {
		var tbl, n, mode string
		if e = rs.Scan(&tbl, &n, &mode); e != nil {
			rs.Close()
			return nil, e
		}
		prefix := "ALTER TABLE " + c.name(tbl)
		change = append(change, prefix+" DISABLE TRIGGER "+quote(n))
		enable := map[string]string{"O": "ENABLE", "A": "ENABLE ALWAYS", "R": "ENABLE REPLICA"}[mode]
		if enable == "" {
			rs.Close()
			return nil, fmt.Errorf("unknown trigger mode")
		}
		restore = append(restore, prefix+" "+enable+" TRIGGER "+quote(n))
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return nil, e
	}
	rs, e = tx.QueryContext(ctx, `SELECT r.relname,k.conname FROM pg_constraint k JOIN pg_class r ON r.oid=k.conrelid JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=$1 AND k.contype='f' AND NOT k.condeferrable ORDER BY r.relname,k.conname`, c.schema)
	if e != nil {
		return nil, e
	}
	for rs.Next() {
		var t, n string
		if e = rs.Scan(&t, &n); e != nil {
			rs.Close()
			return nil, e
		}
		prefix := "ALTER TABLE " + c.name(t) + " ALTER CONSTRAINT " + quote(n)
		change = append(change, prefix+" DEFERRABLE")
		restore = append(restore, prefix+" NOT DEFERRABLE")
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return nil, e
	}
	for _, stmt := range change {
		if _, e = tx.ExecContext(ctx, stmt); e != nil {
			return nil, e
		}
	}
	_, e = tx.ExecContext(ctx, "SET CONSTRAINTS ALL DEFERRED")
	return restore, e
}
