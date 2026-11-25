package management

import (
	"bambastic/internal/access"
	"math"
	"strconv"
	"time"
)

func archiveStats(r request) (any, error) {
	actor := access.From(r.Context())
	q := r.URL.Query()
	zone := time.Local
	if v := q.Get("tz"); v != "" {
		tz, e := time.LoadLocation(v)
		if e != nil {
			return nil, invalid("Unknown time zone")
		}
		zone = tz
	}
	filter := " WHERE l.completed_at IS NOT NULL"
	var args []any
	for _, key := range []string{"date_from", "date_to"} {
		val := q.Get(key)
		if val == "" {
			continue
		}
		t, e := time.ParseInLocation("2006-01-02", val, zone)
		if e != nil {
			return nil, invalid("Invalid date")
		}
		if key == "date_from" {
			filter += " AND julianday(COALESCE(l.started_at,l.created_at))>=julianday(?)"
		} else {
			filter += " AND julianday(COALESCE(l.started_at,l.created_at))<julianday(?)"
			t = t.AddDate(0, 0, 1)
		}
		args = append(args, t.UTC().Format(time.RFC3339))
	}
	if u := q.Get("created_by_id"); u != "" {
		if !actor.Has("stats:filter_by_user") {
			return nil, denied()
		}
		if u == "-1" {
			filter += " AND l.created_by_id IS NULL"
		} else {
			filter += " AND l.created_by_id=?"
			args = append(args, u)
		}
	}
	v, e := list(r.tx, "SELECT l.*,a.print_time_seconds estimate FROM print_log_entries l LEFT JOIN print_archives a ON a.id=l.archive_id"+filter, args...)
	if e != nil {
		return nil, e
	}
	success, failed, cancelled, total := 0, 0, 0, 0
	var seconds, grams, cost, energy, kwh, wear float64
	byPrinter, materials := map[string]int{}, map[string]int{}
	names := map[string]string{}
	for _, run := range v {
		pid := intID(run["printer_id"])
		if !actor.AllowsPrinter(pid) {
			continue
		}
		total++
		switch run["status"] {
		case "completed":
			success++
		case "failed", "aborted":
			failed++
		case "cancelled":
			cancelled++
		}
		seconds += num(run["duration_seconds"])
		grams += num(run["filament_used_grams"])
		cost += num(run["cost"])
		energy += num(run["energy_cost"])
		kwh += num(run["energy_kwh"])
		wear += num(run["wear_cost"])
		if pid > 0 {
			k := strconv.FormatInt(pid, 10)
			byPrinter[k]++
			names[k] = str(run["printer_name"])
		}
		if material := str(run["filament_type"]); material != "" {
			materials[material]++
		}
	}
	return row{"total_prints": total, "successful_prints": success, "failed_prints": failed, "cancelled_prints": cancelled, "total_print_time_hours": cents(seconds / 3600), "total_filament_grams": cents(grams), "total_cost": cents(cost), "total_energy_kwh": math.Round(kwh*1000) / 1000, "total_energy_cost": cents(energy), "total_wear_cost": cents(wear), "energy_data_warming_up": false, "prints_by_filament_type": materials, "prints_by_printer": byPrinter, "printer_names": names, "average_time_accuracy": nil, "time_accuracy_by_printer": map[string]float64{}}, nil
}
