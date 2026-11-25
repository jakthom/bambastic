package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"golang.org/x/text/unicode/norm"
)

func labelView(v obj) obj {
	layout := obj{}
	for _, k := range []string{"page", "label_w_mm", "label_h_mm", "cols", "rows", "top_mm", "left_mm", "col_gap_mm", "row_gap_mm"} {
		layout[k] = v[k]
	}
	return obj{"id": v["id"], "name": v["name"], "layout": layout, "created_at": v["created_at"], "updated_at": v["updated_at"]}
}
func (s *Service) registerLabels(m *http.ServeMux) {
	for _, p := range []string{"GET /api/v1/label-templates", "POST /api/v1/label-templates", "PUT /api/v1/label-templates/{id}", "DELETE /api/v1/label-templates/{id}"} {
		m.HandleFunc(p, s.label)
	}
}
func (s *Service) label(w http.ResponseWriter, r *http.Request) {
	perm := "inventory:read"
	if r.Method != "GET" {
		perm = "inventory:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	if r.Method == "GET" {
		rows, e := query(s.DB, "SELECT * FROM label_templates ORDER BY name_key")
		if e != nil {
			httpx.Error(w, 500, "Label templates unavailable")
			return
		}
		for i, v := range rows {
			rows[i] = labelView(v)
		}
		httpx.JSON(w, 200, rows)
		return
	}
	if r.Method == "DELETE" {
		v, e := s.DB.Exec("DELETE FROM label_templates WHERE id=?", r.PathValue("id"))
		if e != nil {
			httpx.Error(w, 500, "Cannot remove template")
			return
		}
		if n, _ := v.RowsAffected(); n == 0 {
			httpx.Error(w, 404, "Unknown template")
			return
		}
		w.WriteHeader(204)
		return
	}
	var in struct {
		Name   string
		Layout obj
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	l := in.Layout
	if in.Name == "" || len(in.Name) > 100 || l == nil || l["page"] != "a4" && l["page"] != "letter" {
		httpx.Error(w, 422, "Choose a template name and paper size")
		return
	}
	for k, bounds := range map[string][2]float64{"label_w_mm": {40, 300}, "label_h_mm": {20, 300}, "top_mm": {0, 300}, "left_mm": {0, 300}, "col_gap_mm": {0, 300}, "row_gap_mm": {0, 300}, "cols": {1, 20}, "rows": {1, 40}} {
		n, ok := l[k].(float64)
		if !ok && (k == "col_gap_mm" || k == "row_gap_mm") {
			l[k] = float64(0)
			ok = true
		}
		if !ok || n < bounds[0] || n > bounds[1] || (k == "cols" || k == "rows") && math.Trunc(n) != n {
			httpx.Error(w, 422, "Invalid "+k)
			return
		}
	}
	args := []any{in.Name, strings.ToLower(norm.NFKC.String(in.Name)), l["page"], l["label_w_mm"], l["label_h_mm"], l["cols"], l["rows"], l["top_mm"], l["left_mm"], l["col_gap_mm"], l["row_gap_mm"]}
	var id any = r.PathValue("id")
	status := 200
	if r.Method == "POST" {
		res, e := s.DB.Exec("INSERT INTO label_templates(name,name_key,page,label_w_mm,label_h_mm,cols,rows,top_mm,left_mm,col_gap_mm,row_gap_mm) VALUES(?,?,?,?,?,?,?,?,?,?,?)", args...)
		if e != nil {
			httpx.Error(w, 409, "Template name already exists")
			return
		}
		id, _ = res.LastInsertId()
		status = 201
	} else {
		res, e := s.DB.Exec("UPDATE label_templates SET name=?,name_key=?,page=?,label_w_mm=?,label_h_mm=?,cols=?,rows=?,top_mm=?,left_mm=?,col_gap_mm=?,row_gap_mm=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", append(args, id)...)
		if e != nil {
			httpx.Error(w, 409, "Template name already exists")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.Error(w, 404, "Unknown template")
			return
		}
	}
	rows, e := query(s.DB, "SELECT * FROM label_templates WHERE id=?", id)
	if e != nil || len(rows) == 0 {
		httpx.Error(w, 500, "Cannot load template")
		return
	}
	httpx.JSON(w, status, labelView(rows[0]))
}
func (s *Service) announcements(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	enabled, e := access.Setting(r.Context(), s.DB, "announcements_enabled", "true")
	if e != nil {
		httpx.Error(w, 500, "Announcements unavailable")
		return
	}
	all, e := access.Setting(r.Context(), s.DB, "announcements_show_to_all", "false")
	if e != nil {
		httpx.Error(w, 500, "Announcements unavailable")
		return
	}
	visible := enabled == "true" && (p.Admin || all == "true")
	var uid any
	if p.ID != 0 {
		uid = p.ID
	}
	if r.Method == "POST" {
		if !visible {
			httpx.Error(w, 403, "Announcements unavailable")
			return
		}
		var id int64
		e = s.DB.QueryRow("SELECT id FROM announcements WHERE public_id=? AND (published_at IS NULL OR unixepoch(published_at)<=unixepoch())", r.PathValue("id")).Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			httpx.Error(w, 404, "Unknown announcement")
			return
		}
		if e == nil {
			_, e = s.DB.Exec("INSERT INTO announcement_reads(announcement_id,user_id) SELECT ?,? WHERE NOT EXISTS(SELECT 1 FROM announcement_reads WHERE announcement_id=? AND user_id IS ?)", id, uid, id, uid)
		}
		if e != nil {
			httpx.Error(w, 500, "Cannot mark announcement read")
			return
		}
		w.WriteHeader(204)
		return
	}
	out := []obj{}
	if visible {
		out, e = query(s.DB, `SELECT public_id AS id,level,texts,link_url,published_at,expires_at,coalesce(unixepoch(expires_at)<unixepoch(),0) AS archived,EXISTS(SELECT 1 FROM announcement_reads ar WHERE ar.announcement_id=a.id AND ar.user_id IS ?) AS read FROM announcements a WHERE published_at IS NULL OR unixepoch(published_at)<=unixepoch() ORDER BY published_at DESC,a.id DESC`, uid)
		if e != nil {
			httpx.Error(w, 500, "Announcements unavailable")
			return
		}
		for _, v := range out {
			var texts obj
			_ = json.Unmarshal([]byte(httpx.Str(v["texts"])), &texts)
			v["texts"] = texts
			v["read"] = httpx.Bool(v["read"]) || httpx.Bool(v["archived"])
			v["archived"] = httpx.Bool(v["archived"])
		}
	}
	httpx.JSON(w, 200, obj{"visible": visible, "announcements": out})
}
