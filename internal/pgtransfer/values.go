package pgtransfer

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

func timestamp(v any) (time.Time, error) {
	if t, ok := v.(time.Time); ok {
		return t.UTC(), nil
	}
	s := fmt.Sprint(v)
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999-07", "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05.999999999", "2006-01-02"} {
		if t, e := time.Parse(layout, s); e == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unrecognized timestamp")
}
func convert(v any, col column, pg bool) (any, error) {
	if v == nil {
		return nil, nil
	}
	if dv, ok := v.(driver.Valuer); ok {
		value, e := dv.Value()
		if e != nil {
			return nil, e
		}
		v = value
	}
	if !pg {
		switch x := v.(type) {
		case time.Time:
			if col.typ == "DATE" {
				return x.Format("2006-01-02"), nil
			}
			return x.UTC().Format("2006-01-02 15:04:05.999999999"), nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case []byte:
			if !strings.Contains(col.typ, "BLOB") && col.typ != "" {
				if !utf8.Valid(x) {
					return nil, fmt.Errorf("invalid UTF-8 for text column")
				}
				return string(x), nil
			}
			return x, nil
		case float64:
			if math.IsNaN(x) {
				return nil, fmt.Errorf("SQLite cannot represent NaN")
			}
		}
		return v, nil
	}
	switch col.typ {
	case "bool":
		if x, ok := v.(bool); ok {
			return x, nil
		}
		switch strings.ToLower(fmt.Sprint(v)) {
		case "0", "false", "f":
			return false, nil
		case "1", "true", "t":
			return true, nil
		}
		return nil, fmt.Errorf("invalid boolean")
	case "timestamp", "timestamptz", "date":
		return timestamp(v)
	case "int2", "int4", "int8":
		if n, ok := v.(int64); ok {
			return n, nil
		}
		return strconv.ParseInt(fmt.Sprint(v), 10, 64)
	case "float4", "float8":
		return strconv.ParseFloat(fmt.Sprint(v), 64)
	case "bytea":
		if b, ok := v.([]byte); ok {
			return b, nil
		}
		if s, ok := v.(string); ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("invalid binary value")
	case "json", "jsonb":
		s := fmt.Sprint(v)
		if b, ok := v.([]byte); ok {
			s = string(b)
		}
		if !json.Valid([]byte(s)) {
			return nil, fmt.Errorf("malformed JSON")
		}
		return s, nil
	case "numeric":
		return fmt.Sprint(v), nil
	case "text", "varchar", "bpchar", "uuid":
		if b, ok := v.([]byte); ok {
			if !utf8.Valid(b) {
				return nil, fmt.Errorf("invalid UTF-8")
			}
			return string(b), nil
		}
		if t, ok := v.(time.Time); ok {
			return t.UTC().Format(time.RFC3339Nano), nil
		}
		return fmt.Sprint(v), nil
	}
	return v, nil
}
