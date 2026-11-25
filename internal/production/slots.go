package production

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

//go:embed catalog/filament_ids.json
var filamentCatalog []byte
var filamentIDs struct {
	Generic, Settings, Orca map[string]string
	Types                   []string
}
var filamentWord = regexp.MustCompile(`[^A-Z0-9+\-]+`)
var wireColor = regexp.MustCompile(`^[A-F0-9]{8}$`)
var shortPreset = regexp.MustCompile(`^[A-Za-z0-9]{1,8}$`)

func init() {
	if e := json.Unmarshal(filamentCatalog, &filamentIDs); e != nil {
		panic(e)
	}
	sort.SliceStable(filamentIDs.Types, func(i, j int) bool { return len(filamentIDs.Types[i]) > len(filamentIDs.Types[j]) })
}
func filamentType(name string) string {
	upper := strings.ToUpper(strings.TrimSpace(name))
	if upper == "NYLON" {
		return "PA"
	}
	words := filamentWord.Split(upper, -1)
	for _, t := range filamentIDs.Types {
		for i, w := range words {
			if w == t || i+1 < len(words) && w+"-"+words[i+1] == t || len(t) > 2 && (strings.HasSuffix(w, t) || strings.HasPrefix(w, t) && len(w) > len(t) && (w[len(t)] < 'A' || w[len(t)] > 'Z')) {
				return t
			}
		}
	}
	return strings.TrimSpace(name)
}
func filamentID(id string) (string, string) {
	if listed := filamentIDs.Settings[id]; listed != "" {
		return listed, id
	}
	if listed := filamentIDs.Orca[id]; listed != "" {
		return listed, ""
	}
	base, _, _ := strings.Cut(id, "_")
	switch {
	case strings.HasPrefix(base, "GFSNLS"):
		return strings.Replace(base, "GFSNLS", "GFSNL", 1), id
	case strings.HasPrefix(base, "GFSNL"):
		return base, ""
	case strings.HasPrefix(base, "GFS") && len(base) > 3 && base[3] >= 'A' && base[3] <= 'Z':
		return "GF" + base[3:], id
	case strings.HasPrefix(base, "PFUS") || strings.HasPrefix(base, "PFCN"):
		return "", id
	case shortPreset.MatchString(base) && (strings.HasPrefix(base, "GF") || strings.HasPrefix(base, "P") && !in(base, filamentIDs.Types...)):
		return base, ""
	}
	return "", ""
}

type amsSlot struct {
	wire, local, tray, global, extruder int64
	diameter                            string
	current                             row
}

func resolveSlot(pr, rp row, unit, local int64) (amsSlot, error) {
	a := amsSlot{wire: unit, local: local, tray: local, extruder: -1}
	if !dual(pr) {
		a.extruder = 0
	}
	model := modelName(pr["model"])
	wire := unit
	if unit == 6 && model == "A2L" {
		wire = 16
	}
	a.wire = wire
	ams, _ := rp["ams"].(map[string]any)
	us, _ := ams["ams"].([]any)
	var activeUnit row
	for _, raw := range us {
		u, _ := raw.(map[string]any)
		if id, e := strconv.ParseInt(val(u["id"]), 10, 32); e == nil && id == wire {
			activeUnit = row(u)
			break
		}
	}
	switch {
	case wire == 255 && model == "H3C" && local >= 0 && local <= 8:
		// The nine external inputs are separate virtual units. Their ids wrap
		// after the first two positions (254,255,247…253). Each has local slot 0.
		global := 254 + local
		if local >= 2 {
			global = 245 + local
		}
		a.wire, a.global, a.local, a.tray, a.extruder = global, global, 0, 0, 0
		for i, raw := range holders(rp) {
			t, _ := raw.(map[string]any)
			if t["id"] != nil && num(t["id"]) == global || t["id"] == nil && int64(i) == local {
				a.current = row(t)
				break
			}
		}
	case wire == 255 && local >= 0 && local <= 1:
		hs := holders(rp)
		a.tray = 254
		a.global = 254
		a.local = 0
		if len(hs) > 1 {
			a.wire = 254 + local
			a.tray = 0
			a.global = 254 + local
			a.extruder = 1 - local
		} else if local != 0 {
			return a, fail(422, "External holder unavailable")
		}
		if int64(len(hs)) > local {
			v, _ := hs[local].(map[string]any)
			a.current = row(v)
		}
	case wire >= 0 && wire <= 3 && local >= 0 && local < 4:
		a.global = wire*4 + local
	case wire == 16 && model == "A2L" && local >= 0 && local <= 3:
		a.global = 64 + local
	case wire == 32 && model == "H3C" && local >= 0 && local <= 8:
		a.global = local
	case wire >= 128 && wire <= 135 && local == 0:
		a.global = local
	default:
		return a, fail(422, "Unknown AMS slot")
	}
	if activeUnit != nil {
		if dual(pr) && activeUnit["info"] != nil {
			// The unit encodes a direct extruder or, on FTS machines, a switch inlet.
			base := 10
			if _, ok := activeUnit["info"].(string); ok {
				base = 16
			}
			n, e := strconv.ParseUint(strings.TrimPrefix(strings.ToLower(val(activeUnit["info"])), "0x"), base, 32)
			ext := (n >> 8) & 15
			if e == nil && ext < 2 {
				a.extruder = int64(ext)
			} else if e == nil && ext == 14 {
				dev, _ := rp["device"].(map[string]any)
				if _, installed := dev["fila_switch"].(map[string]any); installed && (n>>24)&15 < 2 {
					a.extruder = int64((n >> 24) & 15)
				}
			}
		}
		ts, _ := activeUnit["tray"].([]any)
		for _, raw := range ts {
			v, _ := raw.(map[string]any)
			if num(v["id"]) == local {
				a.current = row(v)
				break
			}
		}
	}
	a.diameter = val(rp["nozzle_diameter"])
	keys := []string{"left_nozzle_diameter"}
	if a.extruder == 1 {
		keys = []string{"right_nozzle_diameter", "nozzle_diameter_2"}
	}
	for _, key := range keys {
		if diameter := val(rp[key]); nozzleOK(diameter) {
			a.diameter = diameter
		}
	}
	dev, _ := rp["device"].(map[string]any)
	nozzles, _ := dev["nozzle"].(map[string]any)
	ns, _ := nozzles["info"].([]any)
	for _, raw := range ns {
		v, _ := raw.(map[string]any)
		if num(v["id"]) == a.extruder && v["diameter"] != nil {
			a.diameter = val(v["diameter"])
		}
	}
	if !nozzleOK(a.diameter) {
		a.diameter = "0.4"
	}
	return a, nil
}
func (s *Service) slotRoutes(rt register) {
	rt("POST", "/printers/{id}/slots/{ams}/{tray}/configure", s.slotCommand("configure"))
	rt("POST", "/printers/{id}/ams/{ams}/tray/{tray}/reset", s.slotCommand("reset"))
	rt("POST", "/printers/{id}/ams/{ams}/slot/{tray}/refresh", s.slotCommand("refresh"))
	rt("GET", "/printers/{id}/slots/{ams}/{tray}/spool-defaults", s.slotCommand("defaults"))
}
func (s *Service) slotCommand(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		perm := "printers:control"
		if action == "defaults" {
			perm = "printers:read"
		}
		pr, e := s.printerRow(r, perm)
		if e != nil {
			return nil, e
		}
		unit, e := bounded(r.PathValue("ams"), 0, 255)
		if e != nil {
			return nil, e
		}
		local, e := bounded(r.PathValue("tray"), 0, 8)
		if e != nil {
			return nil, e
		}
		s.mu.RLock()
		d := s.devices[pathID(r)]
		rp := row{}
		var h hardware
		if d != nil {
			for k, v := range d.report {
				rp[k] = v
			}
			h = d.h
		}
		s.mu.RUnlock()
		a, e := resolveSlot(pr, rp, int64(unit), int64(local))
		if e != nil {
			return nil, e
		}
		if action == "defaults" {
			return s.slotDefaults(ctx, pr, unit, local, a)
		}
		if h == nil || !h.Online() {
			return nil, fail(409, "Printer is disconnected")
		}
		commands := []row{}
		out := row{"success": true, "message": "Command sent to printer"}
		q := r.URL.Query()
		switch action {
		case "refresh":
			am, _ := rp["ams"].(map[string]any)
			if a.wire == 255 || am["tray_now"] == nil || num(am["tray_now"]) != 255 {
				return nil, fail(409, "Unload filament before reading the RFID tag")
			}
			commands = append(commands, packet("print", "ams_get_rfid", row{"ams_id": a.wire, "slot_id": a.local}))
		case "reset", "configure":
			fields := row{"ams_id": a.wire, "tray_id": a.tray, "slot_id": a.local, "tray_info_idx": "", "tray_type": "", "tray_sub_brands": "", "tray_color": "00000000", "nozzle_temp_min": 0, "nozzle_temp_max": 0}
			commands = append(commands, packet("print", "ams_filament_setting", fields))
			if action == "configure" {
				typ := filamentType(q.Get("tray_type"))
				color := strings.ToUpper(strings.TrimPrefix(q.Get("tray_color"), "#"))
				if len(color) == 6 {
					color += "FF"
				}
				if typ == "" || len(typ) > 50 || len(q.Get("tray_sub_brands")) > 150 || !wireColor.MatchString(color) {
					return nil, fail(422, "Invalid filament type, color or name")
				}
				low, e := bounded(q.Get("nozzle_temp_min"), 0, 350)
				if e != nil {
					return nil, e
				}
				high, e := bounded(q.Get("nozzle_temp_max"), low, 350)
				if e != nil {
					return nil, e
				}
				preset, derived := filamentID(q.Get("tray_info_idx"))
				setting := q.Get("setting_id")
				if setting == "" {
					setting = derived
				}
				if kp, kSetting := filamentID(q.Get("kprofile_filament_id")); kp != "" && kp != preset {
					preset = kp
					setting = q.Get("kprofile_setting_id")
					if setting == "" {
						setting = kSetting
					}
				}
				if preset == "" && q.Get("orca_profile_id") == "" && val(a.current["tray_type"]) == typ && val(a.current["tray_sub_brands"]) == q.Get("tray_sub_brands") {
					preset, _ = filamentID(val(a.current["tray_info_idx"]))
				}
				if preset == "" {
					preset = filamentIDs.Generic[strings.ToUpper(q.Get("tray_type"))]
					if preset == "" {
						preset = filamentIDs.Generic[typ]
					}
					if q.Get("orca_profile_id") != "" {
						out["orca_fallback_reason"] = "no_filament_id"
					}
				}
				if setting == "" {
					setting = preset
					if strings.HasPrefix(preset, "GFSNL") {
						setting = strings.Replace(preset, "GFSNL", "GFSNLS", 1)
					} else if strings.HasPrefix(preset, "GF") {
						setting = "GFS" + preset[2:]
					}
				}
				if len(setting) > 128 {
					return nil, fail(422, "Invalid filament setting ID")
				}
				fields["tray_type"] = typ
				fields["tray_color"] = color
				fields["tray_sub_brands"] = q.Get("tray_sub_brands")
				fields["nozzle_temp_min"] = low
				fields["nozzle_temp_max"] = high
				fields["tray_info_idx"] = preset
				if setting != "" {
					fields["setting_id"] = setting
				}
				idx := -1
				if q.Has("cali_idx") {
					idx, e = bounded(q.Get("cali_idx"), -1, 65535)
					if e != nil {
						return nil, e
					}
				}
				diam := q.Get("nozzle_diameter")
				if diam == "" {
					diam = a.diameter
				}
				if !nozzleOK(diam) {
					return nil, fail(422, "Invalid nozzle diameter")
				}
				calUnit := a.wire
				if unit == 255 {
					calUnit = a.global
				}
				commands = append(commands, packet("print", "extrusion_cali_sel", row{"cali_idx": idx, "nozzle_diameter": diam, "filament_id": preset, "ams_id": calUnit, "tray_id": a.global, "slot_id": a.local}))
				out["tray_info_idx"] = preset
				k, _ := decimal(q.Get("k_value"))
				out["k_value_unsent"] = k > 0 && idx < 0
			}
		}
		if action == "configure" || action == "reset" {
			d.drift.mu.Lock()
			defer d.drift.mu.Unlock()
			// The operator's newer explicit slot settings supersede automatic replays.
			if _, e = s.db.ExecContext(ctx, "DELETE FROM bb_slot_config WHERE assignment_id IN (SELECT id FROM spool_assignment WHERE printer_id=? AND ams_id=? AND tray_id=?)", pr["id"], unit, local); e != nil {
				return nil, e
			}
		}
		ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		for _, cmd := range append(commands, packet("pushing", "pushall", nil)) {
			if e = h.Publish(ctx, cmd); e != nil {
				return nil, fail(502, "Printer did not receive slot command")
			}
			if print, ok := cmd["print"].(row); ok && val(print["command"]) == "extrusion_cali_sel" {
				d.drift.chosen(trayKey{int64(unit), int64(local)}, num(print["cali_idx"]))
			}
		}
		return out, nil
	}
}
func (s *Service) slotDefaults(ctx context.Context, pr row, unit, slot int, a amsSlot) (row, error) {
	out := row{"slicer_filament": nil, "slicer_filament_name": nil, "cali_idx": nil, "k_value": nil, "profile_name": nil, "extruder": nil, "nozzle_diameter": a.diameter}
	if a.extruder >= 0 {
		out["extruder"] = a.extruder
	}
	remote, e := access.Setting(ctx, s.db, "spoolman_enabled", "false")
	if e != nil {
		return nil, e
	}
	table, assign, spid := "spool", "spool_assignment", "spool_id"
	if flag(remote) {
		table, assign, spid = "spoolman", "spoolman_slot_assignments", "spoolman_spool_id"
	}
	j, e := one(ctx, s.db, "SELECT "+spid+" spool FROM "+assign+" WHERE printer_id=? AND ams_id=? AND tray_id=?", pr["id"], unit, slot)
	if errors.Is(e, sql.ErrNoRows) {
		return out, nil
	}
	if e != nil {
		return nil, e
	}
	preset, e := one(ctx, s.db, "SELECT slicer_filament,slicer_filament_name FROM "+table+"_filament_preset WHERE "+spid+"=? AND lower(printer_model)=lower(?) AND nozzle_diameter IN (?,'') ORDER BY nozzle_diameter DESC LIMIT 1", j["spool"], pr["model"], a.diameter)
	if errors.Is(e, sql.ErrNoRows) && !flag(remote) {
		preset, e = one(ctx, s.db, "SELECT slicer_filament,slicer_filament_name FROM spool WHERE id=?", j["spool"])
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	for k, v := range preset {
		out[k] = v
	}
	if a.extruder >= 0 {
		kp, e := one(ctx, s.db, "SELECT cali_idx,k_value,name profile_name FROM "+table+"_k_profile WHERE "+spid+"=? AND printer_id=? AND extruder=? AND nozzle_diameter=? ORDER BY id DESC LIMIT 1", j["spool"], pr["id"], a.extruder, a.diameter)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		for k, v := range kp {
			out[k] = v
		}
	}
	return out, nil
}
