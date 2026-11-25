package access

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/httpx"
	"bambastic/internal/mailer"
)

type mailSettings struct {
	Host         string `json:"smtp_host"`
	Port         int    `json:"smtp_port"`
	Username     string `json:"smtp_username"`
	Password     string `json:"smtp_password,omitempty"`
	Security     string `json:"smtp_security"`
	Authenticate bool   `json:"smtp_auth_enabled"`
	From         string `json:"smtp_from_email"`
	Name         string `json:"smtp_from_name"`
}

func (s *Service) smtpConfig(ctx context.Context) (mailSettings, error) {
	var c mailSettings
	var e error
	for k, v := range map[string]*string{"smtp_host": &c.Host, "smtp_username": &c.Username, "smtp_security": &c.Security, "smtp_from_email": &c.From, "smtp_from_name": &c.Name} {
		*v, e = Setting(ctx, s.DB, k, "")
		if e != nil {
			return c, e
		}
	}
	c.Password, e = s.ReadSecret(ctx, "smtp_password")
	if e != nil {
		return c, e
	}
	p, e := Setting(ctx, s.DB, "smtp_port", "587")
	if e != nil {
		return c, e
	}
	c.Port, _ = strconv.Atoi(p)
	a, e := Setting(ctx, s.DB, "smtp_auth_enabled", "true")
	c.Authenticate = a == "true"
	if c.Security == "" {
		c.Security = "starttls"
	}
	return c, e
}
func (c mailSettings) send(ctx context.Context, to, subject, body string) error {
	from := mail.Address{Name: c.Name, Address: c.From}
	return mailer.Send(ctx, mailer.Config{Host: c.Host, Port: c.Port, Username: c.Username, Password: c.Password, Security: c.Security, Auth: c.Authenticate, From: from.String()}, to, subject, body)
}
func (s *Service) smtpUpdate(w http.ResponseWriter, r *http.Request) {
	if !From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	orig, e := s.smtpConfig(r.Context())
	if e != nil {
		dbfail(w, e)
		return
	}
	if r.Method == "GET" {
		if orig.Host == "" {
			httpx.JSON(w, 200, nil)
			return
		}
		orig.Password = ""
		httpx.JSON(w, 200, orig)
		return
	}
	c := mailSettings{Security: "starttls", Authenticate: true}
	recipient := ""
	test := strings.HasSuffix(r.URL.Path, "/test")
	if test {
		var form struct {
			Recipient string        `json:"test_recipient"`
			SMTP      *mailSettings `json:"smtp"`
		}
		if !httpx.Decode(w, r, &form) {
			return
		}
		recipient = form.Recipient
		c = orig
		if form.SMTP != nil {
			c = *form.SMTP
		}
	} else if !httpx.Decode(w, r, &c) {
		return
	}
	if c.Security == "" {
		c.Security = "starttls"
	}
	if c.Host == "" || c.Port < 1 || c.Port > 65535 || strings.ContainsAny(c.Host, " /\r\n") || (c.Security != "starttls" && c.Security != "ssl" && c.Security != "none") {
		httpx.Error(w, 422, "Invalid SMTP host, port or security")
		return
	}
	if a, err := mail.ParseAddress(c.From); err != nil || a.Address != c.From {
		httpx.Error(w, 422, "Invalid sender address")
		return
	}
	if c.Authenticate && (c.Password == "" || c.Password == "********") {
		if c.Host != orig.Host || c.Port != orig.Port || c.Security != orig.Security || c.Username != orig.Username || orig.Password == "" {
			httpx.Error(w, 422, "Enter the password for this SMTP destination")
			return
		}
		c.Password = orig.Password
	}
	if !c.Authenticate {
		c.Password = ""
	}
	if test {
		if e = c.send(r.Context(), recipient, "Bambastic email test", "Bambastic connected to the configured mail service."); e != nil {
			httpx.JSON(w, 200, object{"success": false, "message": e.Error()})
			return
		}
		httpx.JSON(w, 200, object{"success": true, "message": "Test email sent"})
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	secret := ""
	if c.Password != "" {
		secret = s.SealSecret(c.Password)
	}
	for k, v := range map[string]string{"smtp_host": c.Host, "smtp_port": strconv.Itoa(c.Port), "smtp_username": c.Username, "smtp_password": secret, "smtp_security": c.Security, "smtp_auth_enabled": strconv.FormatBool(c.Authenticate), "smtp_from_name": c.Name, "smtp_from_email": c.From} {
		if e = Put(tx, k, v); e != nil {
			dbfail(w, e)
			return
		}
	}
	if e = tx.Commit(); e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"message": "SMTP settings saved"})
}
func (s *Service) advanced(w http.ResponseWriter, r *http.Request) {
	c, e := s.smtpConfig(r.Context())
	if e != nil {
		dbfail(w, e)
		return
	}
	if r.Method == "GET" {
		enabled, e := Setting(r.Context(), s.DB, "advanced_auth_enabled", "false")
		if e != nil {
			dbfail(w, e)
			return
		}
		local, e := Setting(r.Context(), s.DB, "local_login_enabled", "true")
		if e != nil {
			dbfail(w, e)
			return
		}
		var id *int64
		e = s.DB.QueryRow("SELECT id FROM oidc_providers WHERE is_enabled AND is_autologin LIMIT 1").Scan(&id)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"advanced_auth_enabled": enabled == "true", "smtp_configured": c.Host != "", "local_login_enabled": local == "true", "autologin_provider_id": id})
		return
	}
	if !From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	on := strings.HasSuffix(r.URL.Path, "/enable")
	if on && c.Host == "" {
		httpx.Error(w, 400, "Configure SMTP first")
		return
	}
	tx, e := s.DB.Begin()
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	if e = Put(tx, "advanced_auth_enabled", strconv.FormatBool(on)); e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"message": "Email authentication settings saved", "advanced_auth_enabled": on})
}
func (s *Service) registerMail(public, api *http.ServeMux) {
	for _, p := range []string{"GET /api/v1/auth/smtp", "POST /api/v1/auth/smtp", "POST /api/v1/auth/smtp/test"} {
		api.HandleFunc(p, s.smtpUpdate)
	}
	public.HandleFunc("GET /api/v1/auth/advanced-auth/status", s.advanced)
	for _, p := range []string{"enable", "disable"} {
		api.HandleFunc("POST /api/v1/auth/advanced-auth/"+p, s.advanced)
	}
	public.HandleFunc("POST /api/v1/auth/forgot-password", s.second.limit(s.resetByMail))
	public.HandleFunc("POST /api/v1/auth/forgot-password/confirm", s.second.limit(s.resetByMail))
	api.HandleFunc("POST /api/v1/auth/reset-password", s.second.limit(s.resetByMail))
	for _, p := range []string{"enable", "enable/confirm", "disable"} {
		api.HandleFunc("POST /api/v1/auth/2fa/email/"+p, s.second.limit(s.emailFactor))
	}
	public.HandleFunc("POST /api/v1/auth/2fa/email/send", s.second.limit(s.emailFactor))
}
func (s *Service) resetByMail(w http.ResponseWriter, r *http.Request) {
	v, e := Setting(r.Context(), s.DB, "advanced_auth_enabled", "false")
	if e != nil {
		dbfail(w, e)
		return
	}
	if v != "true" {
		httpx.Error(w, 400, "Email recovery is disabled")
		return
	}
	var in struct {
		Email, Token string
		New          string `json:"new_password"`
		UID          int64  `json:"user_id"`
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/confirm") {
		if !passwordOK(in.New) {
			httpx.Error(w, 422, "Choose a stronger password")
			return
		}
		tx, e := s.DB.BeginTx(r.Context(), nil)
		if e != nil {
			dbfail(w, e)
			return
		}
		defer tx.Rollback()
		var uid int64
		e = tx.QueryRow("DELETE FROM bambastic_email_tokens WHERE hash=? AND purpose='reset' AND expires>unixepoch() AND email=(SELECT email FROM users WHERE id=user_id AND is_active AND auth_source='local') RETURNING user_id", digest(in.Token)).Scan(&uid)
		if e != nil {
			httpx.Error(w, 400, "Invalid or expired reset link")
			return
		}
		_, e = tx.Exec("UPDATE users SET password_hash=?,password_changed_at=CURRENT_TIMESTAMP WHERE id=?", hashPassword(in.New), uid)
		for _, table := range []string{"bambastic_credentials", "bambastic_challenges", "bambastic_email_tokens", "bambastic_mcp_codes", "bambastic_oidc_exchange"} {
			if e == nil {
				_, e = tx.Exec("DELETE FROM "+table+" WHERE user_id=?", uid)
			}
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"message": "Password changed"})
		return
	}
	admin := strings.HasSuffix(r.URL.Path, "/reset-password")
	if admin && !From(r.Context()).Admin {
		httpx.Error(w, 403, "Administrator required")
		return
	}
	var uid int64
	var addr string
	if admin {
		e = s.DB.QueryRow("SELECT id,email FROM users WHERE id=? AND is_active AND auth_source='local' AND email IS NOT NULL", in.UID).Scan(&uid, &addr)
	} else {
		e = s.DB.QueryRow("SELECT id,email FROM users WHERE lower(email)=lower(?) AND is_active AND auth_source='local' LIMIT 1", in.Email).Scan(&uid, &addr)
	}
	if e == nil {
		c, err := s.smtpConfig(r.Context())
		if err != nil {
			dbfail(w, err)
			return
		}
		token := random()
		_, e = s.DB.Exec("INSERT INTO bambastic_email_tokens(hash,user_id,email,purpose,expires) VALUES(?,?,?,'reset',?)", digest(token), uid, addr, time.Now().Add(time.Hour).Unix())
		if e == nil {
			// The requester controls Host. Only the configured installation origin is sent to the account owner.
			base := strings.TrimRight(os.Getenv("BAMBASTIC_PUBLIC_URL"), "/")
			if base == "" {
				base, _ = Setting(r.Context(), s.DB, "external_url", "")
				if base == "" {
					base = httpx.DefaultURL
				}
			}
			u, err := url.Parse(base)
			if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
				httpx.Error(w, 500, "Configure the installation URL")
				return
			}
			e = c.send(r.Context(), addr, "Reset your Bambastic password", "Use this link within one hour to set your password:\n"+strings.TrimRight(base, "/")+"/login#reset_token="+token)
		}
		if e != nil {
			s.DB.Exec("DELETE FROM bambastic_email_tokens WHERE hash=?", digest(token))
		}
	}
	if admin && e != nil {
		httpx.Error(w, 400, "Could not send a reset link to this account")
		return
	}
	httpx.JSON(w, 200, object{"message": "If the account is eligible, a reset link will arrive by email."})
}
func (s *Service) emailFactor(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	send := strings.HasSuffix(r.URL.Path, "/send")
	if !send && (p.ID == 0 || p.KeyID != 0) {
		httpx.Error(w, 403, "Account session required")
		return
	}
	var in struct {
		Setup          string `json:"setup_token"`
		Pre            string `json:"pre_auth_token"`
		Code, Password string
	}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) {
		return
	}
	if strings.HasSuffix(r.URL.Path, "/disable") {
		if _, e := s.Authenticate(r, p.Username, in.Password); e != nil {
			httpx.Error(w, 403, "Current account password required")
			return
		}
		if _, e := s.DB.Exec("DELETE FROM settings WHERE key=?", fmt.Sprintf("user_%d_email_2fa_enabled", p.ID)); e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"message": "Email verification disabled"})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/confirm") {
		tx, e := s.DB.BeginTx(r.Context(), nil)
		if e != nil {
			dbfail(w, e)
			return
		}
		defer tx.Rollback()
		ok, e := consumeEmail(tx, p.ID, in.Setup, "enroll", in.Code)
		if e == nil && ok {
			e = Put(tx, fmt.Sprintf("user_%d_email_2fa_enabled", p.ID), "true")
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			dbfail(w, e)
			return
		}
		if !ok {
			httpx.Error(w, 400, "Invalid verification code")
			return
		}
		httpx.JSON(w, 200, object{"message": "Email verification enabled"})
		return
	}
	var uid int64
	token := random()
	purpose := "enroll"
	if send {
		c, e := r.Cookie("2fa_challenge")
		if e != nil {
			httpx.Error(w, 401, "Verification expired")
			return
		}
		e = s.DB.QueryRow("SELECT user_id FROM bambastic_challenges WHERE hash=? AND cookie=? AND attempts<5 AND expires>unixepoch()", digest(in.Pre), digest(c.Value)).Scan(&uid)
		if e != nil {
			httpx.Error(w, 401, "Verification expired")
			return
		}
		enabled, e := Setting(r.Context(), s.DB, fmt.Sprintf("user_%d_email_2fa_enabled", uid), "false")
		if e != nil || enabled != "true" {
			httpx.Error(w, 401, "Email verification unavailable")
			return
		}
		token = in.Pre
		purpose = "login"
	} else {
		uid = p.ID
	}
	var email string
	e := s.DB.QueryRow("SELECT email FROM users WHERE id=? AND is_active AND email IS NOT NULL", uid).Scan(&email)
	if e != nil {
		httpx.Error(w, 400, "Set an account email address first")
		return
	}
	c, e := s.smtpConfig(r.Context())
	if e != nil || c.Host == "" {
		httpx.Error(w, 400, "Configure SMTP first")
		return
	}
	n, e := rand.Int(rand.Reader, big.NewInt(1000000))
	if e != nil {
		dbfail(w, e)
		return
	}
	code := fmt.Sprintf("%06d", n.Int64())
	// The secret random token is required with the short code for validation.
	_, e = s.DB.Exec(`INSERT INTO bambastic_email_tokens(hash,user_id,email,purpose,code,expires) VALUES(?,?,?,?,?,?) ON CONFLICT(hash) DO UPDATE SET code=excluded.code,email=excluded.email,expires=excluded.expires`, digest(token), uid, email, purpose, digest(token+code), time.Now().Add(5*time.Minute).Unix())
	if e != nil {
		dbfail(w, e)
		return
	}
	if e = c.send(r.Context(), email, "Your Bambastic verification code", "Your verification code is "+code+". It expires in five minutes."); e != nil {
		s.DB.Exec("DELETE FROM bambastic_email_tokens WHERE hash=?", digest(token))
		httpx.Error(w, 502, "Mail delivery failed")
		return
	}
	out := object{"message": "Verification code sent"}
	if send {
		out["pre_auth_token"] = token
	} else {
		out["setup_token"] = token
	}
	httpx.JSON(w, 200, out)
}
func consumeEmail(tx *sql.Tx, uid int64, token, purpose, code string) (bool, error) {
	var want string
	e := tx.QueryRow(`UPDATE bambastic_email_tokens SET attempts=attempts+1 WHERE hash=? AND user_id=? AND purpose=? AND attempts<5 AND expires>unixepoch() AND email=(SELECT email FROM users WHERE id=? AND is_active) RETURNING code`, digest(token), uid, purpose, uid).Scan(&want)
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if subtle.ConstantTimeCompare([]byte(digest(token+code)), []byte(want)) != 1 {
		return false, nil
	}
	_, e = tx.Exec("DELETE FROM bambastic_email_tokens WHERE hash=?", digest(token))
	return e == nil, e
}

// SendAccountEmail uses the installation's SMTP identity for operator mail.
func (s *Service) SendAccountEmail(ctx context.Context, to, subject, body string) error {
	config, e := s.smtpConfig(ctx)
	if e != nil {
		return e
	}
	return config.send(ctx, to, subject, body)
}
