// Federated sign-in binds OIDC state, nonce and the one-use login exchange to the initiating browser.
package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	"bambastic/internal/openbridge"
	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const providers = "/api/v1/auth/oidc/providers"
const providerColumns = "id,name,issuer_url,client_id,client_secret,scopes,is_enabled,auto_create_users,auto_link_existing_accounts,email_claim,require_email_verified,group_claim,group_mapping,default_group_id,is_autologin,is_env_managed,icon_data IS NOT NULL AS has_icon,CASE WHEN icon_url LIKE 'openbridge:%' THEN icon_url END AS icon_url"

func (s *Service) SealSecret(v string) string { return s.second.encrypt(v) }
func (s *Service) OpenSecret(v string) (string, error) {
	if strings.HasPrefix(v, "bambastic:") {
		return s.second.decrypt(v)
	}
	if strings.HasPrefix(v, "fernet:") {
		return "", errors.New("credential requires reconfiguration")
	}
	return v, nil
}
func (s *Service) ReadSecret(ctx context.Context, key string) (string, error) {
	v, e := Setting(ctx, s.DB, key, "")
	if e != nil {
		return "", e
	}
	return s.OpenSecret(v)
}
func (s *Service) registerOIDC(public, api *http.ServeMux) {
	s.registerProviderIcons(public, api)
	for _, path := range []string{providers, providers + "/public"} {
		public.HandleFunc("GET "+path, s.oidcList)
	}
	api.HandleFunc("GET "+providers+"/all", s.oidcList)
	api.HandleFunc("POST "+providers, s.oidcSave)
	api.HandleFunc("PUT "+providers+"/{provider}", s.oidcSave)
	api.HandleFunc("DELETE "+providers+"/{provider}", s.oidcSave)
	public.HandleFunc("GET /api/v1/auth/oidc/authorize/{provider}", s.second.limit(s.oidcBegin))
	public.HandleFunc("GET /api/v1/auth/oidc/callback", s.second.limit(s.oidcCallback))
	public.HandleFunc("POST /api/v1/auth/oidc/exchange", s.second.limit(s.oidcExchange))
	api.HandleFunc("GET /api/v1/auth/oidc/links", s.oidcLinks)
	api.HandleFunc("DELETE /api/v1/auth/oidc/links/{provider}", s.oidcLinks)
}
func (s *Service) oidcLinks(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "Account session required")
		return
	}
	if r.Method == "DELETE" {
		// An OIDC-only account must retain at least one way to sign in.
		res, e := s.DB.Exec(`DELETE FROM user_oidc_links WHERE user_id=? AND provider_id=? AND (EXISTS(SELECT 1 FROM users WHERE id=? AND auth_source IN ('local','ldap')) OR EXISTS(SELECT 1 FROM user_oidc_links o JOIN oidc_providers p ON o.provider_id=p.id WHERE o.user_id=? AND o.provider_id<>? AND p.is_enabled))`, p.ID, r.PathValue("provider"), p.ID, p.ID, r.PathValue("provider"))
		if e != nil {
			dbfail(w, e)
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			httpx.Error(w, 409, "No removable sign-in link")
			return
		}
		httpx.JSON(w, 200, object{"message": "Provider unlinked"})
		return
	}
	rows, e := s.DB.Query("SELECT l.id,l.provider_id,p.name AS provider_name,l.provider_email,l.created_at FROM user_oidc_links l JOIN oidc_providers p ON p.id=l.provider_id WHERE l.user_id=? ORDER BY l.id", p.ID)
	if e != nil {
		dbfail(w, e)
		return
	}
	list, e := httpx.Rows(rows)
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, list)
}
func providerView(row object) object {
	delete(row, "client_secret")
	for _, k := range []string{"is_enabled", "auto_create_users", "auto_link_existing_accounts", "require_email_verified", "is_autologin", "is_env_managed", "has_icon"} {
		if _, ok := row[k]; ok {
			row[k] = httpx.Bool(row[k])
		}
	}
	if raw, ok := row["group_mapping"].(string); ok {
		var v map[string]string
		json.Unmarshal([]byte(raw), &v)
		row["group_mapping"] = v
	}
	return row
}
func (s *Service) oidcList(w http.ResponseWriter, r *http.Request) {
	query := "SELECT id,name,icon_data IS NOT NULL AS has_icon,is_autologin FROM oidc_providers WHERE is_enabled ORDER BY name"
	if strings.HasSuffix(r.URL.Path, "/all") {
		if !From(r.Context()).Admin {
			httpx.Error(w, 403, "Administrator required")
			return
		}
		query = "SELECT " + providerColumns + " FROM oidc_providers ORDER BY id"
	}
	rows, e := s.DB.QueryContext(r.Context(), query)
	if e != nil {
		dbfail(w, e)
		return
	}
	vs, e := httpx.Rows(rows)
	if e != nil {
		dbfail(w, e)
		return
	}
	for _, v := range vs {
		providerView(v)
	}
	httpx.JSON(w, 200, vs)
}
func (s *Service) provider(ctx context.Context, id string) (object, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT "+providerColumns+" FROM oidc_providers WHERE id=?", id)
	if e != nil {
		return nil, e
	}
	vs, e := httpx.Rows(rows)
	if e != nil {
		return nil, e
	}
	if len(vs) != 1 {
		return nil, sql.ErrNoRows
	}
	return vs[0], nil
}
func (s *Service) oidcSave(w http.ResponseWriter, r *http.Request) {
	if !From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	ctx := r.Context()
	id := r.PathValue("provider")
	status := 200
	row := object{"scopes": "openid email profile", "is_enabled": true, "auto_create_users": false, "auto_link_existing_accounts": false, "email_claim": "email", "require_email_verified": true, "group_claim": "groups", "group_mapping": "{}", "default_group_id": nil, "is_autologin": false}
	if id != "" {
		v, e := s.provider(ctx, id)
		if e != nil {
			dbfail(w, e)
			return
		}
		if httpx.Bool(v["is_env_managed"]) {
			httpx.Error(w, 409, "Provider is managed by the environment")
			return
		}
		row = v
	}
	if r.Method == "DELETE" {
		_, e := s.DB.ExecContext(ctx, "DELETE FROM oidc_providers WHERE id=?", id)
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"message": "Provider deleted"})
		return
	}
	var patch object
	if !httpx.Decode(w, r, &patch) {
		return
	}
	fields := []string{"name", "issuer_url", "client_id", "client_secret", "scopes", "is_enabled", "auto_create_users", "auto_link_existing_accounts", "email_claim", "require_email_verified", "group_claim", "group_mapping", "default_group_id", "is_autologin"}
	for _, k := range fields {
		v, ok := patch[k]
		if !ok {
			continue
		}
		switch k {
		case "is_enabled", "auto_create_users", "auto_link_existing_accounts", "require_email_verified", "is_autologin":
			if _, ok = v.(bool); !ok {
				httpx.Error(w, 422, "Invalid "+k)
				return
			}
		case "default_group_id":
			if v != nil {
				n, ok := v.(float64)
				if !ok || n < 1 || n != float64(int64(n)) {
					httpx.Error(w, 422, "Invalid group")
					return
				}
			}
		case "group_mapping":
			b, _ := json.Marshal(v)
			var groups map[string]string
			if e := json.Unmarshal(b, &groups); e != nil || groups == nil {
				httpx.Error(w, 422, "Invalid group mapping")
				return
			}
			for _, name := range groups {
				var n int
				if e := s.DB.QueryRow("SELECT count(*) FROM groups WHERE name=?", name).Scan(&n); e != nil || n != 1 {
					httpx.Error(w, 422, "Unknown group in mapping")
					return
				}
			}
			v = string(b)
		default:
			st, ok := v.(string)
			if !ok || len(st) > 4096 {
				httpx.Error(w, 422, "Invalid "+k)
				return
			}
			if k == "client_secret" {
				if st == "" && id != "" {
					continue
				}
				if st != "" {
					v = s.SealSecret(st)
				}
			}
		}
		row[k] = v
	}
	for _, k := range []string{"name", "client_id", "client_secret", "issuer_url"} {
		v, _ := row[k].(string)
		if strings.TrimSpace(v) == "" {
			httpx.Error(w, 422, "Missing "+k)
			return
		}
	}
	issuer := strings.TrimRight(row["issuer_url"].(string), "/")
	uri, e := url.Parse(issuer)
	if e != nil || !redirectOK(issuer) || uri.RawQuery != "" {
		httpx.Error(w, 422, "Invalid issuer URL")
		return
	}
	row["issuer_url"] = issuer
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	args := []any{}
	marks := []string{}
	for _, k := range fields {
		args = append(args, row[k])
		marks = append(marks, "?")
	}
	if httpx.Bool(row["is_autologin"]) {
		if _, e = tx.Exec("UPDATE oidc_providers SET is_autologin=0"); e != nil {
			dbfail(w, e)
			return
		}
	}
	if id == "" {
		res, e := tx.Exec("INSERT INTO oidc_providers("+strings.Join(fields, ",")+") VALUES("+strings.Join(marks, ",")+")", args...)
		if e != nil {
			httpx.Error(w, 422, "Invalid provider configuration")
			return
		}
		n, _ := res.LastInsertId()
		id = strconv.FormatInt(n, 10)
		status = 201
	} else {
		for i, k := range fields {
			marks[i] = k + "=?"
		}
		args = append(args, id)
		if _, e = tx.Exec("UPDATE oidc_providers SET "+strings.Join(marks, ",")+" WHERE id=?", args...); e != nil {
			httpx.Error(w, 422, "Invalid provider configuration")
			return
		}
	}
	if choice, present := patch["icon_url"]; present {
		var ref, svg, typ, etag any
		if choice != nil && choice != "" {
			name := openbridge.Name(httpx.Str(choice))
			if name == "" {
				httpx.Error(w, 422, "Choose a registered OpenBridge icon")
				return
			}
			ref = "openbridge:" + name
			svg = []byte(openbridge.Render(name))
			typ = "image/svg+xml"
			etag = digest(openbridge.Render(name))
		}
		if _, e = tx.ExecContext(ctx, "UPDATE oidc_providers SET icon_url=?,icon_data=?,icon_content_type=?,icon_etag=? WHERE id=?", ref, svg, typ, etag, id); e != nil {
			dbfail(w, e)
			return
		}
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	saved, e := s.provider(ctx, id)
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, status, providerView(saved))
}

func (s *Service) idp(r *http.Request, id string) (object, *oidc.Provider, *oauth2.Config, error) {
	ctx := oidc.ClientContext(r.Context(), lanhttp.Client)
	p, e := s.provider(ctx, id)
	if e != nil || !httpx.Bool(p["is_enabled"]) {
		return nil, nil, nil, errors.New("provider unavailable")
	}
	provider, e := oidc.NewProvider(ctx, p["issuer_url"].(string))
	if e != nil {
		return nil, nil, nil, e
	}
	secret, e := s.OpenSecret(p["client_secret"].(string))
	if e != nil {
		return nil, nil, nil, e
	}
	config := &oauth2.Config{ClientID: p["client_id"].(string), ClientSecret: secret, Endpoint: provider.Endpoint(), RedirectURL: origin(r) + "/api/v1/auth/oidc/callback", Scopes: strings.Fields(p["scopes"].(string))}
	return p, provider, config, nil
}
func oidcCookie(w http.ResponseWriter, r *http.Request, v string) {
	http.SetCookie(w, &http.Cookie{Name: "oidc_browser", Value: v, Path: "/api/v1/auth/oidc", HttpOnly: true, Secure: strings.HasPrefix(origin(r), "https://"), SameSite: http.SameSiteLaxMode, MaxAge: 600})
}
func (s *Service) oidcBegin(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("provider")
	_, _, conf, err := s.idp(r, id)
	if err != nil {
		httpx.Error(w, 502, "Identity provider discovery failed")
		return
	}
	state, nonce, cookie := random(), random(), random()
	pkce := oauth2.GenerateVerifier()
	_, err = s.DB.Exec("INSERT INTO bambastic_oidc_state(hash,provider_id,nonce,verifier,browser,expires) VALUES(?,?,?,?,?,?)", digest(state), id, nonce, pkce, digest(cookie), time.Now().Add(5*time.Minute).Unix())
	if err != nil {
		dbfail(w, err)
		return
	}
	oidcCookie(w, r, cookie)
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, object{"auth_url": conf.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(pkce))})
}
func (s *Service) oidcCallback(w http.ResponseWriter, r *http.Request) {
	fail := func(code string) { http.Redirect(w, r, "/login#oidc_error="+code, 302) }
	cookie, err := r.Cookie("oidc_browser")
	if err != nil {
		fail("invalid_state")
		return
	}
	var providerID, nonce, pkce string
	err = s.DB.QueryRow("DELETE FROM bambastic_oidc_state WHERE hash=? AND browser=? AND expires>unixepoch() RETURNING provider_id,nonce,verifier", digest(r.URL.Query().Get("state")), digest(cookie.Value)).Scan(&providerID, &nonce, &pkce)
	if err != nil {
		fail("invalid_state")
		return
	}
	p, provider, config, err := s.idp(r, providerID)
	if err != nil {
		fail("provider_not_found")
		return
	}
	ctx := oidc.ClientContext(r.Context(), lanhttp.Client)
	tok, err := config.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(pkce))
	if err != nil {
		fail("token_exchange_bad_response")
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idToken, err := provider.Verifier(&oidc.Config{ClientID: config.ClientID}).Verify(ctx, raw)
	if err != nil || idToken.Nonce != nonce || idToken.Subject == "" {
		fail("invalid_id_token")
		return
	}
	var claims object
	if err = idToken.Claims(&claims); err != nil {
		fail("invalid_id_token")
		return
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		fail("provider_error")
		return
	}
	defer tx.Rollback()
	uid, err := federatedUser(tx, p, idToken.Subject, claims)
	if err != nil {
		fail("account_not_linked")
		return
	}
	exchange := random()
	_, err = tx.Exec("INSERT INTO bambastic_oidc_exchange(hash,user_id,browser,expires) VALUES(?,?,?,?)", digest(exchange), uid, digest(cookie.Value), time.Now().Add(time.Minute).Unix())
	if err != nil || tx.Commit() != nil {
		fail("provider_error")
		return
	}
	http.Redirect(w, r, "/login#oidc_token="+exchange, 302)
}
func federatedUser(tx *sql.Tx, p object, sub string, claims object) (int64, error) {
	var uid int64
	email, _ := claims[p["email_claim"].(string)].(string)
	verified, _ := claims["email_verified"].(bool)
	if addr, err := mail.ParseAddress(email); err != nil || addr.Address != email || len(email) > 254 {
		email = ""
	}
	if httpx.Bool(p["require_email_verified"]) && !verified {
		email = ""
	}
	err := tx.QueryRow("SELECT user_id FROM user_oidc_links WHERE provider_id=? AND provider_user_id=?", p["id"], sub).Scan(&uid)
	if errors.Is(err, sql.ErrNoRows) {
		var existing int64
		if email != "" {
			e := tx.QueryRow("SELECT id FROM users WHERE lower(email)=lower(?)", email).Scan(&existing)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return 0, e
			}
		}
		if existing != 0 {
			if !httpx.Bool(p["auto_link_existing_accounts"]) || !verified {
				return 0, errors.New("account link required")
			}
			uid = existing
		} else {
			if !httpx.Bool(p["auto_create_users"]) {
				return 0, errors.New("provisioning disabled")
			}
			name, _ := claims["preferred_username"].(string)
			if name == "" {
				name = strings.Split(email, "@")[0]
			}
			if len(name) > 90 {
				name = ""
			}
			if name == "" {
				name = "oidc-" + digest(sub)[:12]
			}
			var count int
			if err = tx.QueryRow("SELECT count(*) FROM users WHERE username=?", name).Scan(&count); err != nil {
				return 0, err
			}
			if count > 0 {
				name += "-" + random()[:8]
			}
			var addr any
			if email != "" {
				addr = email
			}
			res, e := tx.Exec("INSERT INTO users(username,email,role,auth_source,is_active) VALUES(?,?,'user','oidc',1)", name, addr)
			if e != nil {
				return 0, e
			}
			uid, _ = res.LastInsertId()
			_, e = tx.Exec("INSERT INTO user_groups(user_id,group_id) VALUES(?,coalesce(?,(SELECT id FROM groups WHERE name='Viewers')))", uid, p["default_group_id"])
			if e != nil {
				return 0, e
			}
		}
		_, err = tx.Exec("INSERT INTO user_oidc_links(user_id,provider_id,provider_user_id,provider_email) VALUES(?,?,?,?)", uid, p["id"], sub, email)
	}
	if err != nil {
		return 0, err
	}
	var active bool
	if err = tx.QueryRow("SELECT is_active FROM users WHERE id=?", uid).Scan(&active); err != nil || !active {
		return 0, errors.New("account disabled")
	}
	var mapping map[string]string
	if raw, ok := p["group_mapping"].(string); ok {
		json.Unmarshal([]byte(raw), &mapping)
	}
	groups := []string{}
	switch v := claims[p["group_claim"].(string)].(type) {
	case string:
		groups = []string{v}
	case []any:
		for _, g := range v {
			if v, ok := g.(string); ok {
				groups = append(groups, v)
			}
		}
	}
	return uid, syncGroups(tx, uid, mapping, groups)
}
func syncGroups(tx *sql.Tx, uid int64, mapping map[string]string, groups []string) error {
	managed := map[string]bool{}
	for external, local := range mapping {
		if _, ok := managed[local]; !ok {
			managed[local] = false
		}
		for _, g := range groups {
			if strings.EqualFold(g, external) {
				managed[local] = true
			}
		}
	}
	for local, add := range managed {
		var err error
		if add {
			_, err = tx.Exec("INSERT OR IGNORE INTO user_groups(user_id,group_id) SELECT ?,id FROM groups WHERE name=?", uid, local)
		} else {
			_, err = tx.Exec("DELETE FROM user_groups WHERE user_id=? AND group_id=(SELECT id FROM groups WHERE name=?)", uid, local)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
func (s *Service) oidcExchange(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token string `json:"oidc_token"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	cookie, err := r.Cookie("oidc_browser")
	if err != nil {
		httpx.Error(w, 401, "Sign-in exchange expired")
		return
	}
	var uid int64
	err = s.DB.QueryRow("DELETE FROM bambastic_oidc_exchange WHERE hash=? AND browser=? AND expires>unixepoch() RETURNING user_id", digest(in.Token), digest(cookie.Value)).Scan(&uid)
	if err != nil {
		httpx.Error(w, 401, "Sign-in exchange expired")
		return
	}
	p, err := s.User(r.Context(), uid)
	if err != nil {
		httpx.Error(w, 401, "Account unavailable")
		return
	}
	if !s.challengeLogin(w, r, p) {
		s.completeLogin(w, r, p)
	}
}
