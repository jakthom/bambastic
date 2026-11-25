package production

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// HMS labels are the Bambu Studio tables distributed with Bambuddy. Action
// identifiers use the firmware's spelling and its three-character model prefix.
//
//go:embed catalog/hms_catalog.json
var faultData []byte

//go:embed catalog/hms_actions.json
var actionData []byte

var faults struct {
	HMS          map[string]string            `json:"hms"`
	Errors       map[string]string            `json:"error"`
	HMSByModel   map[string]map[string]string `json:"hms_by_model"`
	ErrorByModel map[string]map[string]string `json:"error_by_model"`
}
var faultActions map[string]map[string][]string

func init() {
	if e := json.Unmarshal(faultData, &faults); e != nil {
		panic(e)
	}
	if e := json.Unmarshal(actionData, &faultActions); e != nil {
		panic(e)
	}
}
func faultValue(v any) uint32 {
	if s, ok := v.(string); ok {
		n, _ := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(s), "0x"), 16, 32)
		return uint32(n)
	}
	return uint32(num(v))
}
func printerFaults(pr, rp row) []row {
	serial := strings.ToUpper(val(pr["serial_number"]))
	model := serial[:min(3, len(serial))]
	out := []row{}
	seen := map[string]bool{}
	appendFault := func(attr, code, level uint32, full, short string, merged, specific map[string]string) {
		if in(short, "0300400C", "0500400E") || seen[short] && len(full) == 8 {
			return
		}
		seen[short] = true
		actions := faultActions[model][full]
		if len(actions) == 0 {
			actions = faultActions[model][short]
		}
		if actions == nil {
			actions = []string{}
		}
		label, ok := specific[full]
		if !ok {
			label = merged[full]
		}
		var description any
		if label != "" {
			description = label
		}
		out = append(out, row{"code": fmt.Sprintf("0x%x", code), "attr": attr, "module": attr >> 24, "severity": level, "full_code": full, "description": description, "actions": actions, "job_id": rp["subtask_id"]})
	}
	entries, _ := rp["hms"].([]any)
	for _, v := range entries {
		obj, _ := v.(map[string]any)
		attr, code := faultValue(obj["attr"]), faultValue(obj["code"])
		if code < 0x4000 {
			continue
		}
		short := fmt.Sprintf("%04X%04X", attr>>16, code&0xffff)
		appendFault(attr, code, code>>16, fmt.Sprintf("%08X%08X", attr, code), short, faults.HMS, faults.HMSByModel[model])
	}
	attr := uint32(num(rp["print_error"]))
	if s, ok := rp["print_error"].(string); ok && strings.HasPrefix(s, "0x") {
		attr = faultValue(s)
	}
	code := attr & 0xffff
	if code >= 0x4000 {
		full := fmt.Sprintf("%08X", attr)
		level := map[uint32]uint32{4: 1, 8: 2, 12: 3}[code>>12]
		appendFault(attr, code, level, full, full, faults.Errors, faults.ErrorByModel[model])
	}
	return out
}
func (s *Service) hmsAction(w http.ResponseWriter, r *http.Request) (any, error) {
	pr, e := s.printerRow(r, "printers:control")
	if e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	k := pathID(r)
	s.mu.RLock()
	d := s.devices[k]
	var list []row
	var h hardware
	if d != nil {
		list = printerFaults(pr, d.report)
		h = d.h
	}
	s.mu.RUnlock()
	if h == nil || !h.Online() {
		return nil, fail(409, "Printer is disconnected")
	}
	action := val(m["action"])
	full := strings.ToUpper(val(m["print_error"]))
	var found row
	for _, v := range list {
		if v["full_code"] == full && slices.Contains(v["actions"].([]string), action) && (m["job_id"] == nil || val(m["job_id"]) == val(v["job_id"])) {
			found = v
			break
		}
	}
	if found == nil {
		return nil, fail(409, "Action unavailable for the active printer fault")
	}
	n, _ := strconv.ParseUint(full, 16, 64)
	decimal := strconv.FormatUint(n, 10)
	cmds := []row{}
	switch action {
	case "RESUME_PRINTING", "RESUME_PRINTING_DEFECTS", "RESUME_PRINTING_PROBELM_SOLVED", "PROBLEM_SOLVED_RESUME", "FILAMENT_LOAD_RESUME", "PROCEED", "DBL_CHECK_RESUME":
		cmds = append(cmds, packet("print", "resume", row{"param": ""}))
	case "STOP_PRINTING":
		cmds = append(cmds, packet("print", "stop", row{"param": ""}))
	case "IGNORE_RESUME", "IGNORE_NO_REMINDER_NEXT_TIME", "DONT_REMIND_NEXT_TIME":
		cmds = append(cmds, packet("print", "ignore", row{"param": "reserve", "err": decimal, "job_id": val(found["job_id"])}))
	case "NO_REMINDER_NEXT_TIME":
		cmds = append(cmds, packet("print", "idle_ignore", row{"err": decimal, "type": 0}))
	case "FILAMENT_EXTRUDED", "DBL_CHECK_DONE":
		cmds = append(cmds, packet("print", "ams_control", row{"param": "done"}))
	case "RETRY_FILAMENT_EXTRUDED", "CONTINUE", "RETRY_PROBLEM_SOLVED", "DBL_CHECK_RETRY":
		cmds = append(cmds, packet("print", "ams_control", row{"param": "resume"}))
	case "ABORT":
		cmds = append(cmds, packet("print", "ams_control", row{"param": "abort"}))
	case "OK_BUTTON", "DBL_CHECK_OK":
		cmds = append(cmds, packet("print", "clean_print_error", nil))
		if action == "DBL_CHECK_OK" {
			cmds = append(cmds, packet("system", "uiop", row{"name": "print_error", "action": "close", "source": 1, "type": "dialog", "err": full}))
		}
	case "REFRESH_NOZZLE":
		cmds = append(cmds, packet("print", "refresh_nozzle", nil))
	case "STOP_DRYING":
		cmds = append(cmds, packet("print", "auto_stop_ams_dry", nil))
	default:
		return row{"success": true, "message": "Continue on the printer display"}, nil
	}
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	for _, msg := range append(cmds, packet("pushing", "pushall", nil)) {
		if e = h.Publish(ctx, msg); e != nil {
			return nil, fail(502, "Printer did not receive fault action")
		}
	}
	return row{"success": true, "message": "Action sent to printer"}, nil
}
