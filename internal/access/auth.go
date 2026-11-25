package access

import (
	"bambastic/internal/httpx"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
)

type object = map[string]any

var b64 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789./").WithPadding(base64.NoPadding)

func hashPassword(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	const rounds = 600000
	out, _ := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	return fmt.Sprintf("$pbkdf2-sha256$%d$%s$%s", rounds, b64.EncodeToString(salt), b64.EncodeToString(out))
}
func verify(password, hash string) bool {
	v := strings.Split(hash, "$")
	if len(v) != 5 || v[1] != "pbkdf2-sha256" || len(password) > 1024 {
		return false
	}
	rounds, e := strconv.Atoi(v[2])
	if e != nil || rounds < 1 || rounds > 2000000 {
		return false
	}
	salt, e := b64.DecodeString(v[3])
	if e != nil {
		return false
	}
	want, e := b64.DecodeString(v[4])
	if e != nil || len(want) != 32 {
		return false
	}
	got, e := pbkdf2.Key(sha256.New, password, salt, rounds, 32)
	return e == nil && subtle.ConstantTimeCompare(want, got) == 1
}
func passwordOK(v string) bool {
	if len(v) < 8 || len(v) > 256 {
		return false
	}
	var up, lo, num, other bool
	for _, c := range v {
		switch {
		case unicode.IsUpper(c):
			up = true
		case unicode.IsLower(c):
			lo = true
		case unicode.IsDigit(c):
			num = true
		default:
			other = true
		}
	}
	return up && lo && num && other
}
func Require(w http.ResponseWriter, r *http.Request, perm string) bool {
	if From(r.Context()).Has(perm) {
		return true
	}
	httpx.Error(w, 403, "Insufficient permission")
	return false
}
func dbfail(w http.ResponseWriter, e error) {
	if errors.Is(e, sql.ErrNoRows) {
		httpx.Error(w, 404, "Not found")
		return
	}
	httpx.Error(w, 500, "Database operation failed")
}
func (s *Service) Register(public, api *http.ServeMux) {
	public.HandleFunc("GET /api/v1/auth/status", s.status)
	public.HandleFunc("POST /api/v1/auth/setup", s.setup)
	s.registerSecondFactor(public, api)
	s.registerOIDC(public, api)
	s.registerLDAP(public, api)
	s.registerMail(public, api)
	s.connectRoutes(public, api)
	s.adminRoutes(api)
	public.HandleFunc("POST /api/v1/auth/login", s.second.limit(s.login))
	api.HandleFunc("GET /api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) { s.respondUser(w, r, From(r.Context())) })
	api.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		h := digest(Credential(r))
		_, e := s.DB.Exec("DELETE FROM bambastic_credentials WHERE hash=? OR parent_hash=?", h, h)
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"message": "Logged out"})
	})
	for path, kind := range map[string]string{"ws-token": "ws", "media-token": "media"} {
		api.HandleFunc("POST /api/v1/auth/"+path, func(w http.ResponseWriter, r *http.Request) {
			p := From(r.Context())
			if kind == "ws" && !Require(w, r, "websocket:connect") {
				return
			}
			token, e := s.Mint(r.Context(), p, kind, time.Hour, Credential(r))
			if e != nil {
				dbfail(w, e)
				return
			}
			httpx.JSON(w, 200, object{"token": token})
		})
	}
	api.HandleFunc("POST /api/v1/auth/disable", func(w http.ResponseWriter, r *http.Request) {
		if !From(r.Context()).Admin {
			httpx.Error(w, 403, "Administrator required")
			return
		}
		tx, e := s.DB.Begin()
		if e != nil {
			dbfail(w, e)
			return
		}
		defer tx.Rollback()
		if e = Put(tx, "auth_enabled", "false"); e == nil {
			_, e = tx.Exec("DELETE FROM bambastic_credentials")
		}
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			dbfail(w, e)
			return
		}
		httpx.JSON(w, 200, object{"auth_enabled": false})
	})
	s.registerDirectory(api)
	s.registerKeys(api)
}
func (s *Service) status(w http.ResponseWriter, r *http.Request) {
	on, e := s.Enabled(r.Context())
	if e != nil {
		dbfail(w, e)
		return
	}
	done, e := Setting(r.Context(), s.DB, "setup_completed", "false")
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"auth_enabled": on, "requires_setup": done != "true"})
}
func (s *Service) setup(w http.ResponseWriter, r *http.Request) {
	var v struct {
		Enabled  *bool  `json:"auth_enabled"`
		User     string `json:"admin_username"`
		Password string `json:"admin_password"`
	}
	if !httpx.Decode(w, r, &v) {
		return
	}
	if v.Enabled == nil {
		httpx.Error(w, 422, "auth_enabled is required")
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	on, e := Setting(r.Context(), tx, "auth_enabled", "false")
	if e != nil {
		dbfail(w, e)
		return
	}
	if on == "true" {
		httpx.Error(w, 403, "Authentication already configured")
		return
	}
	created := false
	if *v.Enabled {
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM users WHERE role='admin' AND is_active").Scan(&n); e != nil {
			dbfail(w, e)
			return
		}
		if n == 0 {
			if strings.TrimSpace(v.User) == "" || !passwordOK(v.Password) {
				httpx.Error(w, 400, "Provide an administrator name and a password of 8 or more characters with uppercase, lowercase, a digit and a symbol")
				return
			}
			res, e := tx.Exec("INSERT INTO users(username,password_hash,role,auth_source,is_active) VALUES(?,?,'admin','local',1)", v.User, hashPassword(v.Password))
			if e != nil {
				httpx.Error(w, 409, "Username unavailable")
				return
			}
			id, _ := res.LastInsertId()
			if _, e = tx.Exec("INSERT INTO user_groups SELECT ?,id FROM groups WHERE name='Administrators'", id); e != nil {
				dbfail(w, e)
				return
			}
			created = true
		}
		if _, e = tx.Exec("DELETE FROM bambastic_credentials WHERE user_id IS NULL"); e != nil {
			dbfail(w, e)
			return
		}
	}
	if e = Put(tx, "auth_enabled", strconv.FormatBool(*v.Enabled)); e == nil {
		e = Put(tx, "setup_completed", "true")
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"auth_enabled": *v.Enabled, "admin_created": created})
}
func (s *Service) Authenticate(r *http.Request, user, pass string) (Principal, error) {
	var id int64
	var hash string

	e := s.DB.QueryRowContext(r.Context(), `SELECT u.id,COALESCE(u.password_hash,'') FROM users u WHERE u.username=? AND u.auth_source='local' AND u.is_active`, user).Scan(&id, &hash)
	if errors.Is(e, sql.ErrNoRows) {
		return s.ldapAuthenticate(r, user, pass)
	}
	if e != nil || !verify(pass, hash) {
		return Principal{}, errors.New("invalid credentials or additional authentication required")
	}
	return s.User(r.Context(), id)
}
func (s *Service) login(w http.ResponseWriter, r *http.Request) {
	var in struct{ Username, Password string }
	if !httpx.Decode(w, r, &in) {
		return
	}
	p, e := s.Authenticate(r, in.Username, in.Password)
	if e != nil {
		httpx.Error(w, 401, "Invalid credentials or additional authentication required")
		return
	}
	if s.challengeLogin(w, r, p) {
		return
	}
	s.completeLogin(w, r, p)
}
func (s *Service) completeLogin(w http.ResponseWriter, r *http.Request, p Principal) {
	user, e := s.userView(r, p)
	if e != nil {
		dbfail(w, e)
		return
	}
	token, e := s.Mint(r.Context(), p, "session", 24*time.Hour, "")
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"access_token": token, "token_type": "bearer", "user": user, "requires_2fa": false, "two_fa_methods": []string{}})
}
func (s *Service) userView(r *http.Request, p Principal) (object, error) {
	role := "user"
	if p.Admin {
		role = "admin"
	}
	v := object{"id": p.ID, "username": p.Username, "role": role, "is_admin": p.Admin, "permissions": []string{}, "groups": []object{}, "is_active": true, "auth_source": "local", "email": nil, "created_at": ""}
	list := []string{}
	if p.Admin {
		list = Groups()["Administrators"].Permissions
	} else {
		for k := range p.Permissions {
			list = append(list, k)
		}
	}
	sort.Strings(list)
	v["permissions"] = list
	if p.ID != 0 {
		var email sql.NullString
		var created time.Time
		var src string
		if e := s.DB.QueryRowContext(r.Context(), "SELECT email,created_at,auth_source FROM users WHERE id=?", p.ID).Scan(&email, &created, &src); e != nil {
			return nil, e
		}
		if email.Valid {
			v["email"] = email.String
		}
		v["created_at"] = created
		v["auth_source"] = src
		rows, e := s.DB.QueryContext(r.Context(), "SELECT g.id,g.name FROM groups g JOIN user_groups u ON g.id=u.group_id WHERE u.user_id=? ORDER BY g.id", p.ID)
		if e != nil {
			return nil, e
		}
		v["groups"], e = httpx.Rows(rows)
		if e != nil {
			return nil, e
		}
	}
	return v, nil
}
func (s *Service) respondUser(w http.ResponseWriter, r *http.Request, p Principal) {
	v, e := s.userView(r, p)
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, v)
}

// VerifyPassword checks an imported passlib credential using its encoded cost and salt.
func VerifyPassword(password, encoded string) bool { return verify(password, encoded) }
