package access

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bambastic/internal/httpx"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/time/rate"
)

type loginBucket struct {
	limiter *rate.Limiter
	seen    time.Time
}
type secondFactor struct {
	seal     cipher.AEAD
	mu       sync.Mutex
	attempts map[string]loginBucket
	cleaned  time.Time
}

func newFactor(tx *sql.Tx, dir string) (*secondFactor, error) {
	path := filepath.Join(dir, "identity.key")
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		err = os.WriteFile(path, key, 0600)
	}
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	seal, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`CREATE TABLE IF NOT EXISTS bambastic_connect_codes(hash TEXT PRIMARY KEY,app_id INTEGER NOT NULL REFERENCES connected_apps(id) ON DELETE CASCADE,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,redirect TEXT NOT NULL,challenge TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_email_tokens(hash TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,email TEXT NOT NULL,purpose TEXT NOT NULL,code TEXT,expires INTEGER NOT NULL,attempts INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS bambastic_oidc_state(hash TEXT PRIMARY KEY,provider_id INTEGER NOT NULL REFERENCES oidc_providers(id) ON DELETE CASCADE,nonce TEXT NOT NULL,verifier TEXT NOT NULL,browser TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_oidc_exchange(hash TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,browser TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_totp_pending(user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,secret TEXT NOT NULL,expires INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS bambastic_challenges(hash TEXT PRIMARY KEY,user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,cookie TEXT NOT NULL,expires INTEGER NOT NULL,attempts INTEGER NOT NULL DEFAULT 0);`)
	if err != nil {
		return nil, err
	}
	factor := &secondFactor{seal: seal, attempts: make(map[string]loginBucket)}
	return factor, factor.importSecrets(tx, dir)
}
func (f *secondFactor) limit(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			host = r.RemoteAddr
		}
		now := time.Now()
		f.mu.Lock()
		if now.Sub(f.cleaned) > time.Minute {
			for k, v := range f.attempts {
				if now.Sub(v.seen) > 10*time.Minute {
					delete(f.attempts, k)
				}
			}
			f.cleaned = now
		}
		key := r.URL.Path + " " + host
		b := f.attempts[key]
		if b.limiter == nil {
			b.limiter = rate.NewLimiter(rate.Every(3*time.Second), 15)
		}
		b.seen = now
		f.attempts[key] = b
		ok := b.limiter.Allow()
		f.mu.Unlock()
		if !ok {
			w.Header().Set("Retry-After", "3")
			httpx.Error(w, 429, "Too many requests")
			return
		}
		h(w, r)
	}
}
func (f *secondFactor) encrypt(v string) string {
	return "bambastic:" + base64.RawStdEncoding.EncodeToString(f.seal.Seal(nil, nil, []byte(v), nil))
}
func (f *secondFactor) decrypt(v string) (string, error) {
	if !strings.HasPrefix(v, "bambastic:") {
		_, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(v)
		return v, err
	}
	data, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(v, "bambastic:"))
	if err != nil {
		return "", err
	}
	plain, err := f.seal.Open(nil, nil, data, nil)
	return string(plain), err
}
func (f *secondFactor) match(secret, code string, last int64) (int64, bool) {
	plain, err := f.decrypt(secret)
	if err != nil {
		return 0, false
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(plain)
	if err != nil || len(key) == 0 || len(code) != 6 {
		return 0, false
	}
	current := time.Now().Unix() / 30
	for i := current - 1; i <= current+1; i++ {
		if i <= last {
			continue
		}
		h := hmac.New(sha1.New, key)
		var stamp [8]byte
		binary.BigEndian.PutUint64(stamp[:], uint64(i))
		h.Write(stamp[:])
		v := h.Sum(nil)
		idx := v[len(v)-1] & 15
		n := binary.BigEndian.Uint32(v[idx:idx+4]) & 0x7fffffff
		if subtle.ConstantTimeCompare([]byte(fmt.Sprintf("%06d", n%1000000)), []byte(code)) == 1 {
			return i, true
		}
	}
	return 0, false
}
func newCodes() ([]string, string) {
	codes := make([]string, 10)
	hashes := make([]string, 10)
	for i := range codes {
		var b [10]byte
		rand.Read(b[:])
		v := strings.ToUpper(hex.EncodeToString(b[:]))
		codes[i] = v[:10] + "-" + v[10:]
		hashes[i] = recoveryDigest(codes[i])
	}
	v, _ := json.Marshal(hashes)
	return codes, string(v)
}
func recoveryDigest(v string) string {
	sum := sha256.Sum256([]byte(strings.ToUpper(strings.TrimSpace(v))))
	return hex.EncodeToString(sum[:])
}

func (s *Service) registerSecondFactor(public, api *http.ServeMux) {
	public.HandleFunc("POST /api/v1/auth/2fa/verify", s.second.limit(s.verifyChallenge))
	api.HandleFunc("GET /api/v1/auth/2fa/status", s.factorStatus)
	for _, name := range []string{"setup", "enable", "disable", "regenerate-backup-codes"} {
		api.HandleFunc("POST /api/v1/auth/2fa/totp/"+name, s.second.limit(func(w http.ResponseWriter, r *http.Request) { s.manageFactor(w, r, name) }))
	}
}
func (s *Service) factorStatus(w http.ResponseWriter, r *http.Request) {
	p := From(r.Context())
	if p.KeyID != 0 {
		httpx.Error(w, 403, "Account session required")
		return
	}
	var enabled bool
	var hashes string
	err := s.DB.QueryRow("SELECT is_enabled,coalesce(backup_codes_json,'[]') FROM user_totp WHERE user_id=?", p.ID).Scan(&enabled, &hashes)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		dbfail(w, err)
		return
	}
	v, err := Setting(r.Context(), s.DB, fmt.Sprintf("user_%d_email_2fa_enabled", p.ID), "false")
	if err != nil {
		dbfail(w, err)
		return
	}
	var codes []string
	json.Unmarshal([]byte(hashes), &codes)
	httpx.JSON(w, 200, object{"totp_enabled": enabled, "email_otp_enabled": v == "true", "backup_codes_remaining": len(codes)})
}
func (s *Service) manageFactor(w http.ResponseWriter, r *http.Request, action string) {
	p := From(r.Context())
	if p.ID == 0 || p.KeyID != 0 {
		httpx.Error(w, 403, "Account session required")
		return
	}
	var body struct{ Code string }
	if r.ContentLength != 0 && !httpx.Decode(w, r, &body) {
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		dbfail(w, err)
		return
	}
	defer tx.Rollback()
	var secret string
	var last int64
	err = tx.QueryRow("SELECT secret,coalesce(last_totp_counter,-1) FROM user_totp WHERE user_id=? AND is_enabled", p.ID).Scan(&secret, &last)
	active := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		dbfail(w, err)
		return
	}
	if action != "enable" && (active || action != "setup") {
		n, ok := s.second.match(secret, body.Code, last)
		if !active || !ok {
			httpx.Error(w, 400, "Current authenticator code required")
			return
		}
		if _, err = tx.Exec("UPDATE user_totp SET last_totp_counter=? WHERE user_id=?", n, p.ID); err != nil {
			dbfail(w, err)
			return
		}
	}
	result := object{}
	switch action {
	case "setup":
		var raw [20]byte
		rand.Read(raw[:])
		secret = base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw[:])
		uri := url.URL{Scheme: "otpauth", Host: "totp", Path: "/Bambastic:" + p.Username, RawQuery: url.Values{"secret": {secret}, "issuer": {"Bambastic"}}.Encode()}
		qr, e := qrcode.Encode(uri.String(), qrcode.Medium, 256)
		if e != nil {
			dbfail(w, e)
			return
		}
		_, err = tx.Exec("INSERT INTO bambastic_totp_pending VALUES(?,?,?) ON CONFLICT(user_id) DO UPDATE SET secret=excluded.secret,expires=excluded.expires", p.ID, s.second.encrypt(secret), time.Now().Add(10*time.Minute).Unix())
		result = object{"secret": secret, "issuer": "Bambastic", "qr_code_b64": base64.StdEncoding.EncodeToString(qr)}
	case "enable":
		err = tx.QueryRow("DELETE FROM bambastic_totp_pending WHERE user_id=? AND expires>unixepoch() RETURNING secret", p.ID).Scan(&secret)
		n, ok := s.second.match(secret, body.Code, -1)
		if err != nil || !ok {
			httpx.Error(w, 400, "Enrollment expired or authenticator code invalid")
			return
		}
		codes, hashes := newCodes()
		_, err = tx.Exec(`INSERT INTO user_totp(user_id,secret,is_enabled,backup_codes_json,last_totp_counter) VALUES(?,?,1,?,?) ON CONFLICT(user_id) DO UPDATE SET secret=excluded.secret,is_enabled=1,backup_codes_json=excluded.backup_codes_json,last_totp_counter=excluded.last_totp_counter`, p.ID, secret, hashes, n)
		result = object{"message": "Authenticator enabled. Store your recovery codes.", "backup_codes": codes}
	case "regenerate-backup-codes":
		codes, hashes := newCodes()
		_, err = tx.Exec("UPDATE user_totp SET backup_codes_json=? WHERE user_id=?", hashes, p.ID)
		result = object{"message": "Recovery codes replaced", "backup_codes": codes}
	case "disable":
		_, err = tx.Exec("DELETE FROM user_totp WHERE user_id=?", p.ID)
		if err == nil {
			_, err = tx.Exec("DELETE FROM bambastic_totp_pending WHERE user_id=?", p.ID)
		}
		result = object{"message": "Authenticator disabled"}
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		dbfail(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, result)
}
func (s *Service) challengeLogin(w http.ResponseWriter, r *http.Request, p Principal) bool {
	var totp bool
	err := s.DB.QueryRow("SELECT EXISTS(SELECT 1 FROM user_totp WHERE user_id=? AND is_enabled)", p.ID).Scan(&totp)
	if err != nil {
		dbfail(w, err)
		return true
	}
	email, err := Setting(r.Context(), s.DB, fmt.Sprintf("user_%d_email_2fa_enabled", p.ID), "false")
	if err != nil {
		dbfail(w, err)
		return true
	}
	methods := []string{}
	if totp {
		methods = append(methods, "totp")
	}
	if strings.EqualFold(email, "true") {
		methods = append(methods, "email")
	}
	if totp {
		methods = append(methods, "backup")
	}
	if len(methods) == 0 {
		return false
	}
	cookie, token := random(), random()
	if _, err = s.DB.Exec("DELETE FROM bambastic_challenges WHERE expires<=unixepoch()"); err == nil {
		_, err = s.DB.Exec("INSERT INTO bambastic_challenges(hash,user_id,cookie,expires) VALUES(?,?,?,?)", digest(token), p.ID, digest(cookie), time.Now().Add(5*time.Minute).Unix())
	}
	if err != nil {
		dbfail(w, err)
		return true
	}
	http.SetCookie(w, &http.Cookie{Name: "2fa_challenge", Value: cookie, Path: "/api/v1/auth/2fa", MaxAge: 300, HttpOnly: true, Secure: strings.HasPrefix(origin(r), "https://"), SameSite: http.SameSiteLaxMode})
	w.Header().Set("Cache-Control", "no-store")
	httpx.JSON(w, 200, object{"requires_2fa": true, "pre_auth_token": token, "two_fa_methods": methods})
	return true
}
func (s *Service) verifyChallenge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Token        string `json:"pre_auth_token"`
		Method, Code string
	}
	if !httpx.Decode(w, r, &in) {
		return
	}
	deny := func() { httpx.Error(w, 401, "Invalid or expired verification") }
	cookie, err := r.Cookie("2fa_challenge")
	if err != nil {
		deny()
		return
	}
	tx, err := s.DB.BeginTx(r.Context(), nil)
	if err != nil {
		dbfail(w, err)
		return
	}
	defer tx.Rollback()
	var uid, last int64
	var secret, hashes string
	err = tx.QueryRow("UPDATE bambastic_challenges SET attempts=attempts+1 WHERE hash=? AND cookie=? AND expires>unixepoch() AND attempts<5 RETURNING user_id", digest(in.Token), digest(cookie.Value)).Scan(&uid)
	if err != nil {
		deny()
		return
	}
	if in.Method != "email" {
		err = tx.QueryRow("SELECT secret,coalesce(last_totp_counter,-1),coalesce(backup_codes_json,'[]') FROM user_totp WHERE user_id=? AND is_enabled", uid).Scan(&secret, &last, &hashes)
		if err != nil {
			deny()
			return
		}
	}
	ok := false
	switch in.Method {
	case "email":
		ok, err = consumeEmail(tx, uid, in.Token, "login", in.Code)
	case "", "totp":
		n, matched := s.second.match(secret, in.Code, last)
		if matched {
			_, err = tx.Exec("UPDATE user_totp SET last_totp_counter=? WHERE user_id=?", n, uid)
			ok = true
		}
	case "backup":
		var list []string
		json.Unmarshal([]byte(hashes), &list)
		hash := recoveryDigest(in.Code)
		for i, v := range list {
			if subtle.ConstantTimeCompare([]byte(v), []byte(hash)) == 1 || strings.HasPrefix(v, "$pbkdf2-sha256$") && verify(strings.ToUpper(strings.TrimSpace(in.Code)), v) {
				list = append(list[:i], list[i+1:]...)
				b, _ := json.Marshal(list)
				_, err = tx.Exec("UPDATE user_totp SET backup_codes_json=? WHERE user_id=?", string(b), uid)
				ok = true
				break
			}
		}
	}
	if err != nil {
		dbfail(w, err)
		return
	}
	if ok {
		_, err = tx.Exec("DELETE FROM bambastic_challenges WHERE hash=?", digest(in.Token))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		dbfail(w, err)
		return
	}
	if !ok {
		deny()
		return
	}
	p, err := s.User(r.Context(), uid)
	if err != nil {
		deny()
		return
	}
	s.completeLogin(w, r, p)
}
