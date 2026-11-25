// Package releases installs platform binaries from an operator-configured signed release feed.
package releases

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	"golang.org/x/mod/semver"
)

const Version = "0.1.0"

type state struct {
	Status   string  `json:"status"`
	Progress int     `json:"progress"`
	Message  string  `json:"message"`
	Error    *string `json:"error"`
}
type artifact struct {
	OS, Arch, URL, SHA256 string
	Size                  int64
}
type manifest struct {
	Version, Name, Notes, URL string
	Published                 string `json:"published_at"`
	Artifacts                 []artifact
}
type envelope struct {
	Manifest  []byte
	Signature []byte
}
type Service struct {
	feed      string
	key       ed25519.PublicKey
	path      func() (string, error)
	mu        sync.Mutex
	state     state
	installed string
}

func New() *Service {
	k, _ := base64.StdEncoding.DecodeString(os.Getenv("BAMBASTIC_RELEASE_KEY"))
	return &Service{feed: os.Getenv("BAMBASTIC_RELEASE_FEED"), key: k, path: os.Executable, state: state{Status: "idle", Message: "Ready"}}
}
func safeURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && u.Host != "" && u.User == nil && u.Fragment == "" && (u.Scheme == "https" || u.Scheme == "http")
}
func fetch(ctx context.Context, u string) (io.ReadCloser, error) {
	if !safeURL(u) {
		return nil, errors.New("invalid release URL")
	}
	req, e := http.NewRequestWithContext(ctx, "GET", u, nil)
	if e != nil {
		return nil, e
	}
	client := *lanhttp.Client
	client.Timeout = 120 * time.Second
	res, e := client.Do(req)
	if e != nil {
		return nil, errors.New("release server unavailable")
	}
	if res.StatusCode != 200 {
		res.Body.Close()
		return nil, fmt.Errorf("release server returned HTTP %d", res.StatusCode)
	}
	return res.Body, nil
}
func (s *Service) read(ctx context.Context) (manifest, artifact, error) {
	var m manifest
	var a artifact
	if s.feed == "" || len(s.key) != ed25519.PublicKeySize {
		return m, a, errors.New("configure a signed Bambastic release feed and public key to enable updates")
	}
	body, e := fetch(ctx, s.feed)
	if e != nil {
		return m, a, e
	}
	defer body.Close()
	var env envelope
	if json.NewDecoder(io.LimitReader(body, 2<<20)).Decode(&env) != nil || !ed25519.Verify(s.key, env.Manifest, env.Signature) {
		return m, a, errors.New("release signature verification failed")
	}
	if json.Unmarshal(env.Manifest, &m) != nil || !semver.IsValid("v"+m.Version) || len(m.Notes) > 64<<10 {
		return m, a, errors.New("invalid release manifest")
	}
	if m.URL != "" && !safeURL(m.URL) {
		return m, a, errors.New("invalid release information URL")
	}
	for _, a = range m.Artifacts {
		if a.OS != runtime.GOOS || a.Arch != runtime.GOARCH {
			continue
		}
		hash, e := hex.DecodeString(a.SHA256)
		if e != nil || len(hash) != 32 || !safeURL(a.URL) || a.Size < 1 || a.Size > 512<<20 {
			return m, a, errors.New("invalid platform artifact")
		}
		return m, a, nil
	}
	return m, a, errors.New("release has no artifact for this operating system and architecture")
}
func (s *Service) set(st string, n int, msg string, e error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.state = state{Status: st, Progress: n, Message: msg}
	if e != nil {
		v := e.Error()
		s.state.Error = &v
	}
}
func (s *Service) install(ctx context.Context, a artifact) error {
	path, e := s.path()
	if e != nil {
		return e
	}
	path, e = filepath.EvalSymlinks(path)
	if e != nil {
		return e
	}
	current, e := os.Stat(path)
	if e != nil || !current.Mode().IsRegular() {
		return errors.New("cannot locate the running binary")
	}
	reader, e := fetch(ctx, a.URL)
	if e != nil {
		return e
	}
	defer reader.Close()
	f, e := os.CreateTemp(filepath.Dir(path), ".bambastic-update-")
	if e != nil {
		return errors.New("the executable directory is not writable")
	}
	defer os.Remove(f.Name())
	defer f.Close()
	hash := sha256.New()
	size, e := io.Copy(io.MultiWriter(f, hash), io.LimitReader(reader, a.Size+1))
	if e != nil || size != a.Size || !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), a.SHA256) {
		return errors.New("download size or SHA-256 digest differs from the signed manifest")
	}
	s.set("installing", 90, "Installing verified binary", nil)
	if e = f.Chmod(current.Mode().Perm()); e == nil {
		e = f.Sync()
	}
	if e == nil {
		e = f.Close()
	}
	if e != nil {
		return e
	}
	return os.Rename(f.Name(), path)
}
func (s *Service) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/updates/status", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "system:read") {
			return
		}
		s.mu.Lock()
		v := s.state
		s.mu.Unlock()
		httpx.JSON(w, 200, v)
	})
	m.HandleFunc("GET /api/v1/updates/check", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "system:read") {
			return
		}
		v := map[string]any{"update_available": false, "current_version": Version, "latest_version": nil}
		m, _, e := s.read(r.Context())
		if e != nil {
			v["message"] = e.Error()
			if s.feed != "" {
				v["error"] = e.Error()
			}
		} else {
			v["latest_version"] = m.Version
			v["update_available"] = semver.Compare("v"+m.Version, "v"+Version) > 0
			v["release_name"] = m.Name
			v["release_notes"] = m.Notes
			v["release_url"] = m.URL
			v["published_at"] = m.Published
		}
		httpx.JSON(w, 200, v)
	})
	m.HandleFunc("POST /api/v1/updates/apply", func(w http.ResponseWriter, r *http.Request) {
		if !access.From(r.Context()).Admin {
			httpx.Error(w, 403, "Administrator required")
			return
		}
		s.mu.Lock()
		if s.state.Status == "downloading" || s.state.Status == "checking" || s.state.Status == "installing" {
			s.mu.Unlock()
			httpx.Error(w, 409, "An update is in progress")
			return
		}
		installed := s.installed
		s.state = state{Status: "checking", Progress: 1, Message: "Checking signed release"}
		s.mu.Unlock()
		m, a, e := s.read(r.Context())
		if e == nil && (semver.Compare("v"+m.Version, "v"+Version) <= 0 || installed != "" && semver.Compare("v"+m.Version, "v"+installed) <= 0) {
			e = errors.New("no newer update is available")
		}
		if e == nil {
			s.set("downloading", 10, "Downloading platform binary", nil)
			e = s.install(r.Context(), a)
		}
		if e != nil {
			s.set("error", 0, "Update could not be installed", e)
			httpx.JSON(w, 200, map[string]any{"success": false, "message": e.Error()})
			return
		}
		s.mu.Lock()
		s.installed = m.Version
		s.mu.Unlock()
		msg := "Installed " + m.Version + ". Restart the Bambastic service to use it."
		s.set("complete", 100, msg, nil)
		httpx.JSON(w, 200, map[string]any{"success": true, "message": msg})
	})
}
