package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func tokenSecret() (string, string) {
	var b [36]byte
	rand.Read(b[:])
	p := hex.EncodeToString(b[:4])
	return "bblt_" + p + "_" + hex.EncodeToString(b[4:]), p
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }
func (s *Service) mediaIdentity(r *http.Request, scope string) (access.Principal, error) {
	ctx := r.Context()
	t := r.URL.Query().Get("token")
	if t == "" {
		return s.auth.Resolve(ctx, access.Credential(r), "session")
	}
	if !strings.HasPrefix(t, "bblt_") {
		if scope != "camera" && scope != "archive" {
			return access.Principal{}, errors.New("Display token required")
		}
		return s.auth.Resolve(ctx, t, "media")
	}
	if len(t) < 14 || t[13] != '_' {
		return access.Principal{}, errors.New("Invalid display token")
	}
	var id, user int64
	var stored, kind string
	e := s.db.QueryRowContext(ctx, "SELECT id,user_id,secret_hash,scope FROM long_lived_tokens WHERE lookup_prefix=? AND revoked_at IS NULL AND unixepoch(expires_at)>unixepoch()", t[5:13]).Scan(&id, &user, &stored, &kind)
	if e != nil || subtle.ConstantTimeCompare([]byte(digest(t)), []byte(stored)) != 1 && !access.VerifyPassword(t, stored) {
		return access.Principal{}, errors.New("Expired or invalid display token")
	}
	if scope != "camera" && scope != kind {
		return access.Principal{}, errors.New("Display token scope does not permit this feed")
	}
	p, e := s.auth.User(ctx, user)
	if e != nil || !p.Has("camera:view") {
		return access.Principal{}, errors.New("Camera permission revoked")
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE long_lived_tokens SET last_used_at=CURRENT_TIMESTAMP WHERE id=? AND (last_used_at IS NULL OR unixepoch(last_used_at)<unixepoch()-60)", id)
	return p, nil
}
func (s *Service) withMedia(scope string, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, e := s.mediaIdentity(r, scope)
		if e != nil {
			httpx.Error(w, 401, e.Error())
			return
		}
		h(w, r.WithContext(access.With(r.Context(), p)))
	}
}
func (s *Service) registerMedia(public, api *http.ServeMux) {
	media := func(pattern, scope string, h http.HandlerFunc) {
		public.HandleFunc(pattern, s.withMedia(scope, h))
		if api != public {
			api.HandleFunc(pattern, h)
		}
	}
	base := "/api/v1/printers/{id}/camera/"
	api.HandleFunc("GET /api/v1/printers/usb-cameras", s.usbList)
	api.HandleFunc("POST "+base+"external/test", s.testExternal)
	media("GET "+base+"stream", "camera", s.camera)
	media("GET "+base+"snapshot", "camera", s.camera)
	media("GET /api/v1/camwall/printers", "camwall", s.wall)
	media("GET /api/v1/printers/{id}/overlay-status", "overlay", s.overlay)
	for _, item := range []struct{ method, path string }{{"GET", "status"}, {"GET", "test"}, {"GET", "stop"}, {"POST", "stop"}, {"POST", "diagnose"}} {
		api.HandleFunc(item.method+" "+base+item.path, s.camera)
	}
	api.HandleFunc("POST /api/v1/printers/camera/stream-token", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "camera:view") {
			return
		}
		token, e := s.auth.Mint(r.Context(), access.From(r.Context()), "media", time.Hour, access.Credential(r))
		if e != nil {
			httpx.Error(w, 500, "Could not issue stream token")
			return
		}
		httpx.JSON(w, 200, object{"token": token})
	})
	for _, v := range []string{"POST /api/v1/auth/tokens", "GET /api/v1/auth/tokens", "GET /api/v1/auth/tokens/all", "DELETE /api/v1/auth/tokens/{id}"} {
		api.HandleFunc(v, s.tokens)
	}
	s.registerProject(public, api)
	s.registerPlates(public, api)
	s.registerBranding(public, api)
	s.registerTimelapses(public, api)
}

const tokenSelect = "id,user_id,name,scope,lookup_prefix,created_at,expires_at,last_used_at"

func (s *Service) tokens(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	if !access.Require(w, r, "camera:view") {
		return
	}
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "Sign in with a user account to manage display tokens")
		return
	}
	ctx := r.Context()
	switch r.Method {
	case "POST":
		var req struct {
			Name  string
			Scope string
			Days  int `json:"expires_in_days"`
		}
		req.Days = 90
		req.Scope = "camera_stream"
		if !httpx.Decode(w, r, &req) {
			return
		}
		if req.Days < 1 || req.Days > 365 || strings.TrimSpace(req.Name) == "" || len(req.Name) > 100 || (req.Scope != "camera_stream" && req.Scope != "camwall" && req.Scope != "overlay") {
			httpx.Error(w, 422, "Supply a name, a display scope, and a lifetime of 1–365 days")
			return
		}
		secret, prefix := tokenSecret()
		res, e := s.db.ExecContext(ctx, "INSERT INTO long_lived_tokens(user_id,name,lookup_prefix,secret_hash,scope,expires_at) VALUES(?,?,?,?,?,?)", p.ID, req.Name, prefix, digest(secret), req.Scope, time.Now().UTC().AddDate(0, 0, req.Days).Format(time.RFC3339))
		if e != nil {
			httpx.Error(w, 500, "Could not create display token")
			return
		}
		id, _ := res.LastInsertId()
		a, e := readRows(ctx, s.db, "SELECT "+tokenSelect+" FROM long_lived_tokens WHERE id=?", id)
		if e != nil || len(a) == 0 {
			httpx.Error(w, 500, "Could not read display token")
			return
		}
		a[0]["token"] = secret
		httpx.JSON(w, 201, a[0])
	case "GET":
		q := "SELECT " + tokenSelect + ",NULL token FROM long_lived_tokens WHERE revoked_at IS NULL"
		args := []any{}
		if strings.HasSuffix(r.URL.Path, "/all") {
			if !p.Admin {
				httpx.Error(w, 403, "Administrator access required")
				return
			}
		} else {
			q += " AND user_id=?"
			args = append(args, p.ID)
		}
		a, e := readRows(ctx, s.db, q+" ORDER BY created_at DESC", args...)
		if e != nil {
			httpx.Error(w, 500, "Could not list tokens")
			return
		}
		httpx.JSON(w, 200, a)
	case "DELETE":
		q := "UPDATE long_lived_tokens SET revoked_at=CURRENT_TIMESTAMP WHERE id=? AND revoked_at IS NULL"
		args := []any{r.PathValue("id")}
		if !p.Admin {
			q += " AND user_id=?"
			args = append(args, p.ID)
		}
		res, e := s.db.ExecContext(ctx, q, args...)
		if e != nil {
			httpx.Error(w, 500, "Could not revoke token")
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			httpx.Error(w, 404, "Token not found")
			return
		}
		w.WriteHeader(204)
	}
}
func readRows(ctx context.Context, db *sql.DB, q string, args ...any) ([]object, error) {
	rows, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(rows)
}
func num(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		i, _ := strconv.ParseInt(n, 10, 64)
		return i
	}
	return 0
}
func project(st object, keys ...string) object {
	out := object{}
	for _, k := range keys {
		out[k] = st[k]
	}
	return out
}
func (s *Service) wall(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "camera:view") {
		return
	}
	p := access.From(r.Context())
	ctx := r.Context()
	states, e := s.farm.InitialStatus(ctx, p)
	if e != nil {
		httpx.Error(w, 500, "Unable to load printer state")
		return
	}
	out := []object{}
	for _, st := range states {
		v := project(st, "id", "name", "connected", "state", "progress", "remaining_time", "layer_num", "total_layers")
		var model string
		_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(model,'') FROM printers WHERE id=?", num(v["id"])).Scan(&model)
		v["model"] = model
		v["hms_errors"] = []object{}
		out = append(out, v)
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) overlay(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "camera:view") {
		return
	}
	id := num(r.PathValue("id"))
	p := access.From(r.Context())
	a, e := s.farm.InitialStatus(r.Context(), p)
	if e != nil {
		httpx.Error(w, 500, "Unable to load printer state")
		return
	}
	for _, v := range a {
		if num(v["id"]) == id && p.AllowsPrinter(id) {
			out := project(v, "id", "name", "connected", "state", "progress", "remaining_time", "layer_num", "total_layers", "temperatures", "current_print", "gcode_file", "stg_cur_name")
			var model string
			var rotation int
			_ = s.db.QueryRowContext(r.Context(), "SELECT COALESCE(model,''),camera_rotation FROM printers WHERE id=?", id).Scan(&model, &rotation)
			out["model"] = model
			out["camera_rotation"] = rotation
			out["time_format"], _ = access.Setting(r.Context(), s.db, "time_format", "system")
			httpx.JSON(w, 200, out)
			return
		}
	}
	httpx.Error(w, 404, "Printer not found")
}
