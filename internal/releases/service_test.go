package releases

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"bambastic/internal/access"
)

func TestSignedUpdateKeepsOriginalUntilVerified(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	const original = "original fixture"
	file := filepath.Join(t.TempDir(), "binary")
	os.WriteFile(file, []byte(original), 0755)
	bytes := "release fixture"
	hash := sha256.Sum256([]byte(bytes))
	var doc []byte
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/feed" {
			w.Write(doc)
		} else if r.URL.Path == "/binary" {
			w.Write([]byte(bytes))
		} else {
			http.NotFound(w, r)
		}
	}))
	defer host.Close()
	sign := func(valid bool) {
		m := manifest{Version: "1.1.0", Artifacts: []artifact{{runtime.GOOS, runtime.GOARCH, host.URL + "/binary", hex.EncodeToString(hash[:]), int64(len("release fixture"))}}}
		b, _ := json.Marshal(m)
		sig := ed25519.Sign(key, b)
		if !valid {
			sig[0] ^= 1
		}
		doc, _ = json.Marshal(envelope{b, sig})
	}
	s := New()
	s.feed = host.URL + "/feed"
	s.key = pub
	s.path = func() (string, error) { return file, nil }
	mux := http.NewServeMux()
	s.Register(mux)
	req := func(admin bool, status int, success bool) {
		r := httptest.NewRequest("POST", "/api/v1/updates/apply", nil)
		r = r.WithContext(access.With(r.Context(), access.Principal{Admin: admin}))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status || strings.Contains(w.Body.String(), `"success":true`) != success {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	sign(true)
	req(false, 403, false)
	sign(false)
	req(true, 200, false)
	sign(true)
	bytes = "invalid fixture"
	req(true, 200, false)
	b, _ := os.ReadFile(file)
	if string(b) != original {
		t.Fatal("failed candidate replaced binary")
	}
	bytes = "release fixture"
	req(true, 200, true)
	b, _ = os.ReadFile(file)
	if string(b) != bytes {
		t.Fatal("verified release was not installed")
	}
	req(true, 200, false)
}
