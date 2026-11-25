package production

import (
	"context"
	"database/sql"
	"errors"
	"maps"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/management"
)

type trayKey = [2]int64
type kWatch struct {
	signature      string
	next, lastSend time.Time
	attempts       int
	lastJob        int64
}
type kDrift struct {
	// Serialize firmware selection with explicit Configure Slot writes.
	mu            sync.Mutex
	watches       map[trayKey]kWatch
	manualDefault map[trayKey]bool
}

func (k *kDrift) chosen(key trayKey, idx int64) {
	if k.manualDefault == nil {
		k.manualDefault = map[trayKey]bool{}
	}
	if idx < 0 {
		k.manualDefault[key] = true
	} else {
		delete(k.manualDefault, key)
	}
	delete(k.watches, key)
}
func selectedIndexLost(v any) bool {
	if v == nil {
		return true
	}
	n, e := strconv.ParseInt(val(v), 10, 32)
	return e == nil && n < 0
}
func usedKTrays(mapping any) map[int64]bool {
	trays := map[int64]bool{}
	if m, ok := unpack(mapping).([]any); ok {
		for _, v := range m {
			n, e := strconv.ParseInt(val(v), 10, 32)
			if e == nil && n >= 0 {
				trays[n] = true
			}
		}
	}
	return trays
}
func spoolTag(v any) string {
	s := strings.ToUpper(strings.TrimSpace(val(v)))
	s = strings.NewReplacer("-", "", ":", "").Replace(s)
	if strings.Trim(s, "0") == "" {
		return ""
	}
	return s
}
func samePhysicalSpool(sp, tray row) bool {
	oldTag, oldUUID := spoolTag(sp["tag_uid"]), spoolTag(sp["tray_uuid"])
	liveTag, liveUUID := spoolTag(tray["tag_uid"]), spoolTag(tray["tray_uuid"])
	if len(liveTag) == 16 && strings.HasSuffix(liveTag, "00") {
		liveTag = liveTag[:14]
	}
	if len(oldTag) == 16 && strings.HasSuffix(oldTag, "00") {
		oldTag = oldTag[:14]
	}
	if oldUUID != "" && liveUUID != "" && oldUUID != liveUUID || oldTag != "" && liveTag != "" && oldTag != liveTag {
		return false
	}
	if liveTag != "" || liveUUID != "" || oldTag != "" || oldUUID != "" {
		return oldUUID != "" && oldUUID == liveUUID || oldTag != "" && oldTag == liveTag
	}
	for _, key := range []string{"fingerprint_type", "material"} {
		if x := val(sp[key]); x != "" && !strings.EqualFold(x, val(tray["tray_type"])) {
			return false
		}
	}
	for _, key := range []string{"fingerprint_color", "rgba"} {
		if x := rgb(sp[key]); x != "" && rgb(tray["tray_color"]) != "" && x != rgb(tray["tray_color"]) {
			return false
		}
	}
	return true
}
func nozzleFlow(v any) string {
	s := strings.ToUpper(val(v))
	if strings.HasPrefix(s, "HH") || strings.HasPrefix(s, "HS") {
		return s[:2]
	}
	return ""
}

type kCandidate struct {
	slot            amsSlot
	report          row
	signature, flow string
	dual            bool
	address         int64
}

func kCandidates(pr, r row) map[trayKey]kCandidate {
	// Dual-holder firmware uses vir_slot. Normalize it for inventory's global
	// addressing and presence-mask handling.
	if vs, ok := r["vir_slot"].([]any); ok {
		r = maps.Clone(r)
		r["vt_tray"] = vs
	}
	slots := management.MaterialSlotReports(num(pr["id"]), r)
	out := make(map[trayKey]kCandidate, len(slots))
	device, _ := r["device"].(map[string]any)
	nozzle, _ := device["nozzle"].(map[string]any)
	nozzles, _ := nozzle["info"].([]any)
	twoNozzles := dual(pr) || len(holders(r)) > 1 || nozzleOK(val(r["nozzle_diameter_2"])) || nozzleOK(val(r["right_nozzle_diameter"]))
	for _, raw := range nozzles {
		n, _ := raw.(map[string]any)
		if num(n["id"]) == 1 && nozzleOK(val(n["diameter"])) {
			twoNozzles = true
		}
	}
	if twoNozzles {
		pr = maps.Clone(pr)
		pr["nozzle_count"] = 2
	}
	for key, tray := range slots {
		unit, local := key[0], key[1]
		a, e := resolveSlot(pr, r, unit, local)
		if e != nil {
			continue
		}
		address := a.global
		switch {
		case unit == 6:
			address = 24 + local
		case unit == 32:
			address = 28 + local
		case unit >= 128 && unit < 136:
			address = unit
		}
		flow := nozzleFlow(r["nozzle_type"])
		keys := []string{"left_nozzle_type"}
		if a.extruder == 1 {
			keys = []string{"right_nozzle_type", "nozzle_type_2"}
		}
		for _, key := range keys {
			if f := nozzleFlow(r[key]); f != "" {
				flow = f
			}
		}
		for _, raw := range nozzles {
			n, _ := raw.(map[string]any)
			if n["id"] != nil && num(n["id"]) == a.extruder {
				for _, key := range []string{"type", "nozzle_type", "nozzle_id"} {
					if f := nozzleFlow(n[key]); f != "" {
						flow = f
						break
					}
				}
			}
		}
		signature := jstr([]any{spoolTag(tray["tag_uid"]), spoolTag(tray["tray_uuid"]), val(tray["tray_type"]), rgb(tray["tray_color"]), val(tray["tray_info_idx"]), a.diameter, a.extruder, flow})
		out[key] = kCandidate{slot: a, report: tray, signature: signature, flow: flow, address: address, dual: twoNozzles}
	}
	return out
}

type kBinding struct {
	assignment, spool, profile, idx int64
	filament                        string
	remote                          bool
}

func (s *Service) driftBinding(ctx context.Context, pr row, key trayKey, c kCandidate) (kBinding, error) {
	mode, e := access.Setting(ctx, s.db, "spoolman_enabled", "false")
	if e != nil {
		return kBinding{}, e
	}
	remote := flag(mode)
	assign, spoolCol, prefix := "spool_assignment", "spool_id", "spool"
	fields := "sp.tag_uid,sp.tray_uuid,sp.material,sp.rgba,a.fingerprint_type,a.fingerprint_color,sp.slicer_filament spool_filament"
	join := "JOIN spool sp ON sp.id=a.spool_id AND sp.archived_at IS NULL"
	if remote {
		assign, spoolCol, prefix = "spoolman_slot_assignments", "spoolman_spool_id", "spoolman"
		join = ""
		fields = "NULL spool_filament"
	}
	// A single nozzle permits the alternate index under which Configure Slot
	// saved an external holder. Two independent hotends never share a K value.
	alternate := c.slot.extruder
	if !c.dual && key[0] == 255 {
		alternate = 1
	}
	rs, e := records(ctx, s.db, "SELECT a.id AS assignment_id,a."+spoolCol+" AS spool_id,k.id AS profile_id,k.extruder,k.cali_idx,k.nozzle_type,"+fields+",(SELECT preset.slicer_filament FROM "+prefix+"_filament_preset preset WHERE preset."+spoolCol+"=a."+spoolCol+" AND lower(preset.printer_model)=lower(?) AND preset.nozzle_diameter IN (?,'') ORDER BY preset.nozzle_diameter DESC LIMIT 1) AS model_filament FROM "+assign+" a "+join+" JOIN printers p ON p.id=a.printer_id AND p.is_active JOIN "+prefix+"_k_profile k ON k."+spoolCol+"=a."+spoolCol+" AND k.printer_id=a.printer_id WHERE a.printer_id=? AND p.serial_number=? AND a.ams_id=? AND a.tray_id=? AND k.nozzle_diameter=? AND k.extruder IN (?,?) AND k.cali_idx BETWEEN 0 AND 65535 ORDER BY CASE WHEN k.extruder=? THEN 0 ELSE 1 END,k.id DESC",
		pr["model"], c.slot.diameter, pr["id"], pr["serial_number"], key[0], key[1], c.slot.diameter, c.slot.extruder, alternate, c.slot.extruder)
	if e != nil {
		return kBinding{}, e
	}
	for _, v := range rs {
		if flow := nozzleFlow(v["nozzle_type"]); flow != "" && c.flow != "" && c.flow != flow {
			continue
		}
		identity := v
		if remote {
			if s.SpoolmanMaterialIdentity == nil {
				return kBinding{}, sql.ErrNoRows
			}
			identity, e = s.SpoolmanMaterialIdentity(ctx, num(v["spool_id"]))
			if e != nil {
				return kBinding{}, e
			}
		}
		if !samePhysicalSpool(identity, c.report) {
			return kBinding{}, sql.ErrNoRows
		}
		filament := ""
		for _, v := range []any{v["model_filament"], v["spool_filament"], c.report["tray_info_idx"]} {
			filament, _ = filamentID(val(v))
			if filament != "" {
				break
			}
		}
		return kBinding{num(v["assignment_id"]), num(v["spool_id"]), num(v["profile_id"]), num(v["cali_idx"]), filament, remote}, nil
	}
	return kBinding{}, sql.ErrNoRows
}
func (s *Service) sameKBinding(ctx context.Context, id int64, key trayKey, b kBinding) bool {
	table, sp, prefix := "spool_assignment", "spool_id", "spool"
	if b.remote {
		table, sp, prefix = "spoolman_slot_assignments", "spoolman_spool_id", "spoolman"
	}
	mode, e := access.Setting(ctx, s.db, "spoolman_enabled", "false")
	if e != nil || flag(mode) != b.remote {
		return false
	}
	var ok bool
	e = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" a JOIN "+prefix+"_k_profile k ON k."+sp+"=a."+sp+" AND k.printer_id=a.printer_id WHERE a.id=? AND a.printer_id=? AND a.ams_id=? AND a.tray_id=? AND a."+sp+"=? AND k.id=? AND k.cali_idx=?)", b.assignment, id, key[0], key[1], b.spool, b.profile, b.idx).Scan(&ok)
	return e == nil && ok
}

// driftSnapshot returns only telemetry received since the current connection
// opened. A printer's previous AMS insertion cannot authorize a new command.
func (s *Service) driftSnapshot(id int64, d *liveDevice) (row, hardware) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.devices[id] != d || d.h == nil || !d.h.Online() || !idle(val(d.kReport["gcode_state"])) {
		return nil, nil
	}
	return maps.Clone(d.kReport), d.h
}
func (s *Service) reapplyK(ctx context.Context, id int64, d *liveDevice, used map[int64]bool, job int64, now time.Time) int {
	pass, stop := context.WithTimeout(ctx, 4*time.Second)
	defer stop()
	ctx = pass
	k := &d.drift
	k.mu.Lock()
	defer k.mu.Unlock()
	r, h := s.driftSnapshot(id, d)
	if h == nil {
		return 0
	}
	pr := d.printer
	if k.watches == nil {
		k.watches = map[trayKey]kWatch{}
	}
	applied := 0
	for key, c := range kCandidates(pr, r) {
		if ctx.Err() != nil {
			break
		}
		if val(c.report["tray_type"]) == "" {
			if c.report["tray_type"] != nil || c.report["exists"] == false {
				delete(k.watches, key)
				delete(k.manualDefault, key)
			}
			continue
		}
		if !selectedIndexLost(c.report["cali_idx"]) {
			delete(k.watches, key)
			continue
		}
		if k.manualDefault[key] || c.slot.extruder < 0 || len(used) > 0 && !used[c.address] {
			continue
		}
		w := k.watches[key]
		if w.signature != c.signature {
			w = kWatch{signature: c.signature}
		}
		if job == 0 && (now.Before(w.next) || w.attempts >= 3) || job != 0 && (w.lastJob == job || now.Sub(w.lastSend) < 2*time.Second) {
			continue
		}
		w.next = now.Add(30 * time.Second)
		if job != 0 {
			w.lastJob = job
		}
		scoped, cancel := context.WithTimeout(ctx, 3*time.Second)
		b, e := s.driftBinding(scoped, pr, key, c)
		if e != nil {
			cancel()
			if errors.Is(e, sql.ErrNoRows) {
				w.next = now.Add(10 * time.Minute)
			}
			k.watches[key] = w
			continue
		}
		// Resolve the same live slot again after a DB or Spoolman read. A slot
		// can be emptied or manually selected while that read is in flight.
		if !s.sameKBinding(scoped, id, key, b) {
			cancel()
			continue
		}
		live, conn := s.driftSnapshot(id, d)
		current := kCandidates(pr, live)[key]
		if conn == nil || current.signature != c.signature || !selectedIndexLost(current.report["cali_idx"]) {
			cancel()
			continue
		}
		a := c.slot
		wire := a.wire
		if key[0] == 255 {
			wire = a.global
		}
		e = conn.Publish(scoped, packet("print", "extrusion_cali_sel", row{"ams_id": wire, "tray_id": a.global, "slot_id": a.local, "cali_idx": b.idx, "filament_id": b.filament, "nozzle_diameter": a.diameter}))
		cancel()
		w.attempts++
		w.lastSend = now
		k.watches[key] = w
		if e == nil {
			applied++
		}
	}
	return applied
}
