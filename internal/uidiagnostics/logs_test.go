package uidiagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestIndependentStreams(t *testing.T) {
	root := t.TempDir()
	mux := http.NewServeMux()
	if err := Register(mux, root); err != nil {
		t.Fatal(err)
	}
	post := func(path, payload string, status int) {
		r := httptest.NewRequest("POST", path, strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != status {
			t.Errorf("%s: got %d, want %d", path, w.Code, status)
		}
	}
	const events = `{"session":"tab-test","events":[{"seq":1,"kind":"navigation"}]}`
	const crash = `{"session":"tab-test","crash":{"seq":2,"source":"react.boundary"}}`
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post("/uie", events, 204); post("/crashes", crash, 204) }()
	}
	wg.Wait()
	post("/crashes", events, 400)
	post("/uie", crash, 400)
	post("/uie", strings.Repeat("x", maxRequest+1), 413)
	for _, name := range []string{"uie", "crashes"} {
		tail, err := Tail(root, name)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(tail), "\n")
		if len(lines) != 8 {
			t.Fatalf("got %d lines for %s", len(lines), name)
		}
		for _, line := range lines {
			var v report
			if err := json.Unmarshal([]byte(line), &v); err != nil || v.Session != "tab-test" || (len(v.Crash) > 0) != (name == "crashes") {
				t.Fatalf("corrupt %s record", name)
			}
		}
	}
	path := filepath.Join(root, Directory, "uie.jsonl")
	if err := os.Truncate(path, maxLog); err != nil {
		t.Fatal(err)
	}
	post("/uie", events, 204)
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatal(err)
	}
	tail, err := Tail(root, "uie")
	if err != nil || strings.Count(tail, "\n") != 1 {
		t.Fatal("unexpected tail after rotation", err)
	}
}
