// Package access resolves installation, user and key authority on each request.
package access

import (
	"bambastic/internal/httpx"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

//go:embed groups.json
var groupData []byte

type Principal struct {
	ID          int64
	Username    string
	Admin       bool
	Permissions map[string]bool
	PrinterIDs  map[int64]bool
	KeyID       int64
}

func (p Principal) Has(s string) bool           { return p.Admin || p.Permissions[s] }
func (p Principal) AllowsPrinter(id int64) bool { return p.PrinterIDs == nil || p.PrinterIDs[id] }

type key struct{}

func From(ctx context.Context) Principal                    { p, _ := ctx.Value(key{}).(Principal); return p }
func With(ctx context.Context, p Principal) context.Context { return context.WithValue(ctx, key{}, p) }

type Service struct {
	DB     *sql.DB
	second *secondFactor
}
type defaults struct {
	Description string
	Permissions []string
}

func Groups() map[string]defaults {
	var g map[string]defaults
	_ = json.Unmarshal(groupData, &g)
	return g
}
func New(db *sql.DB, data string) (*Service, error) {
	tx, e := db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`CREATE TABLE IF NOT EXISTS bambastic_credentials(hash TEXT PRIMARY KEY,user_id INTEGER REFERENCES users(id) ON DELETE CASCADE,kind TEXT NOT NULL,expires INTEGER NOT NULL,key_id INTEGER REFERENCES api_keys(id) ON DELETE CASCADE,parent_hash TEXT)`); e != nil {
		return nil, e
	}
	for _, name := range []string{"Administrators", "Operators", "Viewers"} {
		g := Groups()[name]
		perms, _ := json.Marshal(g.Permissions)
		if _, e = tx.Exec("INSERT OR IGNORE INTO groups(name,description,permissions,is_system) VALUES(?,?,?,1)", name, g.Description, string(perms)); e != nil {
			return nil, e
		}
	}
	factor, e := newFactor(tx, data)
	if e != nil {
		return nil, e
	}
	return &Service{DB: db, second: factor}, tx.Commit()
}

type Query interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func Setting(ctx context.Context, db Query, key, def string) (string, error) {
	var v string
	e := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&v)
	if errors.Is(e, sql.ErrNoRows) {
		return def, nil
	}
	return v, e
}
func Put(tx *sql.Tx, k, v string) error {
	_, e := tx.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=CURRENT_TIMESTAMP", k, v)
	return e
}
func (s *Service) Enabled(ctx context.Context) (bool, error) {
	v, e := Setting(ctx, s.DB, "auth_enabled", "false")
	return v == "true", e
}
func digest(v string) string { b := sha256.Sum256([]byte(v)); return hex.EncodeToString(b[:]) }
func random() string         { var b [32]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func (s *Service) User(ctx context.Context, id int64) (Principal, error) {
	p := Principal{ID: id, Permissions: map[string]bool{}}
	if id == 0 {
		enabled, e := s.Enabled(ctx)
		if e != nil || enabled {
			return p, errors.New("login required")
		}
		p.Admin = true
		p.Username = "local"
		return p, nil
	}
	var role string
	if e := s.DB.QueryRowContext(ctx, "SELECT username,role FROM users WHERE id=? AND is_active", id).Scan(&p.Username, &role); e != nil {
		return p, e
	}
	p.Admin = role == "admin"
	rows, e := s.DB.QueryContext(ctx, "SELECT g.name,g.permissions,g.restrict_printers FROM groups g JOIN user_groups u ON u.group_id=g.id WHERE u.user_id=?", id)
	if e != nil {
		return p, e
	}
	restricted := false
	for rows.Next() {
		var name, raw string
		var restrict bool
		if e = rows.Scan(&name, &raw, &restrict); e != nil {
			rows.Close()
			return p, e
		}
		var list []string
		if e = json.Unmarshal([]byte(raw), &list); e != nil {
			rows.Close()
			return p, e
		}
		for _, v := range list {
			p.Permissions[v] = true
		}
		if name == "Administrators" {
			p.Admin = true
		}
		restricted = restricted || restrict
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return p, e
	}
	if restricted && !p.Admin {
		p.PrinterIDs = map[int64]bool{}
		rows, e := s.DB.QueryContext(ctx, `SELECT gp.printer_id FROM group_printers gp JOIN groups g ON g.id=gp.group_id JOIN user_groups u ON g.id=u.group_id WHERE u.user_id=? AND g.restrict_printers UNION SELECT pr.id FROM printers pr JOIN group_locations l ON pr.location=l.location JOIN groups g ON g.id=l.group_id JOIN user_groups u ON u.group_id=g.id WHERE u.user_id=? AND g.restrict_printers`, id, id)
		if e != nil {
			return p, e
		}
		defer rows.Close()
		for rows.Next() {
			var i int64
			if e := rows.Scan(&i); e != nil {
				return p, e
			}
			p.PrinterIDs[i] = true
		}
		return p, rows.Err()
	}
	return p, nil
}
func Credential(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return r.Header.Get("X-API-Key")
}
func (s *Service) Mint(ctx context.Context, p Principal, kind string, ttl time.Duration, parent string) (string, error) {
	token := random()
	var uid, kid any
	if p.ID != 0 {
		uid = p.ID
	}
	if p.KeyID != 0 {
		kid = p.KeyID
	}
	_, e := s.DB.ExecContext(ctx, "INSERT INTO bambastic_credentials(hash,user_id,kind,expires,key_id,parent_hash) VALUES(?,?,?,?,?,?)", digest(token), uid, kind, time.Now().Add(ttl).Unix(), kid, digest(parent))
	return token, e
}
func (s *Service) Resolve(ctx context.Context, token, kind string) (Principal, error) {
	enabled, e := s.Enabled(ctx)
	if e != nil {
		return Principal{}, e
	}
	if token == "" {
		if enabled {
			return Principal{}, errors.New("login required")
		}
		return s.User(ctx, 0)
	}
	if strings.HasPrefix(token, "bb_") && kind == "session" {
		return s.resolveKey(ctx, token)
	}
	var uid, kid int64
	e = s.DB.QueryRowContext(ctx, "SELECT COALESCE(user_id,0),COALESCE(key_id,0) FROM bambastic_credentials WHERE hash=? AND kind=? AND expires>?", digest(token), kind, time.Now().Unix()).Scan(&uid, &kid)
	if e != nil {
		return Principal{}, e
	}
	if kid != 0 {
		return s.apiKey(ctx, "", kid)
	}
	return s.User(ctx, uid)
}
func (s *Service) Protect(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := Credential(r)
		kind := "session"
		if r.URL.Path == "/api/v1/ws" && r.URL.Query().Has("token") {
			token = r.URL.Query().Get("token")
			kind = "ws"
		}
		archivePath := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/api/v1/archives/"), "/", 3)
		imageDownload := strings.HasPrefix(r.URL.Path, "/api/v1/archives/") && len(archivePath) == 3 && archivePath[1] == "project-image"
		linkPath := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/v1/external-links/"), "/")
		linkIcon := r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/api/v1/external-links/") && len(linkPath) == 2 && linkPath[1] == "icon"
		if (strings.Contains(r.URL.Path, "/camera/") || imageDownload || linkIcon) && token == "" && r.URL.Query().Get("token") != "" {
			token = r.URL.Query().Get("token")
			kind = "media"
		}
		p, e := s.Resolve(r.Context(), token, kind)
		if e != nil {
			httpx.Error(w, 401, "Authentication required")
			return
		}
		h.ServeHTTP(w, r.WithContext(With(r.Context(), p)))
	})
}
