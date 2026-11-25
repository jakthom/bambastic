package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func wledPreset(ctx context.Context, base string, preset int) error {
	if preset < 1 || preset > 250 {
		return fail(422, "Preset must be from 1 to 250")
	}
	payload, _ := json.Marshal(obj{"ps": preset})
	var reply obj
	if e := send(ctx, "POST", base+"/json/state", map[string]string{"Content-Type": "application/json"}, payload, &reply); e != nil {
		return e
	}
	if reply == nil {
		return fail(502, "WLED returned an invalid state")
	}
	return nil
}

func (s *Service) wled(m *http.ServeMux) {
	for _, action := range []string{"presets", "test-connection", "test-preset"} {
		s.route(m, "POST", "printers/{id}/wled/"+action, "printers:update", func(r *http.Request) (any, error) {
			if !permitted(r, id(r)) {
				return nil, sql.ErrNoRows
			}
			if _, e := s.get(r.Context(), "printers", id(r)); e != nil {
				return nil, e
			}
			var req struct {
				Base   string `json:"base_url"`
				Preset int    `json:"preset_id"`
			}
			if e := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req); e != nil {
				return nil, fail(422, "Invalid WLED request")
			}
			base := strings.TrimRight(strings.TrimSpace(req.Base), "/")
			u, e := splitURL(base)
			if e != nil || len(base) > 500 || u.RawQuery != "" {
				return nil, fail(422, "Invalid WLED base URL")
			}
			if action == "test-preset" {
				return obj{"success": true}, wledPreset(r.Context(), base, req.Preset)
			}
			path := "/presets.json"
			if action == "test-connection" {
				path = "/json/info"
			}
			var v obj
			if e := send(r.Context(), "GET", base+path, nil, nil, &v); e != nil {
				return nil, e
			}
			if v == nil {
				return nil, fail(502, "WLED returned an invalid response")
			}
			if action == "test-connection" {
				return obj{"name": v["name"], "version": v["ver"]}, nil
			}
			presets := []obj{}
			for key, val := range v {
				n, e := strconv.Atoi(key)
				p, ok := val.(map[string]any)
				if e != nil || n < 1 || n > 250 || !ok {
					continue
				}
				name, _ := p["n"].(string)
				name = strings.TrimSpace(name)
				if name == "" {
					name = key
				}
				presets = append(presets, obj{"id": n, "name": name})
			}
			sort.Slice(presets, func(i, j int) bool { return presets[i]["id"].(int) < presets[j]["id"].(int) })
			return presets, nil
		})
	}
}

type lightState struct {
	target          string
	retry, finished time.Time
}

func wledFaults(raw any) bool {
	// Domain adapters publish both decoded typed slices and JSON slices.
	b, _ := json.Marshal(raw)
	var faults []struct {
		Severity    int
		Description string
		FullCode    string `json:"full_code"`
		Actions     []any
	}
	if json.Unmarshal(b, &faults) != nil {
		return false
	}
	for _, f := range faults {
		if f.Severity > 0 && (len(f.Actions) > 0 || f.Description != "" && !(len(f.FullCode) == 16 && f.Severity == 3)) {
			return true
		}
	}
	return false
}
func lightStatus(r reading, plate bool, presets obj) string {
	if !r.online {
		return "offline"
	}
	candidates := []string{}
	if r.state == "PAUSE" && r.filamentProblem {
		candidates = append(candidates, "filament_problem")
	}
	if r.hms {
		candidates = append(candidates, "hms_error")
	}
	if plate && (r.state == "IDLE" || r.state == "FINISH" || r.state == "FAILED") {
		candidates = append(candidates, "queue_waiting")
	}
	switch r.state {
	case "IDLE":
		candidates = append(candidates, "idle")
	case "FINISH":
		candidates = append(candidates, "finished")
	case "FAILED":
		candidates = append(candidates, "error")
	case "PAUSE":
		candidates = append(candidates, "paused")
	case "RUNNING", "PRINTING":
		candidates = append(candidates, "printing")
	case "SLICING", "PREPARE":
		candidates = append(candidates, "prepare")
	}
	for _, key := range candidates {
		if presets[key] != nil {
			return key
		}
	}
	return ""
}
func (s *Service) syncLights(ctx context.Context, states map[int64]lightState, when time.Time) {
	rows, e := s.list(ctx, "SELECT id,wled_config,awaiting_plate_clear FROM printers WHERE is_active AND wled_config IS NOT NULL")
	if e != nil {
		return
	}
	requirePlate := flag(setting(ctx, s.db, "require_plate_clear"))
	seen := map[int64]bool{}
	for _, p := range rows {
		cfg := sub(p["wled_config"])
		if !flag(cfg["enabled"]) {
			continue
		}
		pid := int64(num(p["id"]))
		seen[pid] = true
		s.mu.RLock()
		r, known := s.telemetry[pid]
		s.mu.RUnlock()
		if !known {
			continue
		}
		state := states[pid]
		presets := sub(cfg["presets"])
		key := lightStatus(r, requirePlate && flag(p["awaiting_plate_clear"]), presets)
		if key == "finished" {
			if state.finished.IsZero() {
				state.finished = when
			}
			if timeout := time.Duration(num(cfg["finished_timeout_seconds"])) * time.Second; timeout > 0 && when.Sub(state.finished) >= timeout {
				key = "idle"
			}
		} else {
			state.finished = time.Time{}
		}
		base := strings.TrimRight(str(cfg["base_url"]), "/")
		preset := int(num(presets[key]))
		target := fmt.Sprintf("%s#%d", base, preset)
		if base != "" && preset > 0 && (target != state.target || !state.retry.IsZero() && !when.Before(state.retry)) {
			call, cancel := context.WithTimeout(ctx, 3*time.Second)
			e := wledPreset(call, base, preset)
			cancel()
			state.target = target
			state.retry = time.Time{}
			if e != nil {
				state.retry = when.Add(30 * time.Second)
			}
		} else if preset == 0 {
			state.target = ""
			state.retry = time.Time{}
		}
		states[pid] = state
	}
	for pid := range states {
		if !seen[pid] {
			delete(states, pid)
		}
	}
}
func (s *Service) lightPoll(ctx context.Context) {
	states := map[int64]lightState{}
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case when := <-t.C:
			s.syncLights(ctx, states, when)
		}
	}
}
