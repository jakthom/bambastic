package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Bootstrap initializes a generation in one transaction. The reference DDL lists
// tables alphabetically, so cross-table foreign keys are installed after tables.
func Bootstrap(ctx context.Context, db *sql.DB, ddl string) error {
	conn, e := db.Conn(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	return conn.Raw(func(v any) error {
		c, ok := v.(*connection)
		if !ok {
			return errors.New("Bootstrap requires postgres.Open")
		}
		tx, e := c.pg.Begin(ctx)
		if e != nil {
			return e
		}
		defer tx.Rollback(context.Background())
		name := c.owner.schema
		if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended(current_database()||$1, 72651))", name); e != nil {
			return e
		}
		var exists bool
		if e = tx.QueryRow(ctx, "SELECT to_regnamespace($1) IS NOT NULL", name).Scan(&exists); e != nil {
			return e
		}
		if !exists {
			if _, e = tx.Exec(ctx, "CREATE SCHEMA "+pgx.Identifier{name}.Sanitize()); e != nil {
				return e
			}
		}
		if _, e = tx.Exec(ctx, helpers); e != nil {
			return fmt.Errorf("database helpers: %w", e)
		}
		if e = tx.QueryRow(ctx, "SELECT to_regclass('printers') IS NOT NULL").Scan(&exists); e != nil {
			return e
		}
		if !exists {
			late := []string{}
			for _, s := range statements(ddl) {
				ds, e := ddlCommands(s, true)
				if e != nil {
					return e
				}
				for _, q := range ds {
					if strings.HasPrefix(q, "ALTER TABLE") || strings.HasPrefix(strings.ToUpper(q), "CREATE INDEX") || strings.HasPrefix(strings.ToUpper(q), "CREATE UNIQUE INDEX") {
						late = append(late, q)
						continue
					}
					if _, e = tx.Exec(ctx, q); e != nil {
						return fmt.Errorf("schema %s: %w", excerpt(q), e)
					}
				}
			}
			for _, q := range late {
				if _, e = tx.Exec(ctx, q); e != nil {
					return fmt.Errorf("constraint %s: %w", excerpt(q), e)
				}
			}
			if _, e = tx.Exec(ctx, `CREATE INDEX bb_archive_text_search ON print_archives USING gin(to_tsvector('simple'::regconfig,coalesce(print_name,'')||' '||coalesce(filename,'')||' '||coalesce(tags,'')||' '||coalesce(notes,'')||' '||coalesce(designer,'')||' '||coalesce(filament_type,'')))`); e != nil {
				return e
			}
		}
		// Ciphertexts and third-party JWTs can exceed the legacy SQLite VARCHAR hints.
		for _, column := range []string{"cloud_token", "orca_cloud_token", "orca_cloud_refresh_token", "orca_cloud_pending_verifier"} {
			var short bool
			if e = tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema=current_schema() AND table_name='users' AND column_name=$1 AND data_type='character varying')", column).Scan(&short); e != nil {
				return e
			}
			if short {
				if _, e = tx.Exec(ctx, "ALTER TABLE users ALTER COLUMN "+pgx.Identifier{column}.Sanitize()+" TYPE TEXT"); e != nil {
					return e
				}
			}
		}
		// Preserve logical release column types for Go callers decoding model rows.
		hints := map[string]bool{}
		for _, q := range statements(ddl) {
			ts := lex(q)
			if len(ts) < 4 || !ts[0].is("create") || !ts[1].is("table") {
				continue
			}
			for _, col := range split(ts[4 : len(ts)-1]) {
				if len(col) > 1 && col[1].is("json") {
					hints[baseColumn(ts[2].text)+"."+baseColumn(col[0].text)] = true
				}
			}
		}
		r, e := tx.Query(ctx, `SELECT c.oid::bigint,a.attnum::integer,c.relname,a.attname FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace JOIN pg_attribute a ON a.attrelid=c.oid WHERE n.nspname=current_schema() AND a.attnum>0 AND NOT a.attisdropped`)
		if e != nil {
			return e
		}
		logical := map[[2]uint32]string{}
		for r.Next() {
			var oid, attr int64
			var tab, col string
			if e = r.Scan(&oid, &attr, &tab, &col); e != nil {
				r.Close()
				return e
			}
			if hints[tab+"."+col] {
				logical[[2]uint32{uint32(oid), uint32(attr)}] = "JSON"
			}
		}
		r.Close()
		if e = r.Err(); e != nil {
			return e
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
		c.owner.mu.Lock()
		c.owner.logical = logical
		c.owner.mu.Unlock()
		return nil
	})
}
func excerpt(q string) string {
	if len(q) > 120 {
		return q[:120]
	}
	return q
}

const helpers = `
CREATE OR REPLACE VIEW bb_sqlite_master AS SELECT r.relname::text name,CASE r.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view' WHEN 'i' THEN 'index' END AS type FROM pg_class r JOIN pg_namespace n ON n.oid=r.relnamespace WHERE n.nspname=current_schema();
CREATE OR REPLACE VIEW bb_sqlite_sequence AS SELECT r.relname::text AS name,pg_sequence_last_value(pg_get_serial_sequence(format('%I.%I',n.nspname,r.relname),a.attname)::regclass) AS seq FROM pg_attribute a JOIN pg_class r ON a.attrelid=r.oid JOIN pg_namespace n ON r.relnamespace=n.oid WHERE n.nspname=current_schema() AND a.attidentity<>'';
CREATE OR REPLACE FUNCTION bb_sync_identity() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE seq regclass; high bigint;
BEGIN
 seq := pg_get_serial_sequence(format('%I.%I',TG_TABLE_SCHEMA,TG_TABLE_NAME),'id')::regclass;
 high := pg_sequence_last_value(seq);
 IF NEW.id > coalesce(high,0) THEN PERFORM setval(seq,NEW.id,true); END IF;
 RETURN NEW;
END $$;
CREATE OR REPLACE FUNCTION bb_timestamp(v text) RETURNS timestamptz LANGUAGE plpgsql STABLE STRICT AS $$ BEGIN RETURN v::timestamptz; EXCEPTION WHEN invalid_datetime_format OR datetime_field_overflow THEN RETURN NULL; END $$;
CREATE OR REPLACE FUNCTION unixepoch() RETURNS bigint LANGUAGE SQL STABLE AS $$ SELECT floor(extract(epoch FROM current_timestamp))::bigint $$;
CREATE OR REPLACE FUNCTION unixepoch(v text, delta text DEFAULT '0 seconds') RETURNS bigint LANGUAGE SQL STABLE STRICT AS $$ SELECT floor(extract(epoch FROM (bb_timestamp(v) + delta::interval)))::bigint $$;
CREATE OR REPLACE FUNCTION unixepoch(v timestamptz, delta text DEFAULT '0 seconds') RETURNS bigint LANGUAGE SQL STABLE STRICT AS $$ SELECT floor(extract(epoch FROM (v + delta::interval)))::bigint $$;
CREATE OR REPLACE FUNCTION datetime(v text, delta text DEFAULT '0 seconds') RETURNS timestamptz LANGUAGE SQL STABLE STRICT AS $$ SELECT bb_timestamp(v)+delta::interval $$;
CREATE OR REPLACE FUNCTION datetime(v timestamptz, delta text DEFAULT '0 seconds') RETURNS timestamptz LANGUAGE SQL STABLE STRICT AS $$ SELECT v+delta::interval $$;
CREATE OR REPLACE FUNCTION julianday(v text, delta text DEFAULT '0 seconds') RETURNS double precision LANGUAGE SQL STABLE STRICT AS $$ SELECT extract(epoch FROM (bb_timestamp(v)+delta::interval))::double precision/86400+2440587.5 $$;
CREATE OR REPLACE FUNCTION julianday(v timestamptz, delta text DEFAULT '0 seconds') RETURNS double precision LANGUAGE SQL STABLE STRICT AS $$ SELECT extract(epoch FROM (v+delta::interval))::double precision/86400+2440587.5 $$;
CREATE OR REPLACE FUNCTION json_valid(v text) RETURNS boolean LANGUAGE SQL IMMUTABLE STRICT AS $$ SELECT pg_input_is_valid(v,'json') $$;
CREATE OR REPLACE FUNCTION json_extract(v text, k text) RETURNS text LANGUAGE SQL IMMUTABLE STRICT AS $$ SELECT v::jsonb #>> string_to_array(substr(k,3),'.') $$;
CREATE OR REPLACE FUNCTION bb_append_json(v jsonb,x anyelement) RETURNS jsonb LANGUAGE SQL IMMUTABLE AS $$ SELECT v||jsonb_build_array(x) $$;
CREATE OR REPLACE FUNCTION bb_json_text(v jsonb) RETURNS text LANGUAGE SQL IMMUTABLE AS $$ SELECT v::text $$;
CREATE OR REPLACE FUNCTION round(v double precision,places integer) RETURNS numeric LANGUAGE SQL IMMUTABLE STRICT AS $$ SELECT pg_catalog.round(v::numeric,places) $$;
CREATE OR REPLACE FUNCTION bb_add_flag(v bigint,x boolean) RETURNS bigint LANGUAGE SQL IMMUTABLE AS $$ SELECT v+coalesce(x::int,0) $$;
CREATE OR REPLACE AGGREGATE bb_json_group_array(anyelement) (SFUNC=bb_append_json,STYPE=jsonb,INITCOND='[]',FINALFUNC=bb_json_text);
CREATE OR REPLACE AGGREGATE sum(boolean) (SFUNC=bb_add_flag,STYPE=bigint,INITCOND='0');
`
