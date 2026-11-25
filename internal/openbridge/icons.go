// Package openbridge exposes only the OpenBridge glyphs from the SPA's icon picker.
package openbridge

import (
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

//go:embed glyphs.json
var data []byte
var glyphs map[string]string

func init() {
	if e := json.Unmarshal(data, &glyphs); e != nil {
		panic(e)
	}
}
func Name(v string) string {
	v = strings.TrimPrefix(strings.TrimSpace(v), "openbridge:")
	if _, ok := glyphs[v]; ok {
		return v
	}
	return ""
}
func Match(b []byte) string {
	for k, v := range glyphs {
		if strings.TrimSpace(v) == strings.TrimSpace(string(b)) {
			return k
		}
	}
	return ""
}
func Render(name string) string { return glyphs[Name(name)] }
func Serve(w http.ResponseWriter, r *http.Request, name string) {
	b := Render(name)
	if b == "" {
		http.NotFound(w, r)
		return
	}
	etag := fmt.Sprintf("\"%x\"", sha256.Sum256([]byte(b)))
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(304)
		return
	}
	_, _ = w.Write([]byte(b))
}
