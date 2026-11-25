package access

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"bambastic/internal/httpx"
	"github.com/go-ldap/ldap/v3"
)

type ldapConfig struct {
	Enabled, Provision                                          bool
	URL, BindDN, Password, Base, Filter, Security, DefaultGroup string
	Mapping                                                     map[string]string
}

func (s *Service) ldapConfig(ctx context.Context) (ldapConfig, error) {
	var c ldapConfig
	var err error
	for k, dest := range map[string]*string{"ldap_server_url": &c.URL, "ldap_bind_dn": &c.BindDN, "ldap_search_base": &c.Base, "ldap_user_filter": &c.Filter, "ldap_security": &c.Security, "ldap_default_group": &c.DefaultGroup} {
		*dest, err = Setting(ctx, s.DB, k, "")
		if err != nil {
			return c, err
		}
	}
	if c.Filter == "" {
		c.Filter = "(sAMAccountName={username})"
	}
	if c.Security == "" {
		c.Security = "starttls"
	}
	c.Password, err = s.ReadSecret(ctx, "ldap_bind_password")
	if err != nil {
		return c, err
	}
	enabled, err := Setting(ctx, s.DB, "ldap_enabled", "false")
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == "true"
	provision, err := Setting(ctx, s.DB, "ldap_auto_provision", "false")
	if err != nil {
		return c, err
	}
	c.Provision = provision == "true"
	raw, err := Setting(ctx, s.DB, "ldap_group_mapping", "")
	if raw != "" && err == nil {
		err = json.Unmarshal([]byte(raw), &c.Mapping)
	}
	return c, err
}
func (c ldapConfig) connect(ctx context.Context) (*ldap.Conn, error) {
	u, e := url.Parse(c.URL)
	if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || (u.Scheme != "ldap" && u.Scheme != "ldaps") {
		return nil, errors.New("invalid LDAP URL")
	}
	direct := c.Security == "ldaps" || u.Scheme == "ldaps"
	if !direct && c.Security != "starttls" && c.Security != "none" {
		return nil, errors.New("invalid LDAP security")
	}
	port := u.Port()
	if port == "" {
		port = "389"
		if direct {
			port = "636"
		}
	}
	d := net.Dialer{Timeout: 5 * time.Second}
	sock, e := d.DialContext(ctx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if e != nil {
		return nil, e
	}
	sock.SetDeadline(time.Now().Add(12 * time.Second))
	cfg := &tls.Config{ServerName: u.Hostname(), MinVersion: tls.VersionTLS12}
	if direct {
		secured := tls.Client(sock, cfg)
		if e = secured.HandshakeContext(ctx); e != nil {
			sock.Close()
			return nil, e
		}
		sock = secured
	}
	conn := ldap.NewConn(sock, direct)
	conn.Start()
	conn.SetTimeout(6 * time.Second)
	if !direct && c.Security == "starttls" {
		e = conn.StartTLS(cfg)
	}
	if e == nil && c.BindDN != "" {
		if c.Password == "" {
			e = errors.New("directory password required")
		} else {
			e = conn.Bind(c.BindDN, c.Password)
		}
	}
	if e != nil {
		conn.Close()
		return nil, e
	}
	return conn, nil
}

var attrs = []string{"sAMAccountName", "uid", "cn", "mail", "displayName", "memberOf"}

func (c ldapConfig) search(conn *ldap.Conn, filter string, max int) ([]*ldap.Entry, error) {
	req := ldap.NewSearchRequest(c.Base, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, max, 5, false, filter, attrs, nil)
	result, err := conn.Search(req)
	if err != nil && !ldap.IsErrorWithCode(err, ldap.LDAPResultSizeLimitExceeded) {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("no directory result")
	}
	return result.Entries, nil
}
func (c ldapConfig) lookup(conn *ldap.Conn, name string) (*ldap.Entry, error) {
	if strings.TrimSpace(name) == "" || len(name) > 100 || !strings.Contains(c.Filter, "{username}") {
		return nil, errors.New("invalid lookup")
	}
	rows, err := c.search(conn, strings.ReplaceAll(c.Filter, "{username}", ldap.EscapeFilter(name)), 2)
	if err != nil || len(rows) != 1 || rows[0].DN == "" {
		return nil, errors.New("directory user unavailable")
	}
	return rows[0], nil
}
func ldapField(e *ldap.Entry, names ...string) string {
	for _, k := range names {
		if v := e.GetEqualFoldAttributeValue(k); v != "" {
			return v
		}
	}
	return ""
}
func (s *Service) directoryUser(r *http.Request, c ldapConfig, e *ldap.Entry, create bool) (Principal, error) {
	name := ldapField(e, "sAMAccountName", "uid", "cn")
	if name == "" || len(name) > 100 {
		return Principal{}, errors.New("invalid directory username")
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		return Principal{}, err
	}
	defer tx.Rollback()
	var id int64
	var source string
	var active bool
	err = tx.QueryRow("SELECT id,auth_source,is_active FROM users WHERE username=?", name).Scan(&id, &source, &active)
	if errors.Is(err, sql.ErrNoRows) && create {
		var email any
		if v := ldapField(e, "mail"); v != "" {
			email = v
		}
		res, e := tx.Exec("INSERT INTO users(username,email,auth_source,role,is_active) VALUES(?,?,'ldap','user',1)", name, email)
		if e != nil {
			return Principal{}, e
		}
		id, _ = res.LastInsertId()
		_, err = tx.Exec("INSERT INTO user_groups(user_id,group_id) SELECT ?,coalesce((SELECT id FROM groups WHERE name=?),(SELECT id FROM groups WHERE name='Viewers'))", id, c.DefaultGroup)
	} else if err == nil && (source != "ldap" || !active) {
		return Principal{}, errors.New("account unavailable")
	}
	if err == nil {
		err = syncGroups(tx, id, c.Mapping, e.GetEqualFoldAttributeValues("memberOf"))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		return Principal{}, err
	}
	return s.User(r.Context(), id)
}
func (s *Service) ldapAuthenticate(r *http.Request, name, password string) (Principal, error) {
	if password == "" || len(password) > 1024 {
		return Principal{}, errors.New("directory credentials required")
	}
	c, err := s.ldapConfig(r.Context())
	if err != nil || !c.Enabled {
		return Principal{}, errors.New("directory disabled")
	}
	conn, err := c.connect(r.Context())
	if err != nil {
		return Principal{}, err
	}
	defer conn.Close()
	user, err := c.lookup(conn, name)
	if err != nil {
		return Principal{}, err
	}
	if err = conn.Bind(user.DN, password); err != nil {
		return Principal{}, err
	}
	return s.directoryUser(r, c, user, c.Provision)
}
func (s *Service) registerLDAP(public, api *http.ServeMux) {
	public.HandleFunc("GET /api/v1/auth/ldap/status", func(w http.ResponseWriter, r *http.Request) {
		v, e := Setting(r.Context(), s.DB, "ldap_enabled", "false")
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"enabled": v == "true", "ldap_enabled": v == "true"})
	})
	for _, spec := range []string{"POST /api/v1/auth/ldap/test", "GET /api/v1/auth/ldap/search", "POST /api/v1/auth/ldap/provision"} {
		api.HandleFunc(spec, s.ldapAdmin)
	}
}
func (s *Service) ldapAdmin(w http.ResponseWriter, r *http.Request) {
	if !From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	c, err := s.ldapConfig(r.Context())
	if err != nil {
		httpx.Error(w, 422, "Invalid directory configuration")
		return
	}
	test := strings.HasSuffix(r.URL.Path, "/test")
	var name string
	if test {
		var form map[string]string
		if r.ContentLength != 0 && !httpx.Decode(w, r, &form) {
			return
		}
		changed := false
		for k, dest := range map[string]*string{"ldap_server_url": &c.URL, "ldap_bind_dn": &c.BindDN, "ldap_security": &c.Security, "ldap_search_base": &c.Base, "ldap_user_filter": &c.Filter} {
			if v, ok := form[k]; ok {
				if k == "ldap_server_url" || k == "ldap_bind_dn" || k == "ldap_security" {
					changed = changed || *dest != v
				}
				*dest = v
			}
		}
		if v := form["ldap_bind_password"]; v != "" && v != "********" {
			c.Password = v
		} else if changed {
			c.Password = ""
		}
	} else if !c.Enabled {
		httpx.Error(w, 400, "LDAP is disabled")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/provision") {
		var form struct{ Username string }
		if !httpx.Decode(w, r, &form) {
			return
		}
		name = form.Username
	}
	conn, err := c.connect(r.Context())
	if err != nil {
		httpx.Error(w, 400, "Directory connection or bind failed")
		return
	}
	defer conn.Close()
	if test {
		httpx.JSON(w, 200, object{"success": true, "message": "Directory connection succeeded"})
		return
	}
	if r.Method == "POST" {
		u, e := c.lookup(conn, name)
		if e != nil {
			httpx.Error(w, 404, "Directory user unavailable")
			return
		}
		p, e := s.directoryUser(r, c, u, true)
		if e != nil {
			httpx.Error(w, 409, "Directory account could not be provisioned")
			return
		}
		v, e := s.userView(r, p)
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 201, v)
		return
	}
	term := strings.TrimSpace(r.URL.Query().Get("q"))
	if len(term) < 2 || len(term) > 100 {
		httpx.Error(w, 422, "Search requires 2 to 100 characters")
		return
	}
	var filter strings.Builder
	filter.WriteString("(|")
	for _, k := range attrs[:5] {
		filter.WriteString("(" + k + "=*" + ldap.EscapeFilter(term) + "*)")
	}
	filter.WriteString(")")
	users, e := c.search(conn, filter.String(), 25)
	if e != nil {
		httpx.Error(w, 502, "Directory search failed")
		return
	}
	out := []object{}
	for _, u := range users {
		name := ldapField(u, "sAMAccountName", "uid", "cn")
		var n int
		e = s.DB.QueryRow("SELECT count(*) FROM users WHERE username=?", name).Scan(&n)
		if e != nil {
			dbfail(w, e)
			return
		}
		out = append(out, object{"username": name, "email": ldapField(u, "mail"), "dn": u.DN, "display_name": ldapField(u, "displayName", "cn"), "already_provisioned": n > 0})
	}
	httpx.JSON(w, 200, out)
}
