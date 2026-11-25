// Package pgtransfer copies a transactionally consistent Bambastic snapshot between
// an initialized SQLite database and a single PostgreSQL generation. PostgreSQL
// handles are raw pgx stdlib handles, scoped to the generation by search_path.
package pgtransfer

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
)

const Marker = "bb_pg_transfer_state"
const portableSequences = "bb_portable_sequences"

type column struct {
	name, typ                                string
	nullable, defaulted, generated, identity bool
}
type table struct {
	name         string
	cols         []column
	serial       string
	serialColumn string
	auto         bool
}
type catalog struct {
	schema string
	tables map[string]*table
}

func quote(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }
func names(c catalog) []string {
	out := make([]string, 0, len(c.tables))
	for k := range c.tables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (c catalog) name(t string) string { return quote(c.schema) + "." + quote(t) }
func pgCatalog(ctx context.Context, tx *sql.Tx) (catalog, error) {
	c := catalog{tables: map[string]*table{}}
	if e := tx.QueryRowContext(ctx, "SELECT current_schema()").Scan(&c.schema); e != nil {
		return c, e
	}
	if c.schema == "" || strings.HasPrefix(c.schema, "pg_") || c.schema == "information_schema" {
		return c, fmt.Errorf("PostgreSQL connection must select an application schema")
	}
	rs, e := tx.QueryContext(ctx, `SELECT c.table_name,c.column_name,c.udt_name,c.is_nullable='YES',c.column_default IS NOT NULL,c.is_generated<>'NEVER',c.is_identity='YES',pg_get_serial_sequence(format('%I.%I',c.table_schema,c.table_name),c.column_name) FROM information_schema.columns c JOIN information_schema.tables t USING(table_catalog,table_schema,table_name) WHERE c.table_schema=$1 AND t.table_type='BASE TABLE' ORDER BY c.table_name,c.ordinal_position`, c.schema)
	if e != nil {
		return c, e
	}
	defer rs.Close()
	for rs.Next() {
		var n string
		var col column
		var seq sql.NullString
		if e = rs.Scan(&n, &col.name, &col.typ, &col.nullable, &col.defaulted, &col.generated, &col.identity, &seq); e != nil {
			return c, e
		}
		if n == Marker {
			continue
		}
		t := c.tables[n]
		if t == nil {
			t = &table{name: n}
			c.tables[n] = t
		}
		if seq.Valid {
			if t.serial != "" {
				return c, fmt.Errorf("multiple generated sequences in table %q", n)
			}
			t.serial = seq.String
			t.serialColumn = col.name
		}
		t.cols = append(t.cols, col)
	}
	return c, rs.Err()
}
func sqliteCatalog(ctx context.Context, tx *sql.Tx) (catalog, error) {
	c := catalog{"main", map[string]*table{}}
	rs, e := tx.QueryContext(ctx, "SELECT name FROM pragma_table_list WHERE schema='main' AND type='table' AND substr(name,1,7)<>'sqlite_' ORDER BY name")
	if e != nil {
		return c, e
	}
	for rs.Next() {
		var n string
		if e = rs.Scan(&n); e != nil {
			rs.Close()
			return c, e
		}
		if n != Marker && n != portableSequences {
			c.tables[n] = &table{name: n}
		}
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return c, e
	}
	for _, n := range names(c) {
		t := c.tables[n]
		var ddl string
		if e = tx.QueryRowContext(ctx, "SELECT sql FROM main.sqlite_schema WHERE type='table' AND name=?", n).Scan(&ddl); e != nil {
			return c, e
		}
		t.auto = strings.Contains(strings.ToUpper(ddl), "AUTOINCREMENT")
		rs, e = tx.QueryContext(ctx, `SELECT name,type,"notnull",dflt_value IS NOT NULL,pk,hidden FROM pragma_table_xinfo(?) ORDER BY cid`, n)
		if e != nil {
			return c, e
		}
		for rs.Next() {
			var col column
			var notnull bool
			var pk, hidden int
			if e = rs.Scan(&col.name, &col.typ, &notnull, &col.defaulted, &pk, &hidden); e != nil {
				rs.Close()
				return c, e
			}
			col.nullable = !notnull
			col.generated = hidden != 0
			col.typ = strings.ToUpper(col.typ)
			if pk == 1 && col.typ == "INTEGER" {
				col.identity = true
				t.serialColumn = col.name
			}
			t.cols = append(t.cols, col)
		}
		e = rs.Err()
		rs.Close()
		if e != nil {
			return c, e
		}
	}
	return c, nil
}

type copyPlan struct {
	from, to       *table
	source, target []column
}

func plans(ctx context.Context, src *sql.Tx, from, to catalog) ([]copyPlan, error) {
	var out []copyPlan
	for _, n := range names(from) {
		s, t := from.tables[n], to.tables[n]
		if t == nil {
			var rows bool
			if e := src.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+from.name(n)+")").Scan(&rows); e != nil {
				return nil, e
			}
			if rows {
				return nil, fmt.Errorf("destination is missing nonempty table %q", n)
			}
			continue
		}
		m := map[string]column{}
		for _, c := range t.cols {
			m[c.name] = c
		}
		p := copyPlan{from: s, to: t}
		for _, c := range s.cols {
			if c.generated {
				continue
			}
			d, ok := m[c.name]
			if !ok {
				return nil, fmt.Errorf("destination is missing column %s.%s", n, c.name)
			}
			delete(m, c.name)
			if !d.generated {
				p.source = append(p.source, c)
				p.target = append(p.target, d)
			}
		}
		for _, c := range m {
			if !c.nullable && !c.defaulted && !c.generated && !c.identity {
				return nil, fmt.Errorf("source lacks required column %s.%s", n, c.name)
			}
		}
		if len(p.source) == 0 {
			return nil, fmt.Errorf("no portable columns on %q", n)
		}
		out = append(out, p)
	}
	return out, nil
}
