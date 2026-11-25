package operations

import (
	"context"
	"math"
	"strconv"
)

// Boundaries are priced from one meter. A reset counter, missing sample, or delayed start leaves energy unmeasured.
func (s *Service) energySamples(ctx context.Context) error {
	samples, e := s.list(ctx, "SELECT *, (julianday('now')-julianday(queued_at))*86400 AS age FROM bambastic_energy_samples WHERE state='pending' ORDER BY run_id,boundary DESC LIMIT 32")
	if e != nil {
		return e
	}
	for _, sample := range samples {
		rid := sample["run_id"]
		r, e := s.get(ctx, "print_log_entries", rid)
		if e != nil {
			return e
		}
		boundary := str(sample["boundary"])
		finished := r["completed_at"] != nil
		state := "unmeasured"
		if num(sample["age"]) <= 120 && (boundary == "start" && !finished || boundary == "end" && finished && r["energy_start_plug_id"] != nil) {
			query := "SELECT * FROM smart_plugs WHERE enabled AND printer_id=?"
			args := []any{r["printer_id"]}
			if boundary == "end" {
				query += " AND id=?"
				args = append(args, r["energy_start_plug_id"])
			}
			v, e := s.list(ctx, query+" ORDER BY controls_printer_power DESC,id", args...)
			if e != nil {
				return e
			}
			for _, p := range v {
				if p["plug_type"] == "homeassistant" && p["ha_energy_total_entity"] == nil || p["plug_type"] == "rest" && p["rest_energy_total_path"] == nil {
					continue
				}
				res, e := s.status(ctx, p)
				if e != nil {
					continue
				}
				reading := sub(res["energy"])["total"]
				if reading == nil {
					continue
				}
				value, e := strconv.ParseFloat(str(reading), 64)
				if e != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
					continue
				}
				if boundary == "start" {
					var rate any
					if price, e := strconv.ParseFloat(setting(ctx, s.db, "energy_cost_per_kwh"), 64); e == nil && price >= 0 {
						rate = price
					}
					_, e = s.db.ExecContext(ctx, "UPDATE print_log_entries SET energy_start_kwh=?,energy_start_price=?,energy_start_at=CURRENT_TIMESTAMP,energy_start_plug_id=? WHERE id=? AND completed_at IS NULL AND energy_start_kwh IS NULL", value, rate, p["id"], rid)
				} else {
					diff := value - num(r["energy_start_kwh"])
					if diff < 0 {
						break
					}
					var cost any
					if r["energy_start_price"] != nil {
						cost = math.Round(diff*num(r["energy_start_price"])*10000) / 10000
					}
					_, e = s.db.ExecContext(ctx, "UPDATE print_log_entries SET energy_kwh=?,energy_cost=? WHERE id=? AND energy_start_plug_id=?", diff, cost, rid, p["id"])
				}
				if e != nil {
					return e
				}
				state = "measured"
				break
			}
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE bambastic_energy_samples SET state=? WHERE run_id=? AND boundary=?", state, rid, boundary); e != nil {
			return e
		}
	}
	return nil
}
