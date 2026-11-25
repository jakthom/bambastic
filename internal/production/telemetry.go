package production

import (
	"bambastic/internal/access"
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func decimal(v any) (float64, bool) {
	n, e := strconv.ParseFloat(val(v), 64)
	return n, e == nil && !math.IsNaN(n) && !math.IsInf(n, 0)
}
func temperature(v any) any {
	n, ok := decimal(v)
	if !ok || n < -30 || n > 500 {
		return nil
	}
	return n
}
func temps(pr, r row) map[string]any {
	t := map[string]any{"nozzle": temperature(r["nozzle_temper"]), "nozzle_target": temperature(r["nozzle_target_temper"]), "bed": temperature(r["bed_temper"]), "bed_target": temperature(r["bed_target_temper"]), "chamber": temperature(r["chamber_temper"]), "chamber_target": temperature(r["chamber_target_temper"])}
	dev, _ := r["device"].(map[string]any)
	ext, _ := dev["extruder"].(map[string]any)
	info, _ := ext["info"].([]any)
	for _, raw := range info {
		x, _ := raw.(map[string]any)
		if x["temp"] == nil {
			continue
		}
		packed := num(x["temp"])
		if packed < 0 {
			continue
		}
		current := packed & 65535
		target := (packed >> 16) & 65535
		key := "nozzle"
		if dual(pr) && num(x["id"]) == 0 {
			key = "nozzle_2"
		}
		if current < 501 {
			t[key] = float64(current)
		}
		if target < 501 {
			t[key+"_target"] = float64(target)
		}
	}
	return t
}
func holders(r row) []any {
	if v, ok := r["vir_slot"].([]any); ok {
		return v
	}
	if v, ok := r["vt_tray"].([]any); ok {
		return v
	}
	if v, ok := r["vt_tray"].(map[string]any); ok {
		return []any{v}
	}
	return []any{}
}
func augmentState(v row, pr, r row) {
	v["firmware_version"] = r["firmware_version"]
	fw := versionText.FindString(val(r["firmware_version"]))
	model := modelName(pr["model"])
	v["supports_drying"] = dryerSupport(model, fw, true) == ""
	v["drying_screen_only"] = in(model, "P1P", "P1S")
	v["supports_drying_while_printing"] = concurrentFirmware[model] != "" && fw >= concurrentFirmware[model]
	if sub := val(r["subtask_id"]); sub != "" && sub != "0" {
		v["subtask_id"] = sub
	}
	v["vt_tray"] = holders(r)
	v["temperatures"] = temps(pr, r)
	v["supports_chamber_heater"] = heater(pr)
	v["dual_nozzle"] = dual(pr)
	v["speed_level"] = r["spd_lvl"]
	v["speed_percent"] = r["spd_mag"]
	v["sdcard"] = flag(r["sdcard"])
	v["wifi_signal"] = num(strings.TrimSuffix(val(r["wifi_signal"]), "dBm"))
	ams, _ := r["ams"].(map[string]any)
	v["tray_now"] = int64(255)
	if ams["tray_now"] != nil {
		v["tray_now"] = num(ams["tray_now"])
	}
	normalized := []any{}
	if raw, ok := ams["ams"].([]any); ok {
		for _, unit := range raw {
			orig, _ := unit.(map[string]any)
			u := map[string]any(dryerUnit(orig))
			n := num(u["id"])
			u["id"] = n
			u["is_ams_ht"] = n >= 128 && n <= 135
			u["serial_number"] = val(u["sn"])
			if t, ok := decimal(u["temp"]); ok {
				u["temp"] = t
			}
			if t, ok := decimal(u["humidity"]); ok {
				u["humidity"] = t
			}
			tray := []any{}
			if list, ok := u["tray"].([]any); ok {
				for _, raw := range list {
					before, _ := raw.(map[string]any)
					v := map[string]any{}
					for k, x := range before {
						v[k] = x
					}
					v["id"] = num(v["id"])
					v["remain"] = num(v["remain"])
					tray = append(tray, v)
				}
			}
			u["tray"] = tray
			normalized = append(normalized, u)
		}
	}
	v["ams"] = normalized
	v["ams_exists"] = len(normalized) > 0
	v["fans"] = map[string]any{"part": r["cooling_fan_speed"], "aux": r["big_fan1_speed"], "chamber": r["big_fan2_speed"]}
	v["chamber_light"] = false
	if lights, ok := r["lights_report"].([]any); ok {
		for _, l := range lights {
			obj, _ := l.(map[string]any)
			if obj["node"] == "chamber_light" && obj["mode"] == "on" {
				v["chamber_light"] = true
			}
		}
	}
	v["hms_errors"] = printerFaults(pr, r)
}
func (s *Service) sample(ctx context.Context, pr, r row) {
	k := num(pr["id"])
	ts := temps(pr, r)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	for _, kind := range []string{"nozzle", "nozzle_2", "bed", "chamber"} {
		if ts[kind] != nil || ts[kind+"_target"] != nil {
			if _, e = tx.ExecContext(ctx, "INSERT INTO printer_sensor_history(printer_id,sensor_kind,value,target) VALUES(?,?,?,?)", k, kind, ts[kind], ts[kind+"_target"]); e != nil {
				return
			}
		}
	}
	ams, _ := r["ams"].(map[string]any)
	if list, ok := ams["ams"].([]any); ok {
		for _, raw := range list {
			v, _ := raw.(map[string]any)
			if v["humidity"] == nil && v["temp"] == nil {
				continue
			}
			var percent any
			if value, ok := decimal(v["humidity_raw"]); ok && value >= 0 && value <= 100 {
				percent = value
			}
			if _, e = tx.ExecContext(ctx, "INSERT INTO ams_sensor_history(printer_id,ams_id,humidity,humidity_raw,temperature) VALUES(?,?,?,?,?)", k, num(v["id"]), v["humidity"], percent, temperature(v["temp"])); e != nil {
				return
			}
		}
	}
	for _, entry := range []struct{ table, setting string }{{"printer_sensor_history", "printer_sensor_history_retention_days"}, {"ams_sensor_history", "ams_history_retention_days"}} {
		days, _ := access.Setting(ctx, tx, entry.setting, "30")
		d := num(days)
		if d < 1 {
			continue
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM "+entry.table+" WHERE printer_id=? AND recorded_at<datetime('now',?)", k, fmt.Sprintf("-%d days", d))
		if e != nil {
			return
		}
	}
	_ = tx.Commit()
}
func (s *Service) history(ams bool) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		perm := "printer_sensor_history:read"
		if ams {
			perm = "ams_history:read"
		}
		if _, e := s.printerRow(r, perm); e != nil {
			return nil, e
		}
		k := pathID(r)
		hours := 24
		if q := r.URL.Query().Get("hours"); q != "" {
			var e error
			hours, e = bounded(q, 1, 168)
			if e != nil {
				return nil, e
			}
		}
		since := time.Now().UTC().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
		if ams {
			a, e := bounded(r.PathValue("ams"), 0, 255)
			if e != nil {
				return nil, e
			}
			filter := " FROM ams_sensor_history WHERE printer_id=? AND ams_id=? AND recorded_at>=?"
			out, e := one(ctx, s.db, "SELECT min(humidity) min_humidity,max(humidity) max_humidity,avg(humidity) avg_humidity,min(temperature) min_temperature,max(temperature) max_temperature,avg(temperature) avg_temperature"+filter, k, a, since)
			if e != nil {
				return nil, e
			}
			out["data"], e = records(ctx, s.db, "SELECT recorded_at,humidity,humidity_raw,temperature"+filter+" ORDER BY recorded_at,id", k, a, since)
			out["printer_id"] = k
			out["ams_id"] = a
			return out, e
		}
		series := []row{}
		wanted := r.URL.Query().Get("kinds")
		for _, kind := range []string{"nozzle", "nozzle_2", "bed", "chamber"} {
			if wanted != "" && !strings.Contains(","+wanted+",", ","+kind+",") {
				continue
			}
			filter := " FROM printer_sensor_history WHERE printer_id=? AND sensor_kind=? AND recorded_at>=?"
			values, e := records(ctx, s.db, "SELECT recorded_at,value,target"+filter+" ORDER BY recorded_at,id", k, kind, since)
			if e != nil {
				return nil, e
			}
			if len(values) == 0 {
				continue
			}
			stats, e := one(ctx, s.db, "SELECT min(value) min_value,max(value) max_value,avg(value) avg_value"+filter, k, kind, since)
			if e != nil {
				return nil, e
			}
			stats["sensor_kind"] = kind
			stats["data"] = values
			series = append(series, stats)
		}
		return row{"printer_id": k, "series": series}, nil
	}
}
