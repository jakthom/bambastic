package production

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

const assignmentQuery = `SELECT c.*, a.printer_id,a.ams_id,a.tray_id,a.spool_id,
	p.model,p.serial_number,sp.material,sp.subtype,sp.brand,sp.rgba,sp.tag_uid,sp.tray_uuid,
	sp.slicer_filament,sp.slicer_filament_name,sp.nozzle_temp_min,sp.nozzle_temp_max
	FROM bb_slot_config c JOIN spool_assignment a ON a.id=c.assignment_id
	JOIN spool sp ON sp.id=a.spool_id AND sp.archived_at IS NULL
	JOIN printers p ON p.id=a.printer_id AND p.is_active AND p.serial_number=c.serial
	WHERE c.assignment_id=? AND c.token=?`

// ConfigureNativeAssignment is called after inventory commits a manual placement.
// The token distinguishes a new placement even when SQLite recycles a deleted id.
func (s *Service) ConfigureNativeAssignment(ctx context.Context, id int64, token string) bool {
	return s.applyAssignment(ctx, id, token, true)
}
func (s *Service) replayAssignments(ctx context.Context, printer int64) {
	rs, e := records(ctx, s.db, "SELECT c.assignment_id,c.token FROM bb_slot_config c JOIN spool_assignment a ON a.id=c.assignment_id WHERE a.printer_id=? AND c.phase<>'confirmed' ORDER BY c.due,c.assignment_id", printer)
	if e != nil {
		return
	}
	for _, r := range rs {
		if ctx.Err() != nil {
			return
		}
		s.applyAssignment(ctx, num(r["assignment_id"]), val(r["token"]), false)
	}
}
func (s *Service) assignmentActor(ctx context.Context, j row) (access.Principal, bool) {
	mode, e := access.Setting(ctx, s.db, "spoolman_enabled", "false")
	if e != nil || flag(mode) {
		return access.Principal{}, false
	}
	owner, key := num(j["owner"]), num(j["key_id"])
	if owner == 0 && key == 0 {
		v, e := access.Setting(ctx, s.db, "auth_enabled", "false")
		if e != nil || flag(v) {
			return access.Principal{}, false
		}
	}
	p, e := s.durableActor(ctx, owner, key)
	return p, e == nil && p.Has("inventory:update") && p.AllowsPrinter(num(j["printer_id"]))
}
func (s *Service) assignmentDevice(j row) (*liveDevice, hardware, row) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[num(j["printer_id"])]
	if d == nil || d.h == nil || !d.h.Online() || val(d.printer["serial_number"]) != val(j["serial"]) {
		return nil, nil, nil
	}
	return d, d.h, maps.Clone(d.kReport)
}
func assignmentSlot(pr, r row, j row) (kCandidate, bool) {
	key := trayKey{num(j["ams_id"]), num(j["tray_id"])}
	if c, ok := kCandidates(pr, r)[key]; ok {
		return c, true
	}
	// A manual declaration is valid when an older firmware omits tray state.
	a, e := resolveSlot(pr, r, key[0], key[1])
	return kCandidate{slot: a, report: a.current}, e == nil
}
func wrongSpool(j, r row) bool {
	for _, field := range []string{"tag_uid", "tray_uuid"} {
		want, live := spoolTag(j[field]), spoolTag(r[field])
		if want != "" && live != "" && want != live {
			return true
		}
	}
	return false
}
func definitelyEmpty(t row) bool {
	return !flag(t["exists"]) && (num(t["state"]) == 9 || num(t["state"]) == 10)
}
func loaded(t row) bool {
	return !definitelyEmpty(t) && (flag(t["exists"]) || num(t["state"]) == 11 || val(t["tray_type"]) != "")
}
func scalar(v any) string {
	if a, ok := v.([]any); ok && len(a) > 0 {
		return val(a[0])
	}
	return val(v)
}

type spoolConfig struct {
	mapping, name, source string
	material, calibration row
	typ, color, preset    string
}

func (s *Service) assignmentSettings(ctx context.Context, actor access.Principal, j row, c kCandidate, link *calibrationLink) (spoolConfig, error) {
	a := c.slot
	saved, e := one(ctx, s.db, `SELECT slicer_filament,slicer_filament_name FROM spool_filament_preset WHERE spool_id=? AND lower(printer_model)=lower(?) AND nozzle_diameter IN (?,'') ORDER BY nozzle_diameter DESC LIMIT 1`, j["spool_id"], j["model"], a.diameter)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return spoolConfig{}, e
	}
	ref, label := val(j["slicer_filament"]), val(j["slicer_filament_name"])
	if saved != nil && val(saved["slicer_filament"]) != "" {
		ref, label = val(saved["slicer_filament"]), val(saved["slicer_filament_name"])
	}
	typ := filamentType(val(j["material"]))
	brand := strings.Join(strings.Fields(strings.Join([]string{val(j["brand"]), val(j["material"]), val(j["subtype"])}, " ")), " ")
	preset, setting := filamentID(ref)
	var detail row
	base, _, _ := strings.Cut(ref, "_")
	if id, e := strconv.ParseInt(strings.TrimPrefix(ref, "local_"), 10, 64); e == nil && id > 0 {
		loc, e := one(ctx, s.db, "SELECT name,filament_type,setting FROM local_presets WHERE id=? AND preset_type='filament'", id)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return spoolConfig{}, e
		}
		if loc != nil {
			detail, _ = unpack(loc["setting"]).(map[string]any)
			if val(loc["filament_type"]) != "" {
				typ = filamentType(val(loc["filament_type"]))
			}
			if val(loc["name"]) != "" {
				label = val(loc["name"])
			}
		}
	} else if s.ResolveSlotPreset != nil && (strings.HasPrefix(base, "PFUS") || strings.HasPrefix(base, "PFCN") || strings.HasPrefix(base, "GFS") && preset == "") {
		detail, _ = s.ResolveSlotPreset(ctx, actor, "cloud", base, "filament")
	} else if s.ResolveSlotPreset != nil && (strings.HasPrefix(ref, "orca:") || strings.HasPrefix(ref, "orca_cloud:")) {
		_, id, _ := strings.Cut(ref, ":")
		detail, _ = s.ResolveSlotPreset(ctx, actor, "orca_cloud", id, "filament")
	}
	if len(detail) == 0 && preset == "" && (strings.HasPrefix(ref, "PFUS") || strings.HasPrefix(ref, "PFCN")) {
		setting = ""
	}
	if v := scalar(detail["filament_type"]); v != "" {
		typ = filamentType(v)
	}
	if raw := scalar(detail["filament_id"]); raw != "" {
		if id, derived := filamentID(raw); id != "" {
			preset = id
			if setting == "" {
				setting = derived
			}
		}
	}
	if label != "" {
		brand, _, _ = strings.Cut(label, " @")
	}
	if preset == "" && strings.EqualFold(val(c.report["tray_type"]), typ) && strings.EqualFold(val(c.report["tray_sub_brands"]), brand) {
		preset, _ = filamentID(val(c.report["tray_info_idx"]))
	}
	if preset == "" {
		for _, key := range []string{val(j["material"]) + " " + val(j["subtype"]), val(j["material"]), typ} {
			if id := filamentIDs.Generic[strings.ToUpper(strings.TrimSpace(key))]; id != "" {
				preset = id
				break
			}
		}
	}
	if support := map[string]string{"GFS00": "PLA-S", "GFS01": "PA-S", "GFS02": "PLA-S", "GFS03": "PA-S", "GFS05": "PLA-S", "GFS06": "ABS"}[preset]; support != "" {
		typ = support
	} else if (typ == "PLA" || typ == "PA") && strings.Contains(strings.ToUpper(brand), "SUPPORT") {
		typ += "-S"
	}
	if setting == "" && strings.HasPrefix(preset, "GF") {
		setting = "GFS" + preset[2:]
		if strings.HasPrefix(preset, "GFSNL") {
			setting = "GFSNLS" + preset[5:]
		}
	}
	color := strings.ToUpper(strings.TrimPrefix(val(j["rgba"]), "#"))
	if color == "" {
		color = "FFFFFFFF"
	}
	if len(color) == 6 {
		color += "FF"
	}
	ranges := map[string][2]int64{"PLA": {190, 230}, "PETG": {220, 260}, "ABS": {240, 270}, "ASA": {240, 270}, "TPU": {200, 240}, "PA": {260, 290}, "PC": {250, 280}, "PVA": {190, 210}, "PLA-CF": {210, 240}, "PETG-CF": {240, 270}, "PA-CF": {270, 300}}
	temps := [2]int64{200, 240}
	baseType, _, _ := strings.Cut(typ, "-")
	for _, key := range []string{strings.ToUpper(val(j["material"])), typ, baseType} {
		if ts, ok := ranges[key]; ok {
			temps = ts
			break
		}
	}
	for i, key := range []string{"nozzle_temp_min", "nozzle_temp_max"} {
		if j[key] != nil {
			temps[i] = num(j[key])
		}
	}
	if typ == "" || len(typ) > 50 || len(brand) > 300 || len(setting) > 150 || !wireColor.MatchString(color) || temps[0] < 0 || temps[0] > temps[1] || temps[1] > 350 {
		return spoolConfig{}, errors.New("invalid spool configuration")
	}
	idx := int64(-1)
	profiles, e := records(ctx, s.db, "SELECT cali_idx,setting_id,nozzle_type FROM spool_k_profile WHERE spool_id=? AND printer_id=? AND nozzle_diameter=? AND cali_idx BETWEEN 0 AND 65535 ORDER BY CASE WHEN extruder=? THEN 0 ELSE 1 END,id DESC", j["spool_id"], j["printer_id"], a.diameter, a.extruder)
	if e != nil {
		return spoolConfig{}, e
	}
	for _, kp := range profiles {
		if f := nozzleFlow(kp["nozzle_type"]); f != "" && c.flow != "" && f != c.flow {
			continue
		}
		idx = num(kp["cali_idx"])
		if v := val(kp["setting_id"]); v != "" {
			setting = v
			if id, _ := filamentID(v); id != "" {
				preset = id
			}
		}
		break
	}
	// Firmware binds the calibration index to a filament ID. A cached table
	// from this connection carries its actual ID even for a local/custom preset.
	if idx >= 0 {
		link.mu.Lock()
		for i, raw := range link.tables[a.diameter] {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			entry := int64(i)
			if item["cali_idx"] != nil {
				entry = num(item["cali_idx"])
			}
			if entry != idx || item["extruder_id"] != nil && a.extruder >= 0 && num(item["extruder_id"]) != a.extruder {
				continue
			}
			if p, _ := filamentID(val(item["filament_id"])); p != "" {
				preset = p
			}
			if v := val(item["setting_id"]); v != "" && len(v) <= 128 {
				setting = v
			}
			break
		}
		link.mu.Unlock()
	}
	unit := a.wire
	if num(j["ams_id"]) == 255 {
		unit = a.global
	}
	m := row{"ams_id": a.wire, "tray_id": a.tray, "slot_id": a.local, "tray_type": typ, "tray_sub_brands": brand, "tray_color": color, "tray_info_idx": preset, "nozzle_temp_min": temps[0], "nozzle_temp_max": temps[1]}
	if setting != "" {
		m["setting_id"] = setting
	}
	k := row{"ams_id": unit, "tray_id": a.global, "slot_id": a.local, "cali_idx": idx, "filament_id": preset, "nozzle_diameter": a.diameter}
	mapping, source := setting, "cloud"
	if mapping == "" {
		mapping = preset
	}
	if id, e := strconv.ParseInt(strings.TrimPrefix(ref, "local_"), 10, 64); e == nil && id > 0 {
		mapping = "local_" + strconv.FormatInt(id, 10)
		source = "local"
	}
	if len(mapping) > 100 {
		mapping = ""
	}
	name := label
	if name == "" {
		name = brand
	}
	if len([]rune(name)) > 200 {
		name = string([]rune(name)[:200])
	}
	return spoolConfig{material: packet("print", "ams_filament_setting", m), calibration: packet("print", "extrusion_cali_sel", k), typ: typ, color: color, preset: preset, mapping: mapping, name: name, source: source}, nil
}

func (s *Service) applyAssignment(ctx context.Context, id int64, token string, manual bool) bool {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	j, e := one(ctx, s.db, assignmentQuery, id, token)
	if e != nil {
		return false
	}
	d, h, r := s.assignmentDevice(j)
	if h == nil {
		return false
	}
	d.drift.mu.Lock()
	defer d.drift.mu.Unlock()
	// Other clients or the replay loop may have finished while acquiring the slot.
	j, e = one(ctx, s.db, assignmentQuery, id, token)
	if e != nil {
		return false
	}
	d2, h2, r := s.assignmentDevice(j)
	if d2 != d || h2 != h {
		return false
	}
	c, ok := assignmentSlot(d.printer, r, j)
	if !ok || wrongSpool(j, c.report) {
		return false
	}
	actor, ok := s.assignmentActor(ctx, j)
	if !ok {
		return false
	}
	report := c.report
	if j["phase"] == "confirmed" {
		return true
	}
	sent := j["phase"] == "sent"
	if definitelyEmpty(report) {
		if sent {
			_, _ = s.db.ExecContext(ctx, "UPDATE bb_slot_config SET phase='pending',retries=0,due=0 WHERE assignment_id=? AND token=?", id, token)
		}
		return false
	}
	if sent && loaded(report) && strings.EqualFold(val(report["tray_type"]), val(j["expected_type"])) && rgb(report["tray_color"]) == rgb(j["expected_color"]) && (val(j["expected_preset"]) == "" || val(report["tray_info_idx"]) == val(j["expected_preset"])) {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return false
		}
		defer tx.Rollback()
		res, e := tx.ExecContext(ctx, "UPDATE bb_slot_config SET phase='confirmed' WHERE assignment_id=? AND token=?", id, token)
		if e != nil {
			return false
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false
		}
		if _, e = tx.ExecContext(ctx, "UPDATE spool_assignment SET fingerprint_type=?,fingerprint_color=? WHERE id=?", report["tray_type"], report["tray_color"], id); e != nil {
			return false
		}
		return tx.Commit() == nil
	}
	if !manual && (!idle(val(r["gcode_state"])) || !loaded(report)) {
		return false
	}
	if num(j["due"]) > time.Now().Unix() || sent && num(j["retries"]) >= 3 {
		return sent
	}
	opts, e := s.assignmentSettings(ctx, actor, j, c, &d.calibration)
	if e != nil {
		return false
	}
	if _, ok = s.assignmentActor(ctx, j); !ok {
		return false
	}
	d2, h2, r = s.assignmentDevice(j)
	nowSlot, ok := assignmentSlot(d.printer, r, j)
	if d2 != d || h2 != h || !ok || wrongSpool(j, nowSlot.report) || definitelyEmpty(nowSlot.report) || c.slot.diameter != nowSlot.slot.diameter || c.slot.extruder != nowSlot.slot.extruder || !manual && (!idle(val(r["gcode_state"])) || !loaded(nowSlot.report)) {
		return false
	}
	// Acquire the DB writer after the original assignment committed. A move,
	// removal or printer replacement must serialize with these two publications.
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE bb_slot_config SET due=? WHERE assignment_id=? AND token=? AND serial=(SELECT p.serial_number FROM printers p JOIN spool_assignment a ON a.printer_id=p.id JOIN spool sp ON sp.id=a.spool_id WHERE a.id=? AND p.is_active AND sp.archived_at IS NULL) AND due<=?", time.Now().Unix()+15, id, token, id, time.Now().Unix())
	if e != nil {
		return false
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false
	}
	if e = h.Publish(ctx, opts.material); e == nil {
		e = h.Publish(ctx, opts.calibration)
	}
	if e != nil {
		_, _ = tx.ExecContext(ctx, "UPDATE bb_slot_config SET phase='pending' WHERE assignment_id=? AND token=?", id, token)
		_ = tx.Commit()
		return false
	}
	if opts.mapping != "" {
		_, e = tx.ExecContext(ctx, "INSERT INTO slot_preset_mappings(printer_id,ams_id,tray_id,preset_id,preset_name,preset_source,tray_info_idx) VALUES(?,?,?,?,?,?,?) ON CONFLICT(printer_id,ams_id,tray_id) DO UPDATE SET preset_id=excluded.preset_id,preset_name=excluded.preset_name,preset_source=excluded.preset_source,tray_info_idx=excluded.tray_info_idx,updated_at=CURRENT_TIMESTAMP", j["printer_id"], j["ams_id"], j["tray_id"], opts.mapping, opts.name, opts.source, opts.preset)
		if e != nil {
			return false
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE bb_slot_config SET phase='sent', due=?,retries=retries+1,expected_type=?,expected_color=?,expected_preset=? WHERE assignment_id=? AND token=?", time.Now().Unix()+30, opts.typ, opts.color, opts.preset, id, token); e != nil {
		return false
	}
	if tx.Commit() != nil {
		return false
	}
	// An explicit assignment replaces the previous roll's manual K selection.
	key := trayKey{num(j["ams_id"]), num(j["tray_id"])}
	delete(d.drift.watches, key)
	delete(d.drift.manualDefault, key)
	// Pushall is a readback nudge. Command delivery succeeded independently.
	_ = h.Publish(ctx, packet("pushing", "pushall", nil))
	return true
}
