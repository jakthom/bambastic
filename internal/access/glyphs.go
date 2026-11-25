package access

import (
	"bambastic/internal/httpx"
	"bambastic/internal/openbridge"
	"database/sql"
	"net/http"
)

func (s *Service) registerProviderIcons(public, api *http.ServeMux) {
	base := "/api/v1/auth/oidc/providers/{provider}/icon"
	public.HandleFunc("GET "+base, func(w http.ResponseWriter, r *http.Request) {
		var choice sql.NullString
		var legacy, enabled bool
		if s.DB.QueryRowContext(r.Context(), "SELECT icon_url,icon_data IS NOT NULL,is_enabled FROM oidc_providers WHERE id=?", r.PathValue("provider")).Scan(&choice, &legacy, &enabled) != nil {
			httpx.Error(w, 404, "Provider icon not found")
			return
		}
		if !enabled {
			p, e := s.Resolve(r.Context(), Credential(r), "session")
			if e != nil || !p.Admin {
				httpx.Error(w, 404, "Provider icon not found")
				return
			}
		}
		name := openbridge.Name(choice.String)
		if name == "" && legacy {
			name = "globe"
		}
		openbridge.Serve(w, r, name)
	})
	for _, route := range []string{"POST " + base + "/refresh", "DELETE " + base} {
		api.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			if !From(r.Context()).Admin {
				httpx.Error(w, 403, "Administrator required")
				return
			}
			var stored sql.NullString
			var managed bool
			id := r.PathValue("provider")
			if s.DB.QueryRowContext(r.Context(), "SELECT icon_url,is_env_managed FROM oidc_providers WHERE id=?", id).Scan(&stored, &managed) != nil {
				httpx.Error(w, 404, "Provider not found")
				return
			}
			if managed {
				httpx.Error(w, 409, "Environment-managed provider")
				return
			}
			var ref, data, typ, etag any
			if r.Method == "POST" {
				name := openbridge.Name(stored.String)
				if name == "" {
					name = "globe"
				}
				ref = "openbridge:" + name
				data = []byte(openbridge.Render(name))
				typ = "image/svg+xml"
				etag = digest(openbridge.Render(name))
			}
			_, e := s.DB.ExecContext(r.Context(), "UPDATE oidc_providers SET icon_url=?,icon_data=?,icon_content_type=?,icon_etag=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", ref, data, typ, etag, id)
			if e != nil {
				dbfail(w, e)
				return
			}
			httpx.JSON(w, 200, object{"message": "Provider icon updated", "has_icon": data != nil})
		})
	}
}
