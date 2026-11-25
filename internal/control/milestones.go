package control

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/releases"
)

func (s *Service) milestones(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/sponsor-prompt/check", s.sponsor)
	m.HandleFunc("POST /api/v1/sponsor-prompt/dismiss", s.sponsor)
}
func (s *Service) sponsor(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:read") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	var uid any
	if p.ID > 0 {
		uid = p.ID
	}
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		httpx.Error(w, 500, "Cannot read milestones")
		return
	}
	defer tx.Rollback()
	_, e = tx.Exec(`INSERT INTO sponsor_toast_state(user_id,milestones_seen,last_seen_version) SELECT ?,'[]',? WHERE NOT EXISTS(SELECT 1 FROM sponsor_toast_state WHERE user_id IS ?)`, uid, releases.Version, uid)
	var id int64
	var raw, ver string
	var cooled bool
	if e == nil {
		e = tx.QueryRow("SELECT id,milestones_seen,COALESCE(last_seen_version,''),COALESCE(julianday(last_shown_at)>julianday('now','-14 days'),0) FROM sponsor_toast_state WHERE user_id IS ? LIMIT 1", uid).Scan(&id, &raw, &ver, &cooled)
	}
	if e != nil {
		httpx.Error(w, 500, "Cannot load milestones")
		return
	}
	var seen []string
	json.Unmarshal([]byte(raw), &seen)
	families := []struct {
		name       string
		thresholds []int
	}{{"prints", []int{5000, 2500, 1000, 500, 100, 25, 10}}, {"archives", []int{1000, 250, 50, 10, 5}}, {"cost", []int{1000, 500, 100, 50, 25}}}
	if r.Method == "POST" {
		var req struct{ Milestone string }
		if !httpx.Decode(w, r, &req) {
			return
		}
		valid := req.Milestone == "version-update" || req.Milestone == "anniversary-1"
		for _, f := range families {
			for _, n := range f.thresholds {
				valid = valid || req.Milestone == fmt.Sprintf("%s-%d", f.name, n)
			}
		}
		if !valid {
			httpx.Error(w, 422, "Unknown milestone")
			return
		}
		if !slices.Contains(seen, req.Milestone) && req.Milestone != "version-update" {
			seen = append(seen, req.Milestone)
		}
		b, _ := json.Marshal(seen)
		_, e = tx.Exec("UPDATE sponsor_toast_state SET milestones_seen=?,last_seen_version=?,last_shown_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?", string(b), releases.Version, id)
		if e == nil {
			e = tx.Commit()
		}
		if e != nil {
			httpx.Error(w, 500, "Cannot dismiss milestone")
			return
		}
		w.WriteHeader(204)
		return
	}
	out := obj{"show": false}
	if !cooled {
		var days, prints, archives, cost float64
		e = tx.QueryRow(`SELECT COALESCE(julianday('now')-julianday((SELECT min(created_at) FROM users WHERE ? IS NULL OR id=?)),0),(SELECT COUNT(*) FROM print_log_entries WHERE status='completed' AND created_by_id IS ?),(SELECT COUNT(*) FROM print_archives WHERE created_by_id IS ?),(SELECT COALESCE(SUM(COALESCE(cost,0)+COALESCE(energy_cost,0)),0) FROM print_log_entries WHERE created_by_id IS ?)`, uid, uid, uid, uid, uid).Scan(&days, &prints, &archives, &cost)
		if e != nil {
			httpx.Error(w, 500, "Cannot count milestones")
			return
		}
		if days >= 365 && !slices.Contains(seen, "anniversary-1") {
			out = obj{"show": true, "milestone": "anniversary-1", "family": "anniversary"}
		} else {
			counts := []float64{prints, archives, cost}
			found := false
			for i, f := range families {
				for _, n := range f.thresholds {
					key := fmt.Sprintf("%s-%d", f.name, n)
					if counts[i] >= float64(n) && !slices.Contains(seen, key) {
						field := "count"
						if f.name == "cost" {
							field = "total"
						}
						out = obj{"show": true, "milestone": key, "family": f.name, "threshold": n, "payload": obj{field: counts[i]}}
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if !found && ver != "" && ver != releases.Version {
				out = obj{"show": true, "milestone": "version-update", "family": "version-update", "payload": obj{"from": ver, "to": releases.Version}}
			}
		}
	}
	if e = tx.Commit(); e != nil {
		httpx.Error(w, 500, "Cannot read milestone")
		return
	}
	httpx.JSON(w, 200, out)
}
