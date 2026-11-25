// Package control implements installation configuration and operator controls.
package control

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
)

type obj = map[string]any

//go:embed settings.json
var configBytes []byte

type schema struct {
	Type             string
	Default          any
	AnyOf            []schema
	Enum             []any
	Minimum, Maximum *float64
}
type configSpec struct {
	Read  map[string]schema `json:"AppSettings"`
	Write map[string]schema `json:"AppSettingsUpdate"`
	Prefs []string          `json:"_UI_PREFERENCE_FIELDS"`
	Flags []string          `json:"_UI_FLAG_FIELDS"`
}
type Service struct {
	DB   *sql.DB
	cfg  configSpec
	data string
	auth *access.Service
}

func New(db *sql.DB, data string, auth *access.Service) *Service {
	s := &Service{DB: db, data: data, auth: auth}
	if e := json.Unmarshal(configBytes, &s.cfg); e != nil {
		panic(e)
	}
	return s
}
func (s *Service) settings(ctx context.Context) (obj, error) {
	out := obj{}
	for k, v := range s.cfg.Read {
		out[k] = v.Default
	}
	for _, key := range []string{"light_accent", "dark_accent"} {
		out[key] = "blue"
	}
	out["dark_background"] = "cool"
	out["light_background"] = "cool"
	q, e := s.DB.QueryContext(ctx, "SELECT key,value FROM settings")
	if e != nil {
		return nil, e
	}
	defer q.Close()
	for q.Next() {
		var k, v string
		if e := q.Scan(&k, &v); e != nil {
			return nil, e
		}
		sc, ok := s.cfg.Read[k]
		if !ok {
			continue
		}
		if secret(k) {
			if v != "" {
				out[k] = "********"
			}
			continue
		}
		if sc.Type == "string" || len(sc.AnyOf) > 0 && sc.AnyOf[0].Type == "string" {
			out[k] = v
		} else {
			var a any
			if json.Unmarshal([]byte(v), &a) == nil {
				out[k] = a
			} else {
				out[k] = v
			}
		}
	}
	return out, q.Err()
}
func secret(k string) bool {
	return !strings.HasSuffix(k, "_configured") && !strings.HasSuffix(k, "_from_env") && (strings.Contains(k, "password") || strings.HasSuffix(k, "_token") || strings.HasSuffix(k, "_api_key") || strings.Contains(k, "client_secret"))
}
func valid(sc schema, v any) bool {
	if len(sc.AnyOf) > 0 {
		for _, o := range sc.AnyOf {
			if valid(o, v) {
				return true
			}
		}
		return false
	}
	if len(sc.Enum) > 0 {
		found := false
		for _, x := range sc.Enum {
			if x == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	switch sc.Type {
	case "number", "integer":
		n, ok := v.(float64)
		return ok && (sc.Type != "integer" || math.Trunc(n) == n) && (sc.Minimum == nil || n >= *sc.Minimum) && (sc.Maximum == nil || n <= *sc.Maximum)
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "string":
		_, ok := v.(string)
		return ok
	case "array":
		_, ok := v.([]any)
		return ok
	case "object":
		_, ok := v.(map[string]any)
		return ok
	case "null":
		return v == nil
	}
	return true
}
func (s *Service) config(w http.ResponseWriter, r *http.Request) {
	isPrefs := strings.HasSuffix(r.URL.Path, "/ui-preferences")
	flags := strings.HasSuffix(r.URL.Path, "/ui-flags")
	if r.Method == "GET" {
		if !isPrefs && !flags && !access.Require(w, r, "settings:read") {
			return
		}
	} else {
		if !access.Require(w, r, "settings:update") {
			return
		}
		var in obj
		if !httpx.Decode(w, r, &in) {
			return
		}
		tx, e := s.DB.Begin()
		if e != nil {
			httpx.Error(w, 500, "Configuration unavailable")
			return
		}
		defer tx.Rollback()
		// Moving a connector to a different destination invalidates its saved credential.
		for credential, destinations := range map[string][]string{"obico_ml_token": {"obico_ml_url"}, "ha_token": {"ha_url"}, "mqtt_password": {"mqtt_broker", "mqtt_port", "mqtt_use_tls", "mqtt_username"}, "ldap_bind_password": {"ldap_server_url", "ldap_security", "ldap_bind_dn"}} {
			raw, _ := in[credential].(string)
			if raw != "" && raw != "********" {
				continue
			}
			for _, dst := range destinations {
				v, changed := in[dst]
				if !changed {
					continue
				}
				old, err := access.Setting(r.Context(), tx, dst, "")
				if err != nil {
					httpx.Error(w, 500, "Cannot read connector settings")
					return
				}
				if n := fmt.Sprint(v); strings.TrimRight(n, "/") != strings.TrimRight(old, "/") {
					if err = access.Put(tx, credential, ""); err != nil {
						httpx.Error(w, 500, "Cannot reset connector credential")
						return
					}
					break
				}
			}
		}
		for k, v := range in {
			sc, ok := s.cfg.Write[k]
			if !ok || !valid(sc, v) {
				httpx.Error(w, 422, "Invalid setting: "+k)
				return
			}
			if secret(k) {
				if v == nil || v == "" || v == "********" {
					continue
				}
				switch k {
				case "mqtt_password", "ldap_bind_password", "ha_token", "smtp_password", "prometheus_token", "obico_ml_token", "octoeverywhere_api_key":
					str, ok := v.(string)
					if !ok {
						httpx.Error(w, 422, "Invalid credential")
						return
					}
					v = s.auth.SealSecret(str)
				default:
					httpx.Error(w, 422, "This connector credential is not supported yet")
					return
				}
			}
			var raw string
			if str, ok := v.(string); ok {
				raw = str
			} else {
				b, _ := json.Marshal(v)
				raw = string(b)
			}
			if e = access.Put(tx, k, raw); e != nil {
				httpx.Error(w, 500, "Configuration write failed")
				return
			}
		}
		if e = tx.Commit(); e != nil {
			httpx.Error(w, 500, "Configuration commit failed")
			return
		}
	}
	values, e := s.settings(r.Context())
	if e != nil {
		httpx.Error(w, 500, "Cannot read settings")
		return
	}
	if isPrefs || flags {
		keys := s.cfg.Prefs
		if flags {
			keys = s.cfg.Flags
		}
		subset := obj{}
		for _, k := range keys {
			subset[k] = values[k]
		}
		if flags {
			if id, ok := subset["default_printer_id"].(float64); ok && !access.From(r.Context()).AllowsPrinter(int64(id)) {
				subset["default_printer_id"] = nil
			}
			subset["has_gcode_snippets"] = values["gcode_snippets"] != nil && values["gcode_snippets"] != ""
		}
		values = subset
	}
	httpx.JSON(w, 200, values)
}
func (s *Service) Register(public, api *http.ServeMux) {
	s.milestones(api)
	api.HandleFunc("POST /api/v1/settings/reset", s.resetPreferences)
	for _, p := range []string{"/api/v1/settings", "/api/v1/settings/{$}"} {
		for _, m := range []string{"GET", "PATCH", "PUT"} {
			api.HandleFunc(m+" "+p, s.config)
		}
	}
	public.HandleFunc("GET /api/v1/settings/ui-preferences", s.config)
	api.HandleFunc("GET /api/v1/settings/ui-flags", s.config)
	public.HandleFunc("GET /api/v1/updates/version", func(w http.ResponseWriter, r *http.Request) {
		httpx.JSON(w, 200, obj{"version": "0.1.0", "application": "Bambastic", "compatibility_version": "1.3.2"})
	})

	api.HandleFunc("GET /api/v1/settings/check-ffmpeg", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		p, e := exec.LookPath("ffmpeg")
		httpx.JSON(w, 200, obj{"installed": e == nil, "available": e == nil, "path": p})
	})
	api.HandleFunc("GET /api/v1/system/health", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		if e := s.DB.PingContext(r.Context()); e != nil {
			httpx.Error(w, 503, "Database unavailable")
			return
		}
		httpx.JSON(w, 200, obj{"status": "healthy", "database": "sqlite", "runtime": runtime.Version()})
	})
	api.HandleFunc("GET /api/v1/system/db-pool", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		httpx.JSON(w, 200, s.DB.Stats())
	})
	api.HandleFunc("GET /api/v1/system/appliance", func(w http.ResponseWriter, r *http.Request) { httpx.JSON(w, 200, obj{"is_appliance": false}) })
	api.HandleFunc("GET /api/v1/settings/default-sidebar-order", func(w http.ResponseWriter, r *http.Request) {
		v, _ := access.Setting(r.Context(), s.DB, "default_sidebar_order", "")
		httpx.JSON(w, 200, obj{"default_sidebar_order": v})
	})
	s.registerLabels(api)
	api.HandleFunc("GET /api/v1/announcements", s.announcements)
	api.HandleFunc("POST /api/v1/announcements/{id}/read", s.announcements)
	s.locations(api)
	s.registerLinks(api)
	s.registerMaintenance(api)
}

func (s *Service) resetPreferences(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	if !p.Admin || !p.Has("settings:update") {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Cannot reset settings")
		return
	}
	defer tx.Rollback()
	for key := range s.cfg.Write {
		switch key {
		case "setup_completed", "auth_enabled", "advanced_auth_enabled":
			continue
		}
		if _, e = tx.Exec("DELETE FROM settings WHERE key=?", key); e != nil {
			break
		}
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		httpx.Error(w, 500, "Cannot reset settings")
		return
	}
	out, e := s.settings(r.Context())
	if e != nil {
		httpx.Error(w, 500, "Cannot load settings")
		return
	}
	httpx.JSON(w, 200, out)
}
