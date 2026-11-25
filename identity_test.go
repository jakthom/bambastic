package main

import (
	"archive/zip"
	"bambastic/internal/backup"
	"bufio"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Exercise the OIDC callback with a signed ID token, real discovery and JWKS.
func TestFederatedSignIn(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var nonce string
	var lock sync.Mutex
	var issuer *httptest.Server
	enc := base64.RawURLEncoding.EncodeToString
	issuer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{"issuer": issuer.URL, "authorization_endpoint": issuer.URL + "/authorize", "token_endpoint": issuer.URL + "/token", "jwks_uri": issuer.URL + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
		case "/keys":
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{"kty": "RSA", "alg": "RS256", "kid": "a", "use": "sig", "n": enc(key.N.Bytes()), "e": enc(big.NewInt(int64(key.E)).Bytes())}}})
		case "/token":
			lock.Lock()
			once := nonce
			lock.Unlock()
			claims, _ := json.Marshal(map[string]any{"iss": issuer.URL, "sub": "printer-tech", "aud": "workshop", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(), "nonce": once, "email": "avery@example.test", "email_verified": true, "preferred_username": "avery"})
			part := enc([]byte(`{"alg":"RS256","kid":"a"}`)) + "." + enc(claims)
			hash := sha256.Sum256([]byte(part))
			sig, _ := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, hash[:])
			json.NewEncoder(w).Encode(map[string]string{"access_token": "test-token", "token_type": "bearer", "id_token": part + "." + enc(sig)})
		default:
			http.NotFound(w, r)
		}
	}))
	defer issuer.Close()
	s := startTestSystem(t)
	client := s.Client()
	client.Jar, _ = cookiejar.New(nil)
	admin := s.install()
	form, _ := json.Marshal(map[string]any{"name": "Workshop", "issuer_url": issuer.URL, "client_id": "workshop", "client_secret": "test-secret", "auto_create_users": true})
	v := s.json("POST", "/api/v1/auth/oidc/providers", string(form), admin, 201)
	if _, ok := v["client_secret"]; ok {
		t.Fatal("provider response exposed secret")
	}
	start := s.json("GET", "/api/v1/auth/oidc/authorize/1", "", "", 200)
	authURL, err := url.Parse(start["auth_url"].(string))
	if err != nil {
		t.Fatal(err)
	}
	query := authURL.Query()
	lock.Lock()
	nonce = query.Get("nonce")
	lock.Unlock()
	if query.Get("code_challenge") == "" {
		t.Fatal("missing PKCE challenge")
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	path := "/api/v1/auth/oidc/callback?state=" + url.QueryEscape(query.Get("state")) + "&code=sample"
	_, h := s.call("GET", path, "", "", "", 302)
	u, _ := url.Parse(h.Get("Location"))
	params, _ := url.ParseQuery(u.Fragment)
	token := params.Get("oidc_token")
	if token == "" {
		t.Fatal(h.Get("Location"))
	}
	data, _ := json.Marshal(map[string]string{"oidc_token": token})
	signed := s.json("POST", "/api/v1/auth/oidc/exchange", string(data), "", 200)
	user := signed["user"].(map[string]any)
	if user["username"] != "avery" || user["is_admin"] != false {
		t.Fatal(user)
	}
	s.json("POST", "/api/v1/auth/oidc/exchange", string(data), "", 401)
	_, h = s.call("GET", path, "", "", "", 302)
	if !strings.Contains(h.Get("Location"), "invalid_state") {
		t.Fatal("authorization state was reused")
	}
}
func testSMTP(t *testing.T) (net.Addr, <-chan string) {
	t.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	messages := make(chan string, 10)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				r := bufio.NewReader(c)
				fmt.Fprint(c, "220 test\r\n")
				for {
					line, e := r.ReadString('\n')
					if e != nil {
						return
					}
					verb := strings.Fields(line)
					if len(verb) == 0 {
						return
					}
					switch verb[0] {
					case "EHLO", "HELO", "MAIL", "RCPT":
						fmt.Fprint(c, "250 OK\r\n")
					case "DATA":
						fmt.Fprint(c, "354 send message\r\n")
						var body strings.Builder
						for {
							part, err := r.ReadString('\n')
							if err != nil {
								return
							}
							if part == ".\r\n" {
								break
							}
							body.WriteString(part)
						}
						messages <- body.String()
						fmt.Fprint(c, "250 accepted\r\n")
					case "QUIT":
						fmt.Fprint(c, "221 bye\r\n")
						return
					default:
						fmt.Fprint(c, "500 unknown command\r\n")
					}
				}
			}(c)
		}
	}()
	return ln.Addr(), messages
}
func TestMailVerificationAndRecovery(t *testing.T) {
	s := startTestSystem(t)
	client := s.Client()
	client.Jar, _ = cookiejar.New(nil)
	admin := s.install()
	addr, messages := testSMTP(t)
	port, _ := strconv.Atoi(strings.Split(addr.String(), ":")[1])
	smtp, _ := json.Marshal(map[string]any{"smtp_host": "127.0.0.1", "smtp_port": port, "smtp_from_email": "farm@example.test", "smtp_security": "none", "smtp_auth_enabled": false})
	s.json("POST", "/api/v1/auth/smtp", string(smtp), admin, 200)
	s.json("POST", "/api/v1/auth/advanced-auth/enable", "{}", admin, 200)
	if _, e := s.app.db.Exec("UPDATE users SET email='operator@example.test' WHERE username='operator'"); e != nil {
		t.Fatal(e)
	}
	message := func() string {
		t.Helper()
		select {
		case raw := <-messages:
			_, body, _ := strings.Cut(raw, "\r\n\r\n")
			b, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, strings.NewReader(body)))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		case <-time.After(4 * time.Second):
			t.Fatal("mail was not delivered")
			return ""
		}
	}
	getCode := func() string {
		t.Helper()
		m := regexp.MustCompile(`\b[0-9]{6}\b`).FindString(message())
		if m == "" {
			t.Fatal("missing verification code")
		}
		return m
	}
	setup := s.json("POST", "/api/v1/auth/2fa/email/enable", "{}", admin, 200)["setup_token"].(string)
	s.json("POST", "/api/v1/auth/2fa/email/enable/confirm", `{"setup_token":"`+setup+`","code":"`+getCode()+`"}`, admin, 200)
	challenge := s.json("POST", "/api/v1/auth/login", `{"username":"operator","password":"Local-test-pass-2468"}`, "", 200)["pre_auth_token"].(string)
	s.json("POST", "/api/v1/auth/2fa/email/send", `{"pre_auth_token":"`+challenge+`"}`, "", 200)
	payload := `{"pre_auth_token":"` + challenge + `","method":"email","code":"` + getCode() + `"}`
	s.json("POST", "/api/v1/auth/2fa/verify", payload, "", 200)
	s.json("POST", "/api/v1/auth/2fa/verify", payload, "", 401)
	s.json("POST", "/api/v1/auth/forgot-password", `{"email":"operator@example.test"}`, "", 200)
	parts := strings.Split(strings.TrimSpace(message()), "#reset_token=")
	if len(parts) != 2 {
		t.Fatal("reset link absent")
	}
	reset := `{"new_password":"Changed-test-pass-2468","token":"` + parts[1] + `"}`
	s.json("POST", "/api/v1/auth/forgot-password/confirm", reset, "", 200)
	s.json("POST", "/api/v1/auth/forgot-password/confirm", reset, "", 400)
	s.call("GET", "/api/v1/printers/", "", "", admin, 401)
	if _, e := s.app.db.ExecContext(context.Background(), "SELECT 1"); e != nil {
		t.Fatal(e)
	}
}

func TestTaggedFernetBackupMigration(t *testing.T) {
	const taggedToken = "fernet:gAAAAABrBlLzP5uM_pFHsjAA2Boi3Th6jqNnKKfhd1-lZjDzb3x287KCp3aKtyvPxKFKXgPxL6A1d4qVOTdqUtXTWdUCbbQVVEOYxEKLs-4DBlmORQW63Dk="
	const legacyAPIKey = "bb_restore-fixture-from-v132"
	const passlibDigest = "$pbkdf2-sha256$29000$YmFtYnVkZHktbWlncmF0aW9u$cqpZlbrUNII5wSZXeUSm5llks6mcdlVLW9UUdC7Zprw"
	const oldKey = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="
	t.Setenv("MFA_ENCRYPTION_KEY", "")
	// This fixture is a release-format database without Bambastic's additional tables.
	dir := t.TempDir()
	origin := filepath.Join(dir, "origin.db")
	db, e := sql.Open("sqlite", origin)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec(ddl); e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO users(id,username,password_hash,role,is_active,auth_source) VALUES(1,'local-user','fixture','user',1,'local');INSERT INTO user_totp(user_id,secret,is_enabled) VALUES(1,?,1)", taggedToken)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec(`INSERT INTO users(id,username,password_hash,role,is_active,auth_source) VALUES(2,'older-local-user','fixture','user',1,'local');INSERT INTO user_totp(user_id,secret,is_enabled) VALUES(2,'MZXW6YTBOI',1)`)
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO api_keys(id,name,key_hash,key_prefix,user_id,can_queue,can_control_printer,can_read_status,enabled) VALUES(1,'Reference key',?,?,1,0,0,1,1)", passlibDigest, legacyAPIKey[:8]+"...")
	if e != nil {
		t.Fatal(e)
	}
	_, e = db.Exec("INSERT INTO print_archives(filename,file_path,file_size,status,is_favorite,quantity) VALUES('legacy.gcode','archive/1/legacy.gcode',6,'archived',0,1)")
	if e != nil {
		t.Fatal(e)
	}
	db.Close()
	file := filepath.Join(dir, "legacy.zip")
	f, e := os.Create(file)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	raw, e := os.ReadFile(origin)
	if e != nil {
		t.Fatal(e)
	}
	for n, b := range map[string][]byte{"bambuddy.db": raw, ".mfa_encryption_key": []byte(oldKey), "manifest.json": []byte(`{"app_version":"1.3.2","database":"sqlite"}`), "archive/1/legacy.gcode": []byte("G1 X2\n")} {
		w, e := z.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		w.Write(b)
	}
	z.Close()
	f.Close()
	dest := filepath.Join(dir, "imported")
	if e = backup.Restore(context.Background(), file, dest); e != nil {
		t.Fatal(e)
	}
	app, e := testApplicationAt(t, dest)
	if e != nil {
		t.Fatal(e)
	}
	defer app.close()
	var stored string
	if e = app.db.QueryRow("SELECT secret FROM user_totp WHERE user_id=1").Scan(&stored); e != nil || !strings.HasPrefix(stored, "bambastic:") {
		t.Fatal("restored authenticator was not rewrapped", e)
	}
	plain, e := app.auth.OpenSecret(stored)
	if e != nil || plain != "JBSWY3DPEHPK3PXP" {
		t.Fatal("restored authenticator does not match reference fixture", e)
	}
	principal, e := app.auth.Resolve(t.Context(), legacyAPIKey, "session")
	if e != nil || principal.ID != 1 || principal.KeyID != 1 {
		t.Fatal("legacy key did not resolve to its original owner", e)
	}
	var newDigest string
	if e = app.db.QueryRow("SELECT key_hash FROM api_keys WHERE id=1").Scan(&newDigest); e != nil || newDigest == passlibDigest {
		t.Fatal("legacy key was not migrated", e)
	}
	if e = app.db.QueryRow("SELECT secret FROM user_totp WHERE user_id=2").Scan(&stored); e != nil || !strings.HasPrefix(stored, "bambastic:") {
		t.Fatal("plaintext authenticator was not migrated", e)
	}
	if plain, e = app.auth.OpenSecret(stored); e != nil || plain != "MZXW6YTBOI" {
		t.Fatal("plaintext authenticator changed", e)
	}
	server := httptest.NewServer(app.handler)
	defer server.Close()
	resp, e := http.Get(server.URL + "/api/v1/archives/1/download")
	if e != nil {
		t.Fatal(e)
	}
	defer resp.Body.Close()
	content, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(content) != "G1 X2\n" {
		t.Fatalf("archive download %d %.80s", resp.StatusCode, content)
	}
}
