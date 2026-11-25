package access

import (
	"bambastic/internal/httpx"
	"database/sql"
	_ "embed"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

//go:embed categories.json
var categories []byte

func (s *Service) registerDirectory(api *http.ServeMux) {
	for _, path := range []string{"/api/v1/users", "/api/v1/users/{$}"} {
		api.HandleFunc("GET "+path, s.listUsers)
		api.HandleFunc("POST "+path, s.writeUser)
	}
	api.HandleFunc("GET /api/v1/users/slim", s.listUsers)
	api.HandleFunc("GET /api/v1/users/{id}", s.getUser)
	api.HandleFunc("PATCH /api/v1/users/{id}", s.writeUser)
	api.HandleFunc("DELETE /api/v1/users/{id}", s.deleteUser)
	api.HandleFunc("POST /api/v1/users/me/change-password", s.password)
	for _, path := range []string{"/api/v1/groups", "/api/v1/groups/{$}"} {
		api.HandleFunc("GET "+path, s.groups)
		api.HandleFunc("POST "+path, s.writeGroup)
	}
	api.HandleFunc("GET /api/v1/groups/{id}", s.groups)
	api.HandleFunc("PATCH /api/v1/groups/{id}", s.writeGroup)
	api.HandleFunc("DELETE /api/v1/groups/{id}", s.deleteGroup)
	api.HandleFunc("GET /api/v1/groups/permissions", func(w http.ResponseWriter, r *http.Request) {
		if !Require(w, r, "groups:read") {
			return
		}
		var c map[string][]string
		json.Unmarshal(categories, &c)
		out := []object{}
		names := []string{}
		for n := range c {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			p := []object{}
			for _, v := range c[n] {
				p = append(p, object{"value": v, "label": strings.ReplaceAll(v, "_", " ")})
			}
			out = append(out, object{"name": n, "permissions": p})
		}
		httpx.JSON(w, 200, object{"categories": out, "all_permissions": Groups()["Administrators"].Permissions})
	})
}
func admin(w http.ResponseWriter, r *http.Request) bool {
	if From(r.Context()).Admin {
		return true
	}
	httpx.Error(w, 403, "Administrator required")
	return false
}
func (s *Service) getUser(w http.ResponseWriter, r *http.Request) {
	if !Require(w, r, "users:read") {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	rows, e := s.DB.Query("SELECT id,username,email,role,is_active,auth_source,created_at FROM users WHERE id=?", id)
	if e != nil {
		dbfail(w, e)
		return
	}
	v, e := httpx.Rows(rows)
	if e != nil || len(v) == 0 {
		dbfail(w, sql.ErrNoRows)
		return
	}
	s.attachGroups(v)
	httpx.JSON(w, 200, v[0])
}
func (s *Service) attachGroups(v []object) {
	for _, row := range v {
		q, e := s.DB.Query("SELECT g.id,g.name,g.permissions FROM groups g JOIN user_groups u ON g.id=u.group_id WHERE u.user_id=?", row["id"])
		groups := []object{}
		perms := map[string]bool{}
		isAdmin := row["role"] == "admin"
		if e == nil {
			for q.Next() {
				var id int64
				var n, p string
				if q.Scan(&id, &n, &p) == nil {
					groups = append(groups, object{"id": id, "name": n})
					isAdmin = isAdmin || n == "Administrators"
					var v []string
					json.Unmarshal([]byte(p), &v)
					for _, k := range v {
						perms[k] = true
					}
				}
			}
			q.Close()
		}
		list := []string{}
		for k := range perms {
			list = append(list, k)
		}
		sort.Strings(list)
		row["is_admin"] = isAdmin
		row["permissions"] = list
		row["groups"] = groups
	}
}
func (s *Service) listUsers(w http.ResponseWriter, r *http.Request) {
	slim := strings.HasSuffix(r.URL.Path, "/slim")
	if slim {
		if !Require(w, r, "users:read_slim") {
			return
		}
	} else if !Require(w, r, "users:read") {
		return
	}
	fields := "id,username,email,role,is_active,auth_source,created_at"
	if slim {
		fields = "id,username"
	}
	q, e := s.DB.Query("SELECT " + fields + " FROM users ORDER BY username")
	if e != nil {
		dbfail(w, e)
		return
	}
	v, e := httpx.Rows(q)
	if e != nil {
		dbfail(w, e)
		return
	}
	if !slim {
		s.attachGroups(v)
	}
	httpx.JSON(w, 200, v)
}
func (s *Service) writeUser(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	var v struct {
		Username, Email, Role, Password *string
		Active                          *bool    `json:"is_active"`
		Groups                          *[]int64 `json:"group_ids"`
	}
	if !httpx.Decode(w, r, &v) {
		return
	}
	if v.Username != nil && (strings.TrimSpace(*v.Username) == "" || len(*v.Username) > 150) || v.Role != nil && *v.Role != "admin" && *v.Role != "user" || v.Password != nil && !passwordOK(*v.Password) {
		httpx.Error(w, 422, "Invalid username, role or password")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	status := 200
	if r.Method == "POST" {
		if v.Username == nil || v.Password == nil {
			httpx.Error(w, 422, "Username and password required")
			return
		}
		role := "user"
		if v.Role != nil {
			role = *v.Role
		}
		res, e := tx.Exec("INSERT INTO users(username,email,role,password_hash,is_active,auth_source) VALUES(?,?,?,?,1,'local')", *v.Username, v.Email, role, hashPassword(*v.Password))
		if e != nil {
			httpx.Error(w, 409, "Account details already in use")
			return
		}
		id, _ = res.LastInsertId()
		status = 201
		if v.Groups == nil {
			if _, e = tx.Exec("INSERT INTO user_groups SELECT ?,id FROM groups WHERE name=?", id, map[bool]string{true: "Administrators", false: "Viewers"}[role == "admin"]); e != nil {
				dbfail(w, e)
				return
			}
		}
	} else {
		if id <= 0 {
			httpx.Error(w, 404, "User not found")
			return
		}
		if id == From(r.Context()).ID && (v.Role != nil && *v.Role != "admin" || v.Active != nil && !*v.Active || v.Groups != nil) {
			httpx.Error(w, 409, "Cannot remove your own administrative access")
			return
		}
		res, e := tx.Exec("UPDATE users SET username=COALESCE(?,username),email=COALESCE(?,email),role=COALESCE(?,role),is_active=COALESCE(?,is_active),updated_at=CURRENT_TIMESTAMP WHERE id=?", v.Username, v.Email, v.Role, v.Active, id)
		if e != nil {
			httpx.Error(w, 409, "Account update conflict")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.Error(w, 404, "User not found")
			return
		}
		if v.Password != nil {
			if e = replacePassword(tx, id, *v.Password); e != nil {
				dbfail(w, e)
				return
			}
		}
	}
	if v.Groups != nil {
		if _, e = tx.Exec("DELETE FROM user_groups WHERE user_id=?", id); e != nil {
			dbfail(w, e)
			return
		}
		for _, gid := range *v.Groups {
			if _, e = tx.Exec("INSERT OR IGNORE INTO user_groups(user_id,group_id) VALUES(?,?)", id, gid); e != nil {
				httpx.Error(w, 422, "Unknown group")
				return
			}
		}
	}
	if !keepAdmin(r, tx) {
		httpx.Error(w, 409, "Keep at least one active administrator")
		return
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	q, e := s.DB.Query("SELECT id,username,email,role,is_active,auth_source,created_at FROM users WHERE id=?", id)
	if e != nil {
		dbfail(w, e)
		return
	}
	res, e := httpx.Rows(q)
	if e != nil || len(res) == 0 {
		dbfail(w, sql.ErrNoRows)
		return
	}
	s.attachGroups(res)
	httpx.JSON(w, status, res[0])
}
func (s *Service) deleteUser(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == From(r.Context()).ID {
		httpx.Error(w, 409, "Cannot remove your own account")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	res, e := tx.Exec("DELETE FROM users WHERE id=?", id)
	if e != nil {
		httpx.Error(w, 409, "Account has dependent records")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Error(w, 404, "Unknown user")
		return
	}
	if !keepAdmin(r, tx) {
		httpx.Error(w, 409, "Keep at least one active administrator")
		return
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	w.WriteHeader(204)
}
func replacePassword(tx *sql.Tx, id int64, pw string) error {
	if _, e := tx.Exec("UPDATE users SET password_hash=?,password_changed_at=CURRENT_TIMESTAMP WHERE id=?", hashPassword(pw), id); e != nil {
		return e
	}
	return revokeLogin(tx, id)
}
func (s *Service) password(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "User session required")
		return
	}
	var in struct {
		Old string `json:"current_password"`
		New string `json:"new_password"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if !passwordOK(in.New) {
		httpx.Error(w, 422, "Password does not meet complexity requirements")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var h string
	if e = tx.QueryRow("SELECT password_hash FROM users WHERE id=?", p.ID).Scan(&h); e != nil || !verify(in.Old, h) {
		httpx.Error(w, 403, "Current password required")
		return
	}
	if e = replacePassword(tx, p.ID, in.New); e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"message": "Password changed"})
}

const groupSelect = `SELECT g.*, (SELECT count(*) FROM user_groups WHERE group_id=g.id) AS user_count,(SELECT json_group_array(printer_id) FROM group_printers WHERE group_id=g.id) AS printer_ids,(SELECT json_group_array(location) FROM group_locations WHERE group_id=g.id) AS locations FROM groups g`

func (s *Service) groups(w http.ResponseWriter, r *http.Request) {
	if !Require(w, r, "groups:read") {
		return
	}
	s.groupResponse(w, r, r.PathValue("id"), 200)
}
func (s *Service) groupResponse(w http.ResponseWriter, r *http.Request, id string, status int) {
	query := groupSelect
	args := []any{}
	if id != "" {
		query += " WHERE g.id=?"
		args = append(args, id)
	}
	q, e := s.DB.Query(query+" ORDER BY g.id", args...)
	if e != nil {
		dbfail(w, e)
		return
	}
	v, e := httpx.Rows(q)
	if e != nil {
		dbfail(w, e)
		return
	}
	for _, row := range v {
		for _, k := range []string{"permissions", "printer_ids", "locations"} {
			var a []any
			json.Unmarshal([]byte(httpx.Str(row[k])), &a)
			row[k] = a
		}
	}
	if id != "" {
		if len(v) == 0 {
			httpx.Error(w, 404, "Unknown group")
			return
		}
		httpx.JSON(w, status, v[0])
		return
	}
	httpx.JSON(w, status, v)
}
func (s *Service) writeGroup(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	var in struct {
		Name, Description *string
		Permissions       *[]string
		Restricted        *bool    `json:"restrict_printers"`
		Printers          *[]int64 `json:"printer_ids"`
		Locations         *[]string
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Name != nil && strings.TrimSpace(*in.Name) == "" {
		httpx.Error(w, 422, "Name required")
		return
	}
	var perms any
	if in.Permissions != nil {
		all := Groups()["Administrators"].Permissions
		valid := map[string]bool{}
		for _, k := range all {
			valid[k] = true
		}
		for _, k := range *in.Permissions {
			if !valid[k] {
				httpx.Error(w, 422, "Unknown permission")
				return
			}
		}
		b, _ := json.Marshal(in.Permissions)
		perms = string(b)
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	status := 200
	if r.Method == "POST" {
		if in.Name == nil {
			httpx.Error(w, 422, "Name required")
			return
		}
		res, e := tx.Exec("INSERT INTO groups(name,description,permissions,is_system,restrict_printers) VALUES(?,?,COALESCE(?,'[]'),0,COALESCE(?,FALSE))", in.Name, in.Description, perms, in.Restricted)
		if e != nil {
			httpx.Error(w, 409, "Group name unavailable")
			return
		}
		id, _ = res.LastInsertId()
		status = 201
	} else {
		res, e := tx.Exec("UPDATE groups SET name=COALESCE(?,name),description=COALESCE(?,description),permissions=COALESCE(?,permissions),restrict_printers=COALESCE(?,restrict_printers),updated_at=CURRENT_TIMESTAMP WHERE id=? AND NOT is_system", in.Name, in.Description, perms, in.Restricted, id)
		if e != nil {
			httpx.Error(w, 409, "Group conflict")
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			httpx.Error(w, 409, "Unknown or system group")
			return
		}
	}
	if in.Printers != nil {
		if _, e = tx.Exec("DELETE FROM group_printers WHERE group_id=?", id); e != nil {
			dbfail(w, e)
			return
		}
		for _, v := range *in.Printers {
			if _, e = tx.Exec("INSERT OR IGNORE INTO group_printers(group_id,printer_id) VALUES(?,?)", id, v); e != nil {
				httpx.Error(w, 422, "Unknown printer")
				return
			}
		}
	}
	if in.Locations != nil {
		if _, e = tx.Exec("DELETE FROM group_locations WHERE group_id=?", id); e != nil {
			dbfail(w, e)
			return
		}
		for _, v := range *in.Locations {
			if _, e = tx.Exec("INSERT OR IGNORE INTO group_locations(group_id,location) VALUES(?,?)", id, v); e != nil {
				dbfail(w, e)
				return
			}
		}
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	s.groupResponse(w, r, strconv.FormatInt(id, 10), status)
}
func (s *Service) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	res, e := s.DB.Exec("DELETE FROM groups WHERE id=? AND NOT is_system", r.PathValue("id"))
	if e != nil {
		dbfail(w, e)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		httpx.Error(w, 409, "Unknown or system group")
		return
	}
	w.WriteHeader(204)
}
