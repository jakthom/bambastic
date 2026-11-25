package production

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var sequence atomic.Uint32

func packet(section, cmd string, fields row) row {
	if fields == nil {
		fields = row{}
	}
	fields["command"] = cmd
	fields["sequence_id"] = strconv.FormatUint(uint64(sequence.Add(1)), 10)
	return row{section: fields}
}
func line(format string, args ...any) row {
	return packet("print", "gcode_line", row{"param": fmt.Sprintf(format, args...)})
}
func dual(pr row) bool {
	if num(pr["nozzle_count"]) > 1 {
		return true
	}
	switch modelName(pr["model"]) {
	case "H2D", "H2DPRO", "H2C", "X2D", "O1D", "O1E", "O2D", "O1C", "O1C2", "N6":
		return true
	}
	return false
}
func heater(pr row) bool {
	if dual(pr) {
		return true
	}
	switch modelName(pr["model"]) {
	case "H2S", "H3C", "X1E", "O1S", "C13", "N10":
		return true
	}
	return false
}
func bounded(s string, min, max int) (int, error) {
	n, e := strconv.Atoi(s)
	if e != nil || n < min || n > max {
		return 0, fail(422, fmt.Sprintf("Expected an integer between %d and %d", min, max))
	}
	return n, nil
}
func (s *Service) operatorRoutes(rt register) {
	for _, a := range []string{"temperature/bed", "temperature/nozzle", "temperature/chamber", "print-speed", "chamber-light", "fan-speed", "airduct-mode", "ams-backup", "print-options", "ams/load", "ams/unload", "refresh-status", "select-extruder", "hms/clear", "bed-jog", "extruder-jog", "xy-jog", "home-axes", "calibration"} {
		rt("POST", "/printers/{id}/"+a, s.operate(a))
	}
	rt("GET", "/printers/{id}/cover", s.jobMedia("cover"))
	rt("GET", "/printers/{id}/print/objects", s.jobMedia("objects"))
	rt("POST", "/printers/{id}/print/skip-objects", s.jobMedia("skip"))
	rt("POST", "/printers/{id}/hms/execute-action", s.hmsAction)
	rt("GET", "/printers/{id}/diagnostic", s.diagnostic)
	rt("POST", "/printers/diagnostic", s.diagnostic)
	for _, act := range []string{"enable", "disable"} {
		rt("POST", "/printers/{id}/logging/"+act, s.logging)
	}
	rt("GET", "/printers/{id}/logging", s.logging)
	rt("DELETE", "/printers/{id}/logging", s.logging)
	rt("GET", "/printer-sensor-history/{id}", s.history(false))
	rt("GET", "/ams-history/{id}/{ams}", s.history(true))
	rt("GET", "/printers/{id}/slot-presets", s.slotPreset)
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		rt(m, "/printers/{id}/slot-presets/{ams}/{tray}", s.slotPreset)
	}
}
func (s *Service) operate(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		pr, e := s.printerRow(r, "printers:control")
		if e != nil {
			return nil, e
		}
		k := num(pr["id"])
		s.mu.RLock()
		d := s.devices[k]
		var h hardware
		var report row
		if d != nil {
			h = d.h
			report = row{}
			for key, x := range d.report {
				report[key] = x
			}
		}
		s.mu.RUnlock()
		if h == nil || !h.Online() {
			return nil, fail(409, "Printer is disconnected")
		}
		cmds, e := commands(action, r, pr, report)
		if e != nil {
			return nil, e
		}
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		for _, cmd := range cmds {
			if e = h.Publish(ctx, cmd); e != nil {
				return nil, fail(502, "Printer did not acknowledge command delivery")
			}
		}
		return row{"success": true, "message": "Command sent to printer"}, nil
	}
}
func commands(action string, r *http.Request, pr, rp row) ([]row, error) {
	q := r.URL.Query()
	cmds := []row{}
	model := modelName(pr["model"])
	switch action {
	case "home-axes", "bed-jog", "extruder-jog", "xy-jog", "calibration":
		if !idle(val(rp["gcode_state"])) {
			return nil, fail(409, "Printer must be idle for manual movement or calibration")
		}
		switch action {
		case "home-axes":
			if !in(q.Get("axes"), "", "xy", "z", "all") {
				return nil, fail(422, "Invalid home axes")
			}
			cmds = append(cmds, line("G28"))
		case "bed-jog", "extruder-jog":
			d, ok := decimal(q.Get("distance"))
			if !ok || d == 0 || math.Abs(d) > 200 || action == "extruder-jog" && math.Abs(d) > 100 {
				return nil, fail(422, "Invalid jog distance")
			}
			if action == "bed-jog" {
				cmds = append(cmds, line("G91\nG1 Z%.2f F600\nG90", d))
			} else {
				cmds = append(cmds, line("M83\nG1 E%.2f F300\nM82", d))
			}
		case "xy-jog":
			axes := []string{}
			for _, key := range []string{"x", "y"} {
				if !q.Has(key) {
					continue
				}
				n, ok := decimal(q.Get(key))
				if !ok || math.Abs(n) > 200 {
					return nil, fail(422, "Invalid jog distance")
				}
				if n != 0 {
					axes = append(axes, fmt.Sprintf("%s%.2f", strings.ToUpper(key), n))
				}
			}
			if len(axes) == 0 {
				return nil, fail(422, "Select a nonzero movement")
			}
			cmds = append(cmds, line("G91\nG1 %s F6000\nG90", strings.Join(axes, " ")))
		case "calibration":
			flags := 0
			for i, k := range []string{"bed_leveling", "vibration", "motor_noise", "nozzle_offset", "high_temp_heatbed"} {
				if flag(q.Get(k)) {
					if k == "nozzle_offset" && !dual(pr) {
						return nil, fail(422, "Nozzle offset calibration requires two nozzles")
					}
					flags |= 1 << (i + 1)
				}
			}
			if flags == 0 {
				return nil, fail(422, "Select a calibration option")
			}
			cmds = append(cmds, packet("print", "calibration", row{"option": flags}))
		}
	case "hms/clear":
		cmds = append(cmds, packet("print", "clean_print_error", nil), packet("pushing", "pushall", nil))
	case "refresh-status":
		cmds = append(cmds, packet("pushing", "pushall", nil))
	case "temperature/nozzle", "temperature/bed", "temperature/chamber":
		ceiling := 320
		if action == "temperature/bed" {
			ceiling = 140
		}
		if action == "temperature/chamber" {
			ceiling = 65
			if !heater(pr) {
				return nil, fail(422, "Printer has no active chamber heater")
			}
		}
		n, e := bounded(q.Get("target"), 0, ceiling)
		if e != nil {
			return nil, e
		}
		switch action {
		case "temperature/nozzle":
			nozzle := 0
			if q.Has("nozzle") {
				nozzle, e = bounded(q.Get("nozzle"), 0, 1)
				if e != nil {
					return nil, e
				}
			}
			if nozzle == 1 && !dual(pr) {
				return nil, fail(422, "Second nozzle unavailable")
			}
			cmds = append(cmds, line("M104 T%d S%d", nozzle, n))
		case "temperature/bed":
			cmds = append(cmds, line("M140 S%d", n))
		case "temperature/chamber":
			if (model == "H3C" || model == "N10") && n > 0 {
				cmds = append(cmds, packet("print", "set_airduct", row{"modeId": 1, "submode": -1}))
			}
			cmds = append(cmds, line("M141 S%d", n))
		}
	case "print-speed":
		n, e := bounded(q.Get("mode"), 1, 4)
		if e != nil {
			return nil, e
		}
		cmds = append(cmds, packet("print", "print_speed", row{"param": strconv.Itoa(n)}))
	case "select-extruder":
		n, e := bounded(q.Get("extruder"), 0, 1)
		if e != nil {
			return nil, e
		}
		if !dual(pr) {
			return nil, fail(422, "Printer has a single extruder")
		}
		cmds = append(cmds, packet("print", "select_extruder", row{"extruder_index": n}))
	case "chamber-light":
		on, e := strconv.ParseBool(q.Get("on"))
		if e != nil {
			return nil, fail(422, "Expected on=true or false")
		}
		mode := "off"
		if on {
			mode = "on"
		}
		for _, node := range []string{"chamber_light", "chamber_light2"} {
			cmds = append(cmds, packet("system", "ledctrl", row{"led_node": node, "led_mode": mode, "led_on_time": 500, "led_off_time": 500, "loop_times": 0, "interval_time": 0}))
		}
	case "fan-speed":
		fan, ok := map[string]int{"part": 1, "aux": 2, "chamber": 3, "aux2": 10}[q.Get("fan")]
		if !ok {
			return nil, fail(422, "Unknown fan")
		}
		if fan == 10 && !hasAirPart(rp, 10) {
			return nil, fail(409, "Second auxiliary fan unavailable")
		}
		n, e := bounded(q.Get("speed"), 0, 100)
		if e != nil {
			return nil, e
		}
		cmds = append(cmds, line("M106 P%d S%d", fan, int(math.Round(float64(n)*255/100))))
	case "airduct-mode":
		mode, ok := map[string]int{"heating": 1, "cooling": 0}[q.Get("mode")]
		if !ok {
			return nil, fail(422, "Unknown airduct mode")
		}
		cmds = append(cmds, packet("print", "set_airduct", row{"modeId": mode, "submode": -1}))
	case "ams-backup", "print-options":
		on, e := strconv.ParseBool(q.Get("enabled"))
		if e != nil {
			return nil, fail(422, "Expected enabled=true or false")
		}
		if action == "ams-backup" {
			cmds = append(cmds, packet("print", "print_option", row{"auto_switch_filament": on}))
			break
		}
		module := q.Get("module_name")
		switch module {
		case "auto_recovery_step_loss":
			cmds = append(cmds, packet("print", "print_option", row{"auto_recovery": on}))
		case "spaghetti_detector", "first_layer_inspector", "printing_monitor", "buildplate_marker_detector", "allow_skip_parts", "pileup_detector", "clump_detector", "airprint_detector":
			sensitivity := q.Get("sensitivity")
			if sensitivity == "" {
				sensitivity = "medium"
			}
			if sensitivity != "medium" && sensitivity != "low" && sensitivity != "high" && sensitivity != "never_halt" {
				return nil, fail(422, "Unknown sensitivity")
			}
			fields := row{"module_name": module, "control": on, "enable": on, "print_halt": true}
			if sensitivity != "never_halt" {
				fields["halt_print_sensitivity"] = sensitivity
			}
			cmds = append(cmds, packet("xcam", "xcam_control_set", fields))
		default:
			return nil, fail(422, "Unknown print option")
		}
	case "ams/load", "ams/unload":
		cmd, e := trayChange(r, pr, rp, action == "ams/load")
		if e != nil {
			return nil, e
		}
		cmds = append(cmds, cmd)
	default:
		return nil, errors.New("unknown device control")
	}
	return cmds, nil
}
func hasAirPart(r row, n int64) bool {
	air, _ := r["airduct"].(map[string]any)
	parts, _ := air["parts"].([]any)
	for _, x := range parts {
		p, _ := x.(map[string]any)
		if num(p["id"]) == n {
			return true
		}
	}
	return false
}
func trayChange(r *http.Request, pr, rp row, load bool) (row, error) {
	q := r.URL.Query()
	ams, _ := rp["ams"].(map[string]any)
	now := -1
	if ams["tray_now"] != nil {
		now = int(num(ams["tray_now"]))
	}
	requested := now
	if load || q.Has("tray_id") {
		var e error
		requested, e = bounded(q.Get("tray_id"), 0, 255)
		if e != nil {
			return nil, e
		}
	}
	unit, slot, target := requested/4, requested%4, requested
	currentTemp, targetTemp := -1, -1
	model := modelName(pr["model"])
	switch {
	case requested >= 0 && requested < 16:
	case requested >= 24 && requested < 28 && (model == "A2L" || model == "N9"):
		unit, slot, target = 16, requested-24, 16
	case requested >= 128 && requested <= 135:
		unit, slot = requested, 0
	case requested == 254:
		unit, slot = 255, 254
	case requested == 255:
		if load && !dual(pr) {
			return nil, fail(422, "Right external holder requires dual extruders")
		}
		unit, slot = 255, 0
		currentTemp = int(num(rp["nozzle_temper"]))
		targetTemp = currentTemp
	case requested >= 28 && requested <= 36 && (model == "H3C" || model == "N10"):
		slot = requested - 28
		unit = 32
		target = 32
		if !load {
			target = 255
			slot = 255 - slot
		}
		rack, _ := ams["ams"].([]any)
		var wanted map[string]any
		for _, raw := range rack {
			u, _ := raw.(map[string]any)
			if num(u["id"]) != 32 {
				continue
			}
			trays, _ := u["tray"].([]any)
			for _, x := range trays {
				t, _ := x.(map[string]any)
				if num(t["id"]) == int64(requested-28) {
					wanted = t
				}
			}
		}
		if wanted == nil {
			return nil, fail(409, "Rack slot unavailable")
		}
		targetTemp = int((num(wanted["nozzle_temp_min"]) + num(wanted["nozzle_temp_max"])) / 2)
		if targetTemp <= 0 {
			targetTemp = -1
		}
		return packet("print", "ams_change_filament", row{"ams_id": unit, "slot_id": slot, "target": target, "extruder_id": 0, "curr_temp": -1, "tar_temp": targetTemp}), nil
	default:
		return nil, fail(422, "Unaddressable tray")
	}
	if !load {
		if requested != now && q.Has("tray_id") {
			return nil, fail(409, "Cannot confirm the named tray is loaded")
		}
		slot, target = 255, 255
		currentTemp = int(num(rp["nozzle_temper"]))
		if currentTemp < 180 {
			currentTemp = 210
		}
		targetTemp = currentTemp
	}
	fields := row{"ams_id": unit, "slot_id": slot, "target": target, "curr_temp": currentTemp, "tar_temp": targetTemp}
	if load && q.Has("extruder_id") {
		n, e := bounded(q.Get("extruder_id"), 0, 1)
		if e != nil {
			return nil, e
		}
		if !dual(pr) && n != 0 {
			return nil, fail(422, "Extruder unavailable")
		}
		fields["extruder_id"] = n
	}
	return packet("print", "ams_change_filament", fields), nil
}
func (s *Service) slotPreset(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := "printers:read"
	if r.Method != "GET" {
		perm = "printers:control"
	}
	if _, e := s.printerRow(r, perm); e != nil {
		return nil, e
	}
	k := pathID(r)
	ctx := r.Context()
	if r.PathValue("ams") == "" {
		return s.presetsFor(ctx, k)
	}
	ams, e := bounded(r.PathValue("ams"), 0, 255)
	if e != nil {
		return nil, e
	}
	tray, e := bounded(r.PathValue("tray"), 0, 255)
	if e != nil {
		return nil, e
	}
	if r.Method == "DELETE" {
		_, e = s.db.ExecContext(ctx, "DELETE FROM slot_preset_mappings WHERE printer_id=? AND ams_id=? AND tray_id=?", k, ams, tray)
		return row{"success": true}, e
	}
	if r.Method == "PUT" {
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		for _, key := range []string{"preset_id", "preset_name", "preset_source"} {
			if strings.TrimSpace(val(m[key])) == "" {
				return nil, fail(422, "Missing "+key)
			}
		}
		_, e = s.db.ExecContext(ctx, "INSERT INTO slot_preset_mappings(printer_id,ams_id,tray_id,preset_id,preset_name,preset_source,tray_info_idx) VALUES(?,?,?,?,?,?,?) ON CONFLICT(printer_id,ams_id,tray_id) DO UPDATE SET preset_id=excluded.preset_id,preset_name=excluded.preset_name,preset_source=excluded.preset_source,tray_info_idx=excluded.tray_info_idx,updated_at=CURRENT_TIMESTAMP", k, ams, tray, m["preset_id"], m["preset_name"], m["preset_source"], m["tray_info_idx"])
		if e != nil {
			return nil, e
		}
	}
	return one(ctx, s.db, "SELECT * FROM slot_preset_mappings WHERE printer_id=? AND ams_id=? AND tray_id=?", k, ams, tray)
}
