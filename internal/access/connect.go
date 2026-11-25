package access

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"bambastic/internal/httpx"
)

const appFields = "id,name,client_id,redirect_uri,enabled,created_at,last_used_at"

func (s *Service) connectRoutes(public, api *http.ServeMux) {
	for _, p := range []string{"GET /api/v1/connect/apps", "POST /api/v1/connect/apps", "PATCH /api/v1/connect/apps/{id}", "DELETE /api/v1/connect/apps/{id}", "POST /api/v1/connect/apps/{id}/rotate-secret"} {
		api.HandleFunc(p, s.connectedApps)
	}
	api.HandleFunc("GET /api/v1/connect/authorize/info", s.connectAuthorize)
	api.HandleFunc("POST /api/v1/connect/authorize", s.connectAuthorize)
	public.HandleFunc("POST /api/v1/connect/token", s.second.limit(s.connectToken))
}
func (s *Service) connectedApps(w http.ResponseWriter, r *http.Request) {
	if !Require(w, r, "settings:update") {
		return
	}
	id := r.PathValue("id")
	var secret string
	var key any = id
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	if r.Method != "GET" {
		if id != "" {
			var n int
			e = tx.QueryRow("SELECT count(*) FROM connected_apps WHERE id=?", id).Scan(&n)
			if e != nil || n == 0 {
				httpx.Error(w, 404, "Unknown application")
				return
			}
		}
		switch {
		case r.Method == "DELETE":
			_, e = tx.Exec("DELETE FROM connected_apps WHERE id=?", id)
			if e == nil {
				e = tx.Commit()
			}
			if e != nil {
				dbfail(w, e)
				return
			}
			httpx.JSON(w, 200, object{"message": "Connected app deleted"})
			return
		case strings.HasSuffix(r.URL.Path, "/rotate-secret"):
			secret = random()
			_, e = tx.Exec("UPDATE connected_apps SET client_secret_hash=? WHERE id=?", digest(secret), id)
			if e == nil {
				_, e = tx.Exec("DELETE FROM bambastic_connect_codes WHERE app_id=?", id)
			}
		default:
			var in struct {
				Name     *string
				Redirect *string `json:"redirect_uri"`
				Enabled  *bool
			}
			if !httpx.Decode(w, r, &in) {
				return
			}
			if in.Name != nil && (strings.TrimSpace(*in.Name) == "" || len(*in.Name) > 100) || in.Redirect != nil && (!redirectOK(*in.Redirect) || len(*in.Redirect) > 500) {
				httpx.Error(w, 422, "Invalid name or callback URL")
				return
			}
			if id == "" {
				if in.Name == nil || in.Redirect == nil {
					httpx.Error(w, 422, "Name and callback URL are required")
					return
				}
				secret = random()
				var creator any
				if user := From(r.Context()).ID; user != 0 {
					creator = user
				}
				res, err := tx.Exec("INSERT INTO connected_apps(name,client_id,client_secret_hash,redirect_uri,enabled,created_by_id) VALUES(?,?,?,?,1,?)", in.Name, random()[:32], digest(secret), in.Redirect, creator)
				if err != nil {
					dbfail(w, err)
					return
				}
				key, _ = res.LastInsertId()
			} else {
				_, e = tx.Exec("UPDATE connected_apps SET name=coalesce(?,name),redirect_uri=coalesce(?,redirect_uri),enabled=coalesce(?,enabled) WHERE id=?", in.Name, in.Redirect, in.Enabled, id)
				if e == nil {
					_, e = tx.Exec("DELETE FROM bambastic_connect_codes WHERE app_id=?", id)
				}
			}
		}
		if e != nil {
			dbfail(w, e)
			return
		}
	}
	selectSQL := "SELECT " + appFields + " FROM connected_apps"
	args := []any{}
	if r.Method != "GET" {
		selectSQL += " WHERE id=?"
		args = append(args, key)
	}
	rows, e := tx.Query(selectSQL+" ORDER BY id", args...)
	if e != nil {
		dbfail(w, e)
		return
	}
	vs, e := httpx.Rows(rows)
	if e != nil {
		dbfail(w, e)
		return
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	for _, v := range vs {
		v["enabled"] = httpx.Bool(v["enabled"])
	}
	if r.Method == "GET" {
		httpx.JSON(w, 200, vs)
		return
	}
	if len(vs) == 0 {
		httpx.Error(w, 404, "Unknown application")
		return
	}
	if secret != "" {
		vs[0]["client_secret"] = secret
		w.Header().Set("Cache-Control", "no-store")
	}
	httpx.JSON(w, 200, vs[0])
}
func (s *Service) connectAuthorize(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "Sign in to authorize an application")
		return
	}
	var form consentInput
	if r.Method == "POST" {
		if !httpx.Decode(w, r, &form) {
			return
		}
		if !pkce.MatchString(form.Challenge) || form.Method != "S256" {
			httpx.Error(w, 400, "Invalid code challenge")
			return
		}
	} else {
		form.Client = r.URL.Query().Get("client_id")
		form.Redirect = r.URL.Query().Get("redirect_uri")
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var id int64
	var name string
	e = tx.QueryRow("SELECT id,name FROM connected_apps WHERE enabled AND client_id=? AND redirect_uri=?", form.Client, form.Redirect).Scan(&id, &name)
	if e != nil {
		httpx.Error(w, 400, "Invalid client or callback")
		return
	}
	var granted int
	e = tx.QueryRow("SELECT count(*) FROM connected_app_grants WHERE user_id=? AND app_id=?", p.ID, id).Scan(&granted)
	if e != nil {
		dbfail(w, e)
		return
	}
	if r.Method == "GET" {
		httpx.JSON(w, 200, object{"app_name": name, "username": p.Username, "already_granted": granted > 0})
		return
	}
	code := random()
	_, e = tx.Exec("INSERT INTO bambastic_connect_codes VALUES(?,?,?,?,?,?)", digest(code), id, p.ID, form.Redirect, form.Challenge, time.Now().Add(time.Minute).Unix())
	if e == nil && granted == 0 {
		_, e = tx.Exec("INSERT INTO connected_app_grants(app_id,user_id) VALUES(?,?)", id, p.ID)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"code": code, "redirect_uri": form.Redirect})
}
func (s *Service) connectToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Grant    string `json:"grant_type"`
		Code     string
		Redirect string `json:"redirect_uri"`
		Client   string `json:"client_id"`
		Secret   string `json:"client_secret"`
		Verifier string `json:"code_verifier"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if in.Grant != "authorization_code" || !verifier.MatchString(in.Verifier) {
		httpx.Error(w, 400, "invalid_grant")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var id, uid int64
	var hash, challenge string
	e = tx.QueryRow("SELECT id,client_secret_hash FROM connected_apps WHERE enabled AND client_id=? AND redirect_uri=?", in.Client, in.Redirect).Scan(&id, &hash)
	if e != nil || (subtle.ConstantTimeCompare([]byte(digest(in.Secret)), []byte(hash)) != 1 && !verify(in.Secret, hash)) {
		httpx.Error(w, 401, "invalid_client")
		return
	}
	e = tx.QueryRow("DELETE FROM bambastic_connect_codes WHERE hash=? AND app_id=? AND redirect=? AND expires>unixepoch() RETURNING user_id,challenge", digest(in.Code), id, in.Redirect).Scan(&uid, &challenge)
	if errors.Is(e, sql.ErrNoRows) {
		httpx.Error(w, 400, "invalid_grant")
		return
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	sum := sha256.Sum256([]byte(in.Verifier))
	if subtle.ConstantTimeCompare([]byte(challenge), []byte(base64.RawURLEncoding.EncodeToString(sum[:]))) != 1 {
		httpx.Error(w, 400, "invalid_grant")
		return
	}
	if _, e = tx.Exec("UPDATE connected_apps SET last_used_at=CURRENT_TIMESTAMP WHERE id=?", id); e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	p, e := s.User(r.Context(), uid)
	if e != nil {
		httpx.Error(w, 400, "invalid_grant")
		return
	}
	user, e := s.userView(r, p)
	if e != nil {
		dbfail(w, e)
		return
	}
	groups := []string{}
	if list, ok := user["groups"].([]object); ok {
		for _, g := range list {
			if n, ok := g["name"].(string); ok {
				groups = append(groups, n)
			}
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, object{"user": object{"id": uid, "username": p.Username, "email": user["email"], "is_admin": p.Admin, "groups": groups, "permissions": user["permissions"]}, "issued_at": time.Now().UTC()})
}
