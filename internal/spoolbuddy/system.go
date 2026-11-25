package spoolbuddy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const Version = "0.1.0"

func IsOlder(v string) bool {
	if v == "go-1" || v == "" {
		return true
	}
	current := strings.Split(strings.TrimPrefix(v, "v"), ".")
	latest := strings.Split(Version, ".")
	for i, x := range latest {
		if i >= len(current) {
			return true
		}
		a, e := strconv.Atoi(current[i])
		if e != nil {
			return false
		}
		b, _ := strconv.Atoi(x)
		if a != b {
			return a < b
		}
	}
	return false
}

type savedConfig struct {
	Backend string `json:"backend_url"`
	Key     string `json:"api_key"`
}

func runSystemAction(ctx context.Context, k string) error {
	if runtime.GOOS != "linux" {
		return errors.New("systemd is required for station commands")
	}
	commands := map[string][]string{"reboot": {"reboot"}, "shutdown": {"poweroff"}, "restart_daemon": {"restart", "spoolbuddy.service"}, "restart_browser": {"restart", "getty@tty1.service"}}
	args, ok := commands[k]
	if !ok {
		return errors.New("unknown station command")
	}
	return exec.CommandContext(ctx, "/usr/bin/sudo", append([]string{"-n", "/usr/bin/systemctl"}, args...)...).Run()
}
func saveConfig(path string, c savedConfig) error {
	if path == "" {
		return errors.New("station configuration path unavailable")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".station-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if e = json.NewEncoder(f).Encode(c); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
func (s *Station) applySystem(ctx context.Context, d map[string]any) error {
	cmd, _ := d["pending_command"].(string)
	if cmd == "" {
		return nil
	}
	result := func(ok bool, msg string) error {
		_, e := s.post(ctx, message{"devices/" + url.PathEscape(s.Config.DeviceID) + "/system/command-result", map[string]any{"command": cmd, "success": ok, "message": msg}})
		return e
	}
	switch cmd {
	case "run_scale_diag", "run_nfc_diag", "run_read_tag_diag":
		k := strings.TrimSuffix(strings.TrimPrefix(cmd, "run_"), "_diag")
		s.mu.Lock()
		ok := s.nfcOK
		descr := "NFC reader responded"
		if k == "scale" {
			ok = s.scaleOK && s.hasRaw
			descr = fmt.Sprintf("Raw ADC reading: %d", s.raw)
		} else if k == "read_tag" {
			ok = ok && s.lastTag != ""
			descr = "UID: " + s.lastTag
		}
		s.mu.Unlock()
		code := 0
		if !ok {
			code = 1
			descr = "No current " + k + " reading"
		}
		_, e := s.post(ctx, message{"diagnostics/" + url.PathEscape(s.Config.DeviceID) + "/result", map[string]any{"diagnostic": k, "success": ok, "output": descr, "exit_code": code}})
		return e
	case "apply_system_config":
		v, _ := d["pending_system_payload"].(map[string]any)
		backend, _ := v["backend_url"].(string)
		target, e := url.Parse(backend)
		if e != nil || target.Hostname() == "" || target.User != nil || target.Fragment != "" || target.RawQuery != "" || target.Scheme != "http" && target.Scheme != "https" {
			return result(false, "Invalid backend URL")
		}
		token, _ := v["api_key"].(string)
		current, _ := url.Parse(s.Config.Backend)
		if token == "" && current.Scheme == target.Scheme && strings.EqualFold(current.Host, target.Host) && strings.TrimRight(current.Path, "/") == strings.TrimRight(target.Path, "/") {
			token = s.Config.APIKey
		}
		cfg := savedConfig{strings.TrimRight(backend, "/"), token}
		if e = saveConfig(s.Config.ConfigPath, cfg); e != nil {
			return result(false, "Cannot persist station configuration")
		}
		if e = result(true, "Station configuration saved"); e != nil {
			return e
		}
		// The queued hardware events belong to the original backend.
		s.mu.Lock()
		if cfg.Backend != s.Config.Backend {
			s.queue = nil
			s.latest = nil
			s.write = nil
		}
		s.Config.Backend, s.Config.APIKey = cfg.Backend, cfg.Key
		s.mu.Unlock()
		return nil
	case "reboot", "shutdown", "restart_daemon", "restart_browser":
		// Clear a queued reboot before invoking systemd so a reboot cannot replay.
		if e := result(true, "Station received the command"); e != nil {
			return e
		}
		if e := s.systemAction(ctx, cmd); e != nil {
			return result(false, "Operating system rejected the command")
		}
		return nil
	}
	return nil
}
