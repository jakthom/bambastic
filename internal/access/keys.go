package access

import (
	"bambastic/internal/httpx"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

//go:embed key_permissions.json
var keyMapping []byte
var flags = []string{"can_queue", "can_control_printer", "can_read_status", "can_manage_library", "can_manage_inventory", "can_manage_maintenance", "can_manage_archives", "can_manage_projects", "can_access_cloud", "can_update_energy_cost", "can_send_notifications"}

// Tagged installations used passlib hashes and indexed the first eight characters.
// Upgrade the stored digest after verifying possession of an imported key.
func (s *Service) resolveKey(ctx context.Context, token string) (Principal, error) {
	hash := digest(token)
	p, err := s.apiKey(ctx, hash, 0)
	if err == nil || len(token) < 8 || len(token) > 256 {
		return p, err
	}
	rows, e := s.DB.QueryContext(ctx, "SELECT id,key_hash FROM api_keys WHERE key_prefix=? AND enabled AND (expires_at IS NULL OR unixepoch(expires_at)>unixepoch())", token[:8]+"...")
	if e != nil {
		return Principal{}, e
	}
	var found int64
	var old string
	for rows.Next() {
		var id int64
		var legacy string
		if e = rows.Scan(&id, &legacy); e != nil {
			rows.Close()
			return Principal{}, e
		}
		if verify(token, legacy) {
			found, old = id, legacy
			break
		}
	}
	rows.Close()
	if found == 0 {
		return Principal{}, err
	}
	if _, e = s.DB.ExecContext(ctx, "UPDATE api_keys SET key_hash=? WHERE id=? AND key_hash=?", hash, found, old); e != nil {
		return Principal{}, e
	}
	return s.apiKey(ctx, hash, 0)
}

func (s *Service) apiKey(ctx context.Context, hash string, id int64) (Principal, error) {
	sqlText := "SELECT * FROM api_keys WHERE enabled AND (expires_at IS NULL OR unixepoch(expires_at)>unixepoch()) AND "
	var arg any
	if id > 0 {
		sqlText += "id=?"
		arg = id
	} else {
		sqlText += "key_hash=?"
		arg = hash
	}
	rows, e := s.DB.QueryContext(ctx, sqlText, arg)
	if e != nil {
		return Principal{}, e
	}
	list, e := httpx.Rows(rows)
	if e != nil || len(list) != 1 {
		return Principal{}, errors.New("invalid key")
	}
	v := list[0]
	uid, _ := v["user_id"].(int64)
	owner, e := s.User(ctx, uid)
	if e != nil {
		return Principal{}, e
	}
	p := Principal{ID: uid, Username: owner.Username, KeyID: v["id"].(int64), Permissions: map[string]bool{}, PrinterIDs: owner.PrinterIDs}
	var m map[string][]string
	json.Unmarshal(keyMapping, &m)
	for perm, required := range m {
		yes := owner.Has(perm)
		for _, f := range required {
			yes = yes && httpx.Bool(v[f])
		}
		if yes {
			p.Permissions[perm] = true
		}
	}
	p.Permissions["settings:update_energy_cost"] = httpx.Bool(v["can_update_energy_cost"]) && owner.Has("settings:update")
	p.Permissions["notifications:send"] = httpx.Bool(v["can_send_notifications"]) && owner.Has("notifications:update")
	if raw, ok := v["printer_ids"].(string); ok && raw != "null" {
		var ids []int64
		if e = json.Unmarshal([]byte(raw), &ids); e != nil {
			return Principal{}, e
		}
		p.PrinterIDs = map[int64]bool{}
		for _, id := range ids {
			if owner.AllowsPrinter(id) {
				p.PrinterIDs[id] = true
			}
		}
	}
	return p, nil
}
func (s *Service) registerKeys(api *http.ServeMux) {
	for _, p := range []string{"/api/v1/api-keys", "/api/v1/api-keys/{$}"} {
		api.HandleFunc("GET "+p, s.keys)
		api.HandleFunc("POST "+p, s.saveKey)
	}
	api.HandleFunc("GET /api/v1/api-keys/{id}", s.keys)
	api.HandleFunc("PATCH /api/v1/api-keys/{id}", s.saveKey)
	api.HandleFunc("DELETE /api/v1/api-keys/{id}", s.dropKey)
}
func (s *Service) keys(w http.ResponseWriter, r *http.Request) {
	if !Require(w, r, "api_keys:read") {
		return
	}
	p := From(r.Context())
	query := "SELECT * FROM api_keys WHERE 1=1"
	args := []any{}
	if !p.Admin {
		query += " AND user_id=?"
		args = append(args, p.ID)
	}
	id := r.PathValue("id")
	if id != "" {
		query += " AND id=?"
		args = append(args, id)
	}
	rows, e := s.DB.QueryContext(r.Context(), query+" ORDER BY id", args...)
	if e != nil {
		dbfail(w, e)
		return
	}
	v, e := httpx.Rows(rows)
	if e != nil {
		dbfail(w, e)
		return
	}
	for _, item := range v {
		formatKey(item)
	}
	if id != "" {
		if len(v) == 0 {
			dbfail(w, sql.ErrNoRows)
			return
		}
		httpx.JSON(w, 200, v[0])
		return
	}
	httpx.JSON(w, 200, v)
}
func formatKey(v object) {
	delete(v, "key_hash")
	if text, ok := v["printer_ids"].(string); ok {
		var ids []int64
		json.Unmarshal([]byte(text), &ids)
		v["printer_ids"] = ids
	}
}
func (s *Service) saveKey(w http.ResponseWriter, r *http.Request) {
	create := r.Method == "POST"
	perm := "api_keys:update"
	if create {
		perm = "api_keys:create"
	}
	if !Require(w, r, perm) {
		return
	}
	var in object
	if !httpx.Decode(w, r, &in) {
		return
	}
	p := From(r.Context())
	values := object{}
	for _, f := range flags {
		if b, ok := in[f]; ok {
			if _, yes := b.(bool); !yes {
				httpx.Error(w, 422, "Expected boolean")
				return
			}
			values[f] = b
		}
	}
	if v, ok := in["printer_ids"]; ok && v != nil {
		list, ok := v.([]any)
		if !ok {
			httpx.Error(w, 422, "Expected printer list")
			return
		}
		for _, v := range list {
			id, ok := v.(float64)
			if !ok || id <= 0 || float64(int64(id)) != id || !p.AllowsPrinter(int64(id)) {
				httpx.Error(w, 422, "Invalid printer")
				return
			}
		}
		b, _ := json.Marshal(list)
		values["printer_ids"] = string(b)
	} else if ok {
		values["printer_ids"] = nil
	}
	if v, ok := in["name"]; ok {
		if strings.TrimSpace(httpx.Str(v)) == "" {
			httpx.Error(w, 422, "Name required")
			return
		}
		values["name"] = v
	}
	if v, ok := in["expires_at"]; ok {
		if v == nil {
			values["expires_at"] = nil
		} else {
			t, e := time.Parse(time.RFC3339, httpx.Str(v))
			if e != nil {
				httpx.Error(w, 422, "Invalid expiration")
				return
			}
			values["expires_at"] = t.UTC().Format("2006-01-02 15:04:05")
		}
	}
	if v, ok := in["enabled"].(bool); ok {
		values["enabled"] = v
	}
	var secret string
	var id any = r.PathValue("id")
	status := 200
	if create {
		if values["name"] == nil {
			httpx.Error(w, 422, "Name required")
			return
		}
		secret = "bb_" + random()
		values["key_hash"] = digest(secret)
		values["key_prefix"] = secret[:8] + "..."
		values["enabled"] = true
		if p.ID > 0 {
			values["user_id"] = p.ID
		}
		for _, f := range flags {
			if _, ok := values[f]; !ok {
				values[f] = f == "can_read_status" || f == "can_queue" || strings.HasPrefix(f, "can_manage_")
			}
		}
	}
	cols := []string{}
	places := []string{}
	args := []any{}
	for k, v := range values {
		cols = append(cols, k)
		places = append(places, "?")
		args = append(args, v)
	}
	if create {
		res, e := s.DB.Exec("INSERT INTO api_keys("+strings.Join(cols, ",")+") VALUES("+strings.Join(places, ",")+")", args...)
		if e != nil {
			dbfail(w, e)
			return
		}
		id, _ = res.LastInsertId()
		status = 201
	} else {
		if len(cols) == 0 {
			httpx.Error(w, 422, "No changes")
			return
		}
		for i := range cols {
			cols[i] += "=?"
		}
		q := "UPDATE api_keys SET " + strings.Join(cols, ",") + " WHERE id=?"
		args = append(args, id)
		if !p.Admin {
			q += " AND user_id=?"
			args = append(args, p.ID)
		}
		res, e := s.DB.Exec(q, args...)
		if e != nil {
			dbfail(w, e)
			return
		}
		if n, _ := res.RowsAffected(); n == 0 {
			dbfail(w, sql.ErrNoRows)
			return
		}
	}
	rows, e := s.DB.Query("SELECT * FROM api_keys WHERE id=?", id)
	if e != nil {
		dbfail(w, e)
		return
	}
	v, e := httpx.Rows(rows)
	if e != nil || len(v) != 1 {
		dbfail(w, sql.ErrNoRows)
		return
	}
	formatKey(v[0])
	if create {
		v[0]["key"] = secret
	}
	httpx.JSON(w, status, v[0])
}
func (s *Service) dropKey(w http.ResponseWriter, r *http.Request) {
	if !Require(w, r, "api_keys:delete") {
		return
	}
	p := From(r.Context())
	q := "DELETE FROM api_keys WHERE id=?"
	args := []any{r.PathValue("id")}
	if !p.Admin {
		q += " AND user_id=?"
		args = append(args, p.ID)
	}
	res, e := s.DB.Exec(q, args...)
	if e != nil {
		dbfail(w, e)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		dbfail(w, sql.ErrNoRows)
		return
	}
	w.WriteHeader(204)
}

// KeyByID resolves the current owner, grants, expiry and printer scope for deferred work.
func (s *Service) KeyByID(ctx context.Context, id int64) (Principal, error) {
	if id <= 0 {
		return Principal{}, errors.New("invalid key")
	}
	return s.apiKey(ctx, "", id)
}
