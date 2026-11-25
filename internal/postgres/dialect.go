package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

type token struct {
	text string
	kind byte
}

func (t token) is(s string) bool { return t.kind == 'w' && strings.EqualFold(t.text, s) }
func word(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c >= '0' && c <= '9'
}
func lex(q string) []token {
	out := []token{}
	for i := 0; i < len(q); {
		j := i + 1
		c := q[i]
		switch {
		case c == '-' && j < len(q) && q[j] == '-':
			for j < len(q) && q[j] != '\n' {
				j++
			}
			i = j
			continue
		case c == '/' && j < len(q) && q[j] == '*':
			n := strings.Index(q[j+1:], "*/")
			if n < 0 {
				i = len(q)
			} else {
				i = j + 1 + n + 2
			}
			continue
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i = j
			continue
		case c == '\'' || c == '"':
			for j < len(q) {
				if q[j] == c {
					j++
					if j >= len(q) || q[j] != c {
						break
					}
				}
				j++
			}
			out = append(out, token{q[i:j], c})
		case word(c):
			for j < len(q) && word(q[j]) {
				j++
			}
			out = append(out, token{q[i:j], 'w'})
		default:
			if j < len(q) && strings.Contains(" <= >= == != <> || :: := << >> ", " "+q[i:j+1]+" ") {
				j++
			}
			out = append(out, token{q[i:j], 's'})
		}
		i = j
	}
	return out
}
func join(ts []token) string {
	var b strings.Builder
	for i, t := range ts {
		if i > 0 && t.text != "." && ts[i-1].text != "." {
			b.WriteByte(' ')
		}
		b.WriteString(t.text)
	}
	return b.String()
}
func statements(q string) []string {
	ts := lex(q)
	var out []string
	start := 0
	trig := false
	for i, t := range ts {
		if i == start+1 && ts[start].is("create") && t.is("trigger") {
			trig = true
		}
		if t.text == ";" && (!trig || i >= 2 && ts[i-1].is("end") && ts[i-2].text == ";") {
			if i > start {
				out = append(out, join(ts[start:i]))
			}
			start = i + 1
			trig = false
		}
	}
	if start < len(ts) {
		out = append(out, join(ts[start:]))
	}
	return out
}
func split(ts []token) [][]token {
	d := 0
	i := 0
	out := [][]token{}
	for j, t := range ts {
		switch t.text {
		case "(":
			d++
		case ")":
			d--
		case ",":
			if d == 0 {
				out = append(out, ts[i:j])
				i = j + 1
			}
		}
	}
	if i < len(ts) {
		out = append(out, ts[i:])
	}
	return out
}
func ddlStmt(q string) bool {
	f := strings.Fields(strings.ToLower(q))
	return len(f) > 0 && (f[0] == "create" || f[0] == "alter" || f[0] == "drop")
}

var numericFlags = map[string]bool{"awaiting_plate_clear": true, "required": true, "as_zip": true, "settled": true, "is_admin": true, "confirmed": true, "force_color_match": true}

func baseColumn(s string) string { return strings.ToLower(strings.Trim(s, "\"")) }
func transform(ts []token) string {
	out := []token{}
	n := 0
	for i, t := range ts {
		switch {
		case t.text == "?":
			n++
			t.text = "$" + strconv.Itoa(n)
			if i > 0 && ts[i-1].is("select") && i+1 < len(ts) && ts[i+1].is("union") || i > 1 && ts[i-2].is("values") && i+2 < len(ts) && ts[i+2].is("union") {
				t.text += "::bigint"
			}
		case t.is("in") && i+1 < len(ts) && ts[i+1].kind == 'w':
			out = append(out, t)
			ts[i+1].text = "(SELECT * FROM " + ts[i+1].text + ")"
			continue
		case t.is("like"):
			t.text = "ILIKE"
		case t.is("rowid"):
			t.text = "ctid"
		case t.is("is") && i+1 < len(ts) && !ts[i+1].is("null") && !ts[i+1].is("true") && !ts[i+1].is("false"):
			if !ts[i+1].is("not") {
				t.text = "IS NOT DISTINCT FROM"
			} else if i+2 < len(ts) && !ts[i+2].is("null") && !ts[i+2].is("true") && !ts[i+2].is("false") {
				t.text = "IS DISTINCT FROM"
				ts[i+1].text = ""
			}
		case t.is("nocase") && i > 0 && ts[i-1].is("collate"):
			out = out[:len(out)-1]
			if len(out) >= 3 && out[len(out)-2].text == "=" {
				end := len(out) - 3
				begin := end
				if out[end].text == ")" {
					depth := 1
					for begin > 0 && depth > 0 {
						begin--
						switch out[begin].text {
						case ")":
							depth++
						case "(":
							depth--
						}
					}
					if begin > 0 && out[begin-1].kind == 'w' {
						begin--
					}
				}
				lhs := join(out[begin : end+1])
				rhs := out[len(out)-1].text
				out = append(out[:begin], token{"lower(" + lhs + ") = lower(" + rhs + ")", 's'})
			}
			continue
		case t.is("json_group_array"):
			t.text = "bb_json_group_array"
		case t.is("json_object"):
			t.text = "json_build_object"
		case t.is("sqlite_sequence"):
			t.text = "bb_sqlite_sequence"
		case t.is("sqlite_master"):
			t.text = "bb_sqlite_master"
		case t.is("instr"):
			t.text = "strpos"
		case (t.is("max") || t.is("min")) && i+1 < len(ts) && ts[i+1].text == "(":
			depth := 0
			for _, x := range ts[i+1:] {
				if x.text == "(" {
					depth++
				} else if x.text == ")" {
					depth--
					if depth == 0 {
						break
					}
				} else if depth == 1 && x.text == "," {
					if t.is("max") {
						t.text = "greatest"
					} else {
						t.text = "least"
					}
					break
				}
			}
		case t.text == "0" || t.text == "1":
			// An untyped literal accepts PostgreSQL's native target type (integer or boolean).
			// Preserve numeric literals in casts, arithmetic, decimal fractions and limits.
			before, after := "", ""
			if i > 0 {
				before = strings.ToLower(ts[i-1].text)
			}
			if i+1 < len(ts) {
				after = ts[i+1].text
			}
			if !slices.Contains([]string{".", "+", "-", "*", "/", "%", "limit", "offset", "select", "then", "else"}, before) && !slices.Contains([]string{".", "+", "-", "*", "/", "%"}, after) {
				t.text = "'" + t.text + "'"
			}
		}
		out = append(out, t)
	}
	return join(out)
}

func (c *connection) query(ctx context.Context, q string, forExec bool) (string, bool, error) {
	ts := lex(q)
	if len(ts) == 0 {
		return "", false, errors.New("empty SQL statement")
	}
	ignore, replace := false, false
	if ts[0].is("insert") && len(ts) > 3 && ts[1].is("or") {
		ignore = ts[2].is("ignore")
		replace = ts[2].is("replace")
		if !ignore && !replace {
			return "", false, errors.New("invalid insert conflict mode")
		}
		ts = append(ts[:1], ts[3:]...)
	}
	if !ts[0].is("insert") {
		return transform(ts), false, nil
	}
	if len(ts) < 3 || !ts[1].is("into") {
		return "", false, errors.New("invalid insert")
	}
	tbl := baseColumn(ts[2].text)
	if tbl == "archive_fts" && strings.Contains(q, "'rebuild'") {
		return "REINDEX INDEX bb_archive_text_search", false, nil
	}
	m, e := c.metadata(ctx, tbl)
	if e != nil {
		return "", false, e
	}
	cols := m.columns
	if len(ts) > 3 && ts[3].text == "(" {
		cols = nil
		for _, x := range ts[4:] {
			if x.text == ")" {
				break
			}
			if x.text != "," {
				cols = append(cols, baseColumn(x.text))
			}
		}
	}
	render := transform(ts)
	if ignore {
		render += " ON CONFLICT DO NOTHING"
	}
	if replace {
		if len(m.pk) == 0 {
			return "", false, fmt.Errorf("missing primary key for %s", tbl)
		}
		set := []string{}
		for _, col := range cols {
			if !slices.Contains(m.pk, col) {
				set = append(set, col+"=excluded."+col)
			}
		}
		render += " ON CONFLICT (" + strings.Join(m.pk, ",") + ") "
		if len(set) == 0 {
			render += "DO NOTHING"
		} else {
			render += "DO UPDATE SET " + strings.Join(set, ",")
		}
	}
	if forExec && m.id {
		for _, t := range ts {
			if t.is("returning") {
				return render, false, nil
			}
		}
		render += " RETURNING id"
		return render, true, nil
	}
	return render, false, nil
}

var createTrigger = regexp.MustCompile(`(?is)^CREATE TRIGGER (?:IF NOT EXISTS )?(\w+) (BEFORE|AFTER) (.*?) ON (\w+)(?: FOR EACH ROW)?(?: WHEN (.*?))? BEGIN (.*) ; END$`)
var typemap = map[string]string{"integer": "BIGINT", "datetime": "TIMESTAMPTZ", "json": "TEXT", "blob": "BYTEA", "float": "DOUBLE PRECISION", "real": "DOUBLE PRECISION", "autoincrement": ""}

func createTable(q string, deferFK bool) (string, []string, error) {
	ts := lex(q)
	open := -1
	for i, t := range ts {
		if t.text == "(" {
			open = i
			break
		}
	}
	if open < 1 || ts[len(ts)-1].text != ")" {
		return "", nil, errors.New("invalid table definition")
	}
	name := ts[open-1].text
	columns := split(ts[open+1 : len(ts)-1])
	parts := []string{}
	pending := []string{}
	idKey := false
	for _, col := range columns {
		if len(col) >= 4 && col[0].is("primary") && col[1].is("key") && col[3].is("id") && len(col) == 5 {
			idKey = true
		}
	}
	for _, col := range columns {
		if len(col) < 2 {
			return "", nil, errors.New("invalid column definition")
		}
		def := join(col)
		if deferFK && (col[0].is("foreign") || len(col) > 2 && col[0].is("constraint") && col[2].is("foreign")) {
			pending = append(pending, "ALTER TABLE "+name+" ADD "+def)
			continue
		}
		if col[0].is("id") && col[1].is("integer") && (idKey || strings.Contains(strings.ToUpper(def), "PRIMARY KEY")) {
			col[1].text = "BIGINT GENERATED BY DEFAULT AS IDENTITY"
		}
		if col[1].is("integer") && numericFlags[strings.ToLower(col[0].text)] {
			col[1].text = "BOOLEAN"
		}
		for i, t := range col {
			if t.kind == 'w' {
				if to, ok := typemap[strings.ToLower(t.text)]; ok {
					col[i].text = to
				}
			}
			if (t.text == "0" || t.text == "1") && i > 0 && col[i-1].is("default") && col[1].is("boolean") {
				col[i].text = map[string]string{"0": "FALSE", "1": "TRUE"}[t.text]
			}
		}
		parts = append(parts, join(col))
	}
	return join(ts[:open]) + " (" + strings.Join(parts, ",") + ")", pending, nil
}

func ddlCommands(q string, deferFK bool) ([]string, error) {
	u := strings.ToUpper(q)
	switch {
	case strings.HasPrefix(u, "CREATE UNIQUE INDEX ") && !strings.Contains(u, "IF NOT EXISTS"):
		return []string{"CREATE UNIQUE INDEX IF NOT EXISTS " + transform(lex(q[len("CREATE UNIQUE INDEX "):]))}, nil
	case strings.HasPrefix(u, "DROP TRIGGER"):
		ts := lex(q)
		name := ts[len(ts)-1].text
		if !validSchema.MatchString(name) {
			return nil, errors.New("invalid trigger name")
		}
		return []string{"DO $drop$ DECLARE tab regclass; BEGIN SELECT t.tgrelid::regclass INTO tab FROM pg_trigger t JOIN pg_class r ON r.oid=t.tgrelid WHERE t.tgname='" + name + "' AND r.relnamespace=current_schema()::regnamespace; IF tab IS NOT NULL THEN EXECUTE format('DROP TRIGGER %I ON %s','" + name + "',tab); END IF; END $drop$"}, nil
	case strings.HasPrefix(u, "CREATE TABLE"):
		s, p, e := createTable(q, deferFK)
		if e != nil {
			return nil, e
		}
		ds := append([]string{s}, p...)
		if strings.Contains(s, "GENERATED BY DEFAULT AS IDENTITY") {
			ts := lex(q)
			for i, t := range ts {
				if t.text == "(" {
					ds = append(ds, "CREATE OR REPLACE TRIGGER bb_sync_identity AFTER INSERT ON "+ts[i-1].text+" FOR EACH ROW EXECUTE FUNCTION bb_sync_identity()")
					break
				}
			}
		}
		return ds, nil
	case strings.HasPrefix(u, "CREATE VIRTUAL TABLE ARCHIVE_FTS"):
		return nil, nil
	case strings.HasPrefix(u, "CREATE TRIGGER"):
		m := createTrigger.FindStringSubmatch(q)
		if len(m) != 7 {
			return nil, fmt.Errorf("invalid trigger definition: %s", q)
		}
		id, when, ev, tab, cond, body := m[1], m[2], m[3], m[4], m[5], m[6]
		if strings.HasPrefix(id, "archive_fts_") {
			return nil, nil
		}
		if id == "trg_filament_sku_settings_updated_at" {
			when = "BEFORE"
			body = "NEW.updated_at := CURRENT_TIMESTAMP"
		}
		body = transform(lex(body))
		b := lex(body)
		if len(b) == 7 && b[0].is("select") && b[1].is("raise") && b[3].is("abort") && b[5].kind == '\'' {
			body = "RAISE EXCEPTION " + b[5].text
		}
		if strings.HasPrefix(strings.ToUpper(body), "INSERT OR IGNORE ") {
			body = "INSERT " + body[len("INSERT OR IGNORE "):] + " ON CONFLICT DO NOTHING"
		}
		if cond != "" {
			cond = " WHEN (" + transform(lex(cond)) + ")"
		}
		ret := "NEW"
		if strings.EqualFold(ev, "delete") {
			ret = "OLD"
		}
		fn := "bb_fn_" + id
		return []string{"CREATE OR REPLACE FUNCTION " + fn + "() RETURNS trigger LANGUAGE plpgsql AS $trig$ BEGIN " + body + "; RETURN " + ret + "; END $trig$", "CREATE OR REPLACE TRIGGER " + id + " " + when + " " + ev + " ON " + tab + " FOR EACH ROW" + cond + " EXECUTE FUNCTION " + fn + "()"}, nil
	default:
		return []string{transform(lex(q))}, nil
	}
}
