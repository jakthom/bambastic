package control

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/openbridge"
	"database/sql"
	"net/http"
	"net/url"
	"strings"
)

func (s *Service) registerLinks(api *http.ServeMux) {
	for _, m := range []string{"GET", "POST", "DELETE"} {
		api.HandleFunc(m+" /api/v1/external-links/{id}/icon", s.linkIcon)
	}
	api.HandleFunc("PUT /api/v1/external-links/badge", s.badge)
	for _, p := range []string{"/api/v1/external-links", "/api/v1/external-links/{$}", "/api/v1/external-links/{id}"} {
		api.HandleFunc("GET "+p, s.links)
	}
	api.HandleFunc("POST /api/v1/external-links/{$}", s.editLink)
	api.HandleFunc("PATCH /api/v1/external-links/{id}", s.editLink)
	api.HandleFunc("DELETE /api/v1/external-links/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "external_links:delete") {
			return
		}
		res, e := s.DB.Exec("DELETE FROM external_links WHERE id=?", r.PathValue("id"))
		if e != nil {
			httpx.Error(w, 500, "Cannot delete link")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.Error(w, 404, "Link not found")
			return
		}
		w.WriteHeader(204)
	})
	api.HandleFunc("PUT /api/v1/external-links/reorder", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "external_links:update") {
			return
		}
		var in struct {
			IDs []int64 `json:"link_ids"`
		}
		if !httpx.Decode(w, r, &in) {
			return
		}
		tx, e := s.DB.Begin()
		if e != nil {
			httpx.Error(w, 500, "Database unavailable")
			return
		}
		defer tx.Rollback()
		seen := map[int64]bool{}
		for i, id := range in.IDs {
			res, e := tx.Exec("UPDATE external_links SET sort_order=? WHERE id=?", i, id)
			if e != nil {
				httpx.Error(w, 500, "Cannot reorder links")
				return
			}
			if n, _ := res.RowsAffected(); n == 0 || seen[id] {
				httpx.Error(w, 422, "Invalid link id")
				return
			}
			seen[id] = true
		}
		if tx.Commit() != nil {
			httpx.Error(w, 500, "Cannot reorder links")
			return
		}
		s.sendLinks(w, r, "", 200)
	})
}
func (s *Service) links(w http.ResponseWriter, r *http.Request) {
	if access.Require(w, r, "external_links:read") {
		s.sendLinks(w, r, r.PathValue("id"), 200)
	}
}
func (s *Service) sendLinks(w http.ResponseWriter, r *http.Request, id any, status int) {
	q := "SELECT id,name,url,icon,open_in_new_tab,sort_order,created_at,updated_at,(SELECT count FROM external_link_badges b WHERE b.link_id=external_links.id AND b.updated_at>datetime('now','-10 minutes')) AS badge_count FROM external_links"
	args := []any{}
	detail := id != ""
	if detail {
		q += " WHERE id=?"
		args = append(args, id)
	}
	rows, e := s.DB.QueryContext(r.Context(), q+" ORDER BY sort_order,id", args...)
	if e != nil {
		httpx.Error(w, 500, "Cannot read links")
		return
	}
	v, e := httpx.Rows(rows)
	if e != nil {
		httpx.Error(w, 500, "Cannot read links")
		return
	}
	if detail {
		if len(v) == 0 {
			httpx.Error(w, 404, "Link not found")
			return
		}
		httpx.JSON(w, status, v[0])
		return
	}
	httpx.JSON(w, status, v)
}
func (s *Service) editLink(w http.ResponseWriter, r *http.Request) {
	newLink := r.Method == "POST"
	perm := "external_links:update"
	if newLink {
		perm = "external_links:create"
	}
	if !access.Require(w, r, perm) {
		return
	}
	var in struct {
		Name, URL, Icon *string
		NewTab          *bool `json:"open_in_new_tab"`
		Order           *int  `json:"sort_order"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Icon != nil {
		v := openbridge.Name(*in.Icon)
		if v == "" {
			httpx.Error(w, 422, "Choose a registered OpenBridge icon")
			return
		}
		in.Icon = &v
	}
	if newLink && (in.Name == nil || in.URL == nil) || in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		httpx.Error(w, 422, "Name and URL required")
		return
	}
	if in.URL != nil {
		u, e := url.Parse(*in.URL)
		if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
			httpx.Error(w, 422, "Enter an HTTP or HTTPS link")
			return
		}
	}
	var id any = r.PathValue("id")
	status := 200
	var e error
	if newLink {
		var res sql.Result
		res, e = s.DB.Exec(`INSERT INTO external_links(name,url,icon,open_in_new_tab,sort_order) VALUES(?,?,COALESCE(?,'link'),COALESCE(?,0),COALESCE(?,(SELECT COALESCE(MAX(sort_order),-1)+1 FROM external_links)))`, in.Name, in.URL, in.Icon, in.NewTab, in.Order)
		if e == nil {
			id, _ = res.LastInsertId()
		}
		status = 201
	} else {
		_, e = s.DB.Exec("UPDATE external_links SET name=COALESCE(?,name),url=COALESCE(?,url),icon=COALESCE(?,icon),open_in_new_tab=COALESCE(?,open_in_new_tab),sort_order=COALESCE(?,sort_order),updated_at=CURRENT_TIMESTAMP WHERE id=?", in.Name, in.URL, in.Icon, in.NewTab, in.Order, id)
	}
	if e != nil {
		httpx.Error(w, 409, "Cannot save link")
		return
	}
	s.sendLinks(w, r, id, status)
}

func (s *Service) badge(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	if !p.Has("notifications:send") && !p.Has("notifications:update") {
		httpx.Error(w, 403, "Notification permission required")
		return
	}
	var in struct {
		URL   string
		Count *int
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Count == nil || *in.Count < 0 {
		httpx.Error(w, 422, "Expected a nonnegative badge count")
		return
	}
	place := func(raw string) string {
		u, e := url.Parse(raw)
		if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
			return ""
		}
		port := u.Port()
		if port == "" {
			if u.Scheme == "https" {
				port = "443"
			} else {
				port = "80"
			}
		}
		return u.Scheme + " " + strings.ToLower(u.Hostname()) + " " + port + " " + strings.TrimRight(u.EscapedPath(), "/")
	}
	match := place(in.URL)
	if match == "" {
		httpx.Error(w, 422, "Invalid link URL")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Database unavailable")
		return
	}
	defer tx.Rollback()
	items, e := query(tx, "SELECT id,url FROM external_links")
	if e != nil {
		httpx.Error(w, 500, "Links unavailable")
		return
	}
	n := 0
	for _, item := range items {
		if place(httpx.Str(item["url"])) != match {
			continue
		}
		_, e = tx.Exec("INSERT INTO external_link_badges(link_id,count,updated_at) VALUES(?,?,CURRENT_TIMESTAMP) ON CONFLICT(link_id) DO UPDATE SET count=excluded.count,updated_at=CURRENT_TIMESTAMP", item["id"], *in.Count)
		if e != nil {
			httpx.Error(w, 500, "Cannot update badge")
			return
		}
		n++
	}
	if n == 0 {
		httpx.Error(w, 404, "No sidebar link points to this address")
		return
	}
	if e = tx.Commit(); e != nil {
		httpx.Error(w, 500, "Badge update failed")
		return
	}
	httpx.JSON(w, 200, obj{"links": n})
}
