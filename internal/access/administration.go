package access

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"bambastic/internal/httpx"
)

func (s *Service) adminRoutes(api *http.ServeMux) {
	for _, m := range []string{"POST", "DELETE"} {
		api.HandleFunc(m+" /api/v1/groups/{group}/users/{user}", s.member)
	}
	api.HandleFunc("GET /api/v1/users/{user}/items-count", s.itemCount)
	api.HandleFunc("GET /api/v1/auth/encryption-status", s.encryption)
	api.HandleFunc("DELETE /api/v1/auth/2fa/admin/{user}", s.second.limit(s.resetFactor))
	api.HandleFunc("GET /api/v1/user-notifications/preferences", s.mailPreferences)
	api.HandleFunc("PUT /api/v1/user-notifications/preferences", s.mailPreferences)
}
func (s *Service) member(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	g, u := r.PathValue("group"), r.PathValue("user")
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var user, group string
	e = tx.QueryRow("SELECT u.username,g.name FROM users u CROSS JOIN groups g WHERE u.id=? AND g.id=?", u, g).Scan(&user, &group)
	if e != nil {
		dbfail(w, e)
		return
	}
	if r.Method == "POST" {
		_, e = tx.Exec("INSERT OR IGNORE INTO user_groups(user_id,group_id) VALUES(?,?)", u, g)
	} else {
		_, e = tx.Exec("DELETE FROM user_groups WHERE user_id=? AND group_id=?", u, g)
	}
	if e != nil {
		dbfail(w, e)
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
	httpx.JSON(w, 200, object{"message": "Group membership updated", "username": user, "group": group})
}
func keepAdmin(r *http.Request, tx *sql.Tx) bool {
	enabled, e := Setting(r.Context(), tx, "auth_enabled", "false")
	if e != nil {
		return false
	}
	if enabled != "true" {
		return true
	}
	var n int
	e = tx.QueryRow(`SELECT count(*) FROM users u WHERE u.is_active AND (u.role='admin' OR EXISTS(SELECT 1 FROM user_groups ug JOIN groups g ON ug.group_id=g.id WHERE ug.user_id=u.id AND g.name='Administrators' AND g.is_system))`).Scan(&n)
	return e == nil && n > 0
}
func (s *Service) itemCount(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	var n int
	e := s.DB.QueryRow("SELECT count(*) FROM users WHERE id=?", r.PathValue("user")).Scan(&n)
	if e != nil || n == 0 {
		httpx.Error(w, 404, "Unknown account")
		return
	}
	out := object{}
	for k, t := range map[string]string{"archives": "print_archives", "queue_items": "print_queue", "library_files": "library_files"} {
		e = s.DB.QueryRow("SELECT count(*) FROM "+t+" WHERE created_by_id=?", r.PathValue("user")).Scan(&n)
		if e != nil {
			dbfail(w, e)
			return
		}
		out[k] = n
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) encryption(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	rows, e := s.DB.Query("SELECT 'user_totp', secret FROM user_totp WHERE secret IS NOT NULL UNION ALL SELECT 'oidc_providers',client_secret FROM oidc_providers WHERE client_secret IS NOT NULL")
	if e != nil {
		dbfail(w, e)
		return
	}
	defer rows.Close()
	legacy := map[string]int{"user_totp": 0, "oidc_providers": 0}
	encrypted := map[string]int{"user_totp": 0, "oidc_providers": 0}
	broken := 0
	for rows.Next() {
		var source, val string
		if e := rows.Scan(&source, &val); e != nil {
			dbfail(w, e)
			return
		}
		if strings.HasPrefix(val, "bambastic:") || strings.HasPrefix(val, "fernet:") {
			encrypted[source]++
			if _, e := s.OpenSecret(val); e != nil {
				broken++
			}
		} else if val != "" {
			legacy[source]++
		}
	}
	if e = rows.Err(); e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"key_configured": true, "key_source": "file", "legacy_plaintext_rows": legacy, "encrypted_rows": encrypted, "decryption_broken": broken > 0, "migration_error_count": broken})
}
func revokeLogin(tx *sql.Tx, uid any) error {
	for _, table := range []string{"bambastic_credentials", "bambastic_challenges", "bambastic_mcp_codes", "bambastic_connect_codes", "bambastic_oidc_exchange", "bambastic_email_tokens"} {
		if _, e := tx.Exec("DELETE FROM "+table+" WHERE user_id=?", uid); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) resetFactor(w http.ResponseWriter, r *http.Request) {
	if !admin(w, r) {
		return
	}
	p := From(r.Context())
	if p.KeyID != 0 {
		httpx.Error(w, 403, "Account session required")
		return
	}
	var in struct {
		Password string `json:"admin_password"`
	}
	if r.ContentLength != 0 && !httpx.Decode(w, r, &in) {
		return
	}
	if p.ID > 0 {
		if _, e := s.Authenticate(r, p.Username, in.Password); e != nil {
			httpx.Error(w, 403, "Confirm the administrator password")
			return
		}
	}
	uid := r.PathValue("user")
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var count int
	e = tx.QueryRow("SELECT count(*) FROM users WHERE id=?", uid).Scan(&count)
	if e != nil || count == 0 {
		httpx.Error(w, 404, "Unknown account")
		return
	}
	email, e := Setting(r.Context(), tx, "user_"+uid+"_email_2fa_enabled", "false")
	if e != nil {
		dbfail(w, e)
		return
	}
	res, e := tx.Exec("DELETE FROM user_totp WHERE user_id=?", uid)
	if e != nil {
		dbfail(w, e)
		return
	}
	count64, _ := res.RowsAffected()
	if _, e = tx.Exec("DELETE FROM bambastic_totp_pending WHERE user_id=?", uid); e == nil {
		_, e = tx.Exec("DELETE FROM settings WHERE key=?", "user_"+uid+"_email_2fa_enabled")
	}
	if e == nil {
		e = revokeLogin(tx, uid)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		dbfail(w, e)
		return
	}
	httpx.JSON(w, 200, object{"had_2fa": email == "true" || count64 > 0, "message": "Second-factor enrollment reset"})
}
func (s *Service) mailPreferences(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.KeyID != 0 {
		httpx.Error(w, 403, "Account session required")
		return
	}
	prefs := object{"notify_print_start": true, "notify_print_complete": true, "notify_print_failed": true, "notify_print_stopped": true}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		dbfail(w, e)
		return
	}
	defer tx.Rollback()
	var a, b, c, d bool
	e = tx.QueryRow("SELECT notify_print_start,notify_print_complete,notify_print_failed,notify_print_stopped FROM user_email_preferences WHERE user_id=?", p.ID).Scan(&a, &b, &c, &d)
	if e == nil {
		prefs = object{"notify_print_start": a, "notify_print_complete": b, "notify_print_failed": c, "notify_print_stopped": d}
	} else if !errors.Is(e, sql.ErrNoRows) {
		dbfail(w, e)
		return
	}
	if r.Method == "PUT" {
		if p.ID == 0 {
			httpx.Error(w, 403, "Sign in to save email preferences")
			return
		}
		var in map[string]bool
		if !httpx.Decode(w, r, &in) {
			return
		}
		for k, v := range in {
			if _, known := prefs[k]; known {
				prefs[k] = v
			}
		}
		_, e = tx.Exec(`INSERT INTO user_email_preferences(user_id,notify_print_start,notify_print_complete,notify_print_failed,notify_print_stopped) VALUES(?,?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET notify_print_start=excluded.notify_print_start,notify_print_complete=excluded.notify_print_complete,notify_print_failed=excluded.notify_print_failed,notify_print_stopped=excluded.notify_print_stopped`, p.ID, prefs["notify_print_start"], prefs["notify_print_complete"], prefs["notify_print_failed"], prefs["notify_print_stopped"])
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			dbfail(w, e)
			return
		}
	}
	httpx.JSON(w, 200, prefs)
}
