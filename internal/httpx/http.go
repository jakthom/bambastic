// Package httpx implements the JSON envelopes used by the application API.
package httpx

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	DefaultListenAddr = "127.0.0.1:8080"
	DefaultURL        = "http://" + DefaultListenAddr
)

type Object = map[string]any

func JSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if status != 204 {
		_ = json.NewEncoder(w).Encode(v)
	}
}
func Error(w http.ResponseWriter, status int, detail string) {
	JSON(w, status, Object{"detail": detail})
}
func Decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20))
	if err := dec.Decode(v); err != nil {
		Error(w, 422, "Invalid JSON request body")
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		Error(w, 422, "Expected one JSON value")
		return false
	}
	return true
}
func Rows(rows *sql.Rows) ([]Object, error) {
	defer rows.Close()
	columns, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	result := []Object{}
	for rows.Next() {
		values := make([]any, len(columns))
		ptr := make([]any, len(columns))
		for i := range ptr {
			ptr[i] = &values[i]
		}
		if err := rows.Scan(ptr...); err != nil {
			return nil, err
		}
		row := Object{}
		for i, column := range columns {
			v := values[i]
			if b, ok := v.([]byte); ok {
				v = string(b)
			}
			if v != nil && strings.EqualFold(column.DatabaseTypeName(), "BOOLEAN") {
				v = Bool(v)
			}
			row[column.Name()] = v
		}
		result = append(result, row)
	}
	return result, rows.Err()
}
func Str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
func Bool(v any) bool { return v == true || v == int64(1) || strings.EqualFold(fmt.Sprint(v), "true") }
