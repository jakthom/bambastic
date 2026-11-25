// Package uidiagnostics stores development browser reports as independent JSONL streams.
package uidiagnostics

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

//go:embed README.md
var Guide string

const Directory = "ui-diagnostics"
const maxRequest = 256 << 10
const maxLog = 16 << 20

var sessionID = regexp.MustCompile("^[A-Za-z0-9-]{1,80}$")

type report struct {
	Session string            `json:"session"`
	Events  []json.RawMessage `json:"events,omitempty"`
	Dropped int               `json:"dropped,omitempty"`
	Crash   json.RawMessage   `json:"crash,omitempty"`
}

type stream struct {
	path string
	mu   sync.Mutex
}

func (s *stream) append(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if info, err := os.Stat(s.path); err == nil && info.Size()+int64(len(line)) > maxLog {
		if err := os.Rename(s.path, s.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	return errors.Join(err, f.Close())
}

// Register is called only by the dev build. Early boot and login failures occur before sign-in.
func Register(mux *http.ServeMux, root string) error {
	started := time.Now()
	dir := filepath.Join(root, Directory)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	for _, name := range []string{"uie", "crashes"} {
		log := &stream{path: filepath.Join(dir, name+".jsonl")}
		if err := log.append(nil); err != nil {
			return err
		}
		limit := rate.NewLimiter(80, 160)
		mux.HandleFunc("POST /"+name, func(w http.ResponseWriter, r *http.Request) {
			if !limit.Allow() {
				http.Error(w, "Please retry", 429)
				return
			}
			if typ, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); typ != "application/json" {
				http.Error(w, "Expected JSON", 415)
				return
			}
			data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequest))
			if err != nil {
				http.Error(w, "Could not read report within size limit", 413)
				return
			}
			var body report
			if json.Unmarshal(data, &body) != nil || !sessionID.MatchString(body.Session) ||
				(name == "uie" && (len(body.Events) == 0 || len(body.Events) > 128 || len(body.Crash) != 0)) ||
				(name == "crashes" && (len(body.Events) != 0 || len(body.Crash) == 0 || body.Crash[0] != '{')) {
				http.Error(w, "Invalid UI report", 400)
				return
			}
			var server map[string]any
			if name == "crashes" {
				server = map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "go": runtime.Version(),
					"uptimeMs": time.Since(started).Milliseconds(), "goroutines": runtime.NumGoroutine()}
			}
			line, err := json.Marshal(struct {
				Received time.Time      `json:"received"`
				Server   map[string]any `json:"server,omitempty"`
				report
			}{time.Now().UTC(), server, body})
			if err != nil || log.append(append(line, '\n')) != nil {
				http.Error(w, "Could not write UI report", 500)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	}
	return nil
}

// Tail returns complete recent records for a local developing agent.
func Tail(root, name string) (string, error) {
	if name != "uie" && name != "crashes" {
		return "", errors.New("unknown UI log")
	}
	f, err := os.OpenInRoot(filepath.Join(root, Directory), name+".jsonl")
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	const size = 512 << 10
	start := max(int64(0), info.Size()-size)
	b, err := io.ReadAll(io.NewSectionReader(f, start, info.Size()-start))
	if err != nil {
		return "", err
	}
	if start > 0 {
		_, b, _ = bytes.Cut(b, []byte("\n"))
	}
	if last := bytes.LastIndexByte(b, '\n'); last >= 0 {
		return string(b[:last+1]), nil
	}
	return "", nil
}
