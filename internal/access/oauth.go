// OAuth authorization for the installation's MCP resource. Codes bind the client,
// user, exact callback, resource and S256 challenge in one database transaction.
package access

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"bambastic/internal/httpx"
)

var pkce = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)
var verifier = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func origin(r *http.Request) string {
	if v := strings.TrimRight(os.Getenv("BAMBASTIC_PUBLIC_URL"), "/"); v != "" {
		return v
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
func redirectOK(v string) bool {
	u, e := url.Parse(v)
	if e != nil || u.User != nil || u.Fragment != "" || u.Hostname() == "" {
		return false
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "https" || u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}
func oauthErr(w http.ResponseWriter, status int, reason string) {
	httpx.JSON(w, status, object{"error": reason})
}
func (s *Service) RegisterOAuth(router *http.ServeMux, api *http.ServeMux, mcp http.Handler) error {
	_, e := s.DB.Exec(`CREATE TABLE IF NOT EXISTS bambastic_mcp_clients(id TEXT PRIMARY KEY,name TEXT NOT NULL,redirects TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_mcp_codes(hash TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,client TEXT NOT NULL REFERENCES bambastic_mcp_clients(id),redirect TEXT NOT NULL,challenge TEXT NOT NULL,resource TEXT NOT NULL,expires INTEGER NOT NULL);`)
	if e != nil {
		return e
	}
	resource := func(w http.ResponseWriter, r *http.Request) {
		u := origin(r)
		httpx.JSON(w, 200, object{"resource": u + "/mcp", "authorization_servers": []string{u}, "scopes_supported": []string{"bambastic"}, "bearer_methods_supported": []string{"header"}})
	}
	router.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", resource)
	router.HandleFunc("GET /.well-known/oauth-protected-resource", resource)
	router.HandleFunc("GET /.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		u := origin(r)
		httpx.JSON(w, 200, object{"issuer": u, "authorization_endpoint": u + "/authorize-mcp", "token_endpoint": u + "/oauth/token", "registration_endpoint": u + "/oauth/register", "response_types_supported": []string{"code"}, "grant_types_supported": []string{"authorization_code"}, "code_challenge_methods_supported": []string{"S256"}, "token_endpoint_auth_methods_supported": []string{"none"}, "scopes_supported": []string{"bambastic"}})
	})
	router.HandleFunc("POST /oauth/register", s.second.limit(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Name      string   `json:"client_name"`
			Redirects []string `json:"redirect_uris"`
			Method    string   `json:"token_endpoint_auth_method"`
		}
		if !httpx.Decode(w, r, &v) {
			return
		}
		if len(v.Redirects) < 1 || len(v.Redirects) > 12 || len(v.Name) > 200 || v.Method != "" && v.Method != "none" {
			oauthErr(w, 400, "invalid_client_metadata")
			return
		}
		for _, u := range v.Redirects {
			if !redirectOK(u) || len(u) > 2048 {
				oauthErr(w, 400, "invalid_redirect_uri")
				return
			}
		}
		id := random()
		list, _ := json.Marshal(v.Redirects)
		_, e := s.DB.Exec("INSERT INTO bambastic_mcp_clients VALUES(?,?,?)", id, v.Name, string(list))
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 201, object{"client_id": id, "client_name": v.Name, "redirect_uris": v.Redirects, "token_endpoint_auth_method": "none", "grant_types": []string{"authorization_code"}, "response_types": []string{"code"}})
	}))
	api.HandleFunc("POST /api/v1/mcp/consent", s.consent)
	router.HandleFunc("POST /oauth/token", s.exchange)
	router.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		u := origin(r)
		token := Credential(r)
		kind := "mcp:" + u + "/mcp"
		if strings.HasPrefix(token, "bb_") {
			kind = "session"
		}
		p, e := s.Resolve(r.Context(), token, kind)
		if e != nil {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+u+`/.well-known/oauth-protected-resource/mcp"`)
			oauthErr(w, 401, "invalid_token")
			return
		}
		mcp.ServeHTTP(w, r.WithContext(With(r.Context(), p)))
	})
	return nil
}

type consentInput struct {
	Client    string `json:"client_id"`
	Redirect  string `json:"redirect_uri"`
	State     string `json:"state"`
	Challenge string `json:"code_challenge"`
	Method    string `json:"code_challenge_method"`
	Resource  string `json:"resource"`
	Response  string `json:"response_type"`
	Scope     string `json:"scope"`
	Decision  string `json:"decision"`
}

func (s *Service) consent(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "Sign in with a Bambastic account")
		return
	}
	var in consentInput
	if !httpx.Decode(w, r, &in) {
		return
	}
	u := origin(r) + "/mcp"
	if in.Resource != u || in.Method != "S256" || !pkce.MatchString(in.Challenge) || in.Response != "code" || in.Scope != "" && in.Scope != "bambastic" {
		oauthErr(w, 400, "invalid_request")
		return
	}
	var name, raw string
	if e := s.DB.QueryRow("SELECT name,redirects FROM bambastic_mcp_clients WHERE id=?", in.Client).Scan(&name, &raw); e != nil {
		oauthErr(w, 400, "invalid_client")
		return
	}
	redirects := []string{}
	json.Unmarshal([]byte(raw), &redirects)
	ok := false
	for _, v := range redirects {
		ok = ok || v == in.Redirect
	}
	if !ok {
		oauthErr(w, 400, "invalid_redirect_uri")
		return
	}
	if in.Decision == "inspect" {
		httpx.JSON(w, 200, object{"client_name": name, "redirect_uri": in.Redirect, "username": p.Username})
		return
	}
	uri, e := url.Parse(in.Redirect)
	if e != nil {
		oauthErr(w, 400, "invalid_redirect_uri")
		return
	}
	q := uri.Query()
	if in.State != "" {
		q.Set("state", in.State)
	}
	switch in.Decision {
	case "deny":
		q.Set("error", "access_denied")
	case "allow":
		code := random()
		_, e = s.DB.Exec("INSERT INTO bambastic_mcp_codes VALUES(?,?,?,?,?,?,?)", digest(code), p.ID, in.Client, in.Redirect, in.Challenge, u, time.Now().Add(time.Minute).Unix())
		if e != nil {
			dbfail(w, e)
			return
		}
		q.Set("code", code)
	default:
		oauthErr(w, 400, "invalid_request")
		return
	}
	uri.RawQuery = q.Encode()
	httpx.JSON(w, 200, object{"redirect_uri": uri.String()})
}
func (s *Service) exchange(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if r.ParseForm() != nil || r.PostForm.Get("grant_type") != "authorization_code" {
		oauthErr(w, 400, "unsupported_grant_type")
		return
	}
	f := r.PostForm
	resource := origin(r) + "/mcp"
	if f.Get("resource") != resource || !verifier.MatchString(f.Get("code_verifier")) {
		oauthErr(w, 400, "invalid_grant")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var uid int64
	var challenge string
	e = tx.QueryRow("DELETE FROM bambastic_mcp_codes WHERE hash=? AND client=? AND redirect=? AND resource=? AND expires>? RETURNING user_id,challenge", digest(f.Get("code")), f.Get("client_id"), f.Get("redirect_uri"), resource, time.Now().Unix()).Scan(&uid, &challenge)
	if errors.Is(e, sql.ErrNoRows) {
		oauthErr(w, 400, "invalid_grant")
		return
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	sum := sha256.Sum256([]byte(f.Get("code_verifier")))
	if subtle.ConstantTimeCompare([]byte(challenge), []byte(base64.RawURLEncoding.EncodeToString(sum[:]))) != 1 {
		oauthErr(w, 400, "invalid_grant")
		return
	}
	token := random()
	_, e = tx.Exec("INSERT INTO bambastic_credentials(hash,user_id,kind,expires) VALUES(?,?,?,?)", digest(token), uid, "mcp:"+resource, time.Now().Add(time.Hour).Unix())
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, object{"access_token": token, "token_type": "Bearer", "expires_in": 3600, "scope": "bambastic"})
}
