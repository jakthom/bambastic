package management

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

type trayObservation struct {
	signature, prompted string
	checked, emptySince time.Time
	heldAssignment      int64
}

type inventoryMode struct {
	base                     string
	remote, autoAdd, enabled bool
}

func currentInventoryMode(ctx context.Context, db access.Query) (inventoryMode, error) {
	m := inventoryMode{}
	var remote, auto, mode, disconnected string
	e := db.QueryRowContext(ctx, `SELECT
		coalesce((SELECT value FROM settings WHERE key='spoolman_enabled'),'false'),
		coalesce((SELECT value FROM settings WHERE key='auto_add_unknown_rfid'),'true'),
		coalesce((SELECT value FROM settings WHERE key='spoolman_sync_mode'),'auto'),
		coalesce((SELECT value FROM settings WHERE key='spoolman_url'),''),
		coalesce((SELECT value FROM settings WHERE key='bambastic_spoolman_disconnected'),'false')`).Scan(&remote, &auto, &mode, &m.base, &disconnected)
	m.base = strings.TrimRight(m.base, "/")
	m.remote = strings.EqualFold(remote, "true")
	m.autoAdd = strings.EqualFold(auto, "true")
	m.enabled = !m.remote || (strings.EqualFold(mode, "auto") && !strings.EqualFold(disconnected, "true") && m.base != "")
	return m, e
}

func materialFromTray(tx *sql.Tx, tray row) (row, error) {
	material, variant := strings.TrimSpace(str(tray["tray_type"])), strings.TrimSpace(str(tray["tray_sub_brands"]))
	subtype := ""
	if variant != "" && !strings.EqualFold(variant, material) {
		first, other, has := strings.Cut(variant, " ")
		if has && strings.EqualFold(first, material) {
			subtype = strings.TrimSpace(other)
		} else {
			material = variant
		}
	}
	name := str(tray["tray_id_name"])
	family, code, coded := strings.Cut(name, "-")
	if strings.HasPrefix(code, "M") {
		subtype = "Gradient"
		if family == "A05" {
			subtype = "Dual Color"
		}
	} else if strings.HasPrefix(code, "T") {
		subtype = "Tri Color"
	}
	label := num(tray["tray_weight"])
	if label <= 0 {
		label = 1000
	}
	used := 0.
	if remain, e := strconv.ParseFloat(str(tray["remain"]), 64); e == nil && remain >= 0 && remain <= 100 {
		used = math.Round(label*(100-remain)/10) / 10
	}
	d := row{"material": material, "label_weight": label, "weight_used": used, "weight_locked": false, "core_weight": 250., "brand": "Bambu Lab", "data_origin": "rfid_auto", "tag_type": "bambulab"}
	for k, x := range map[string]any{"subtype": subtype, "rgba": tray["tray_color"], "slicer_filament": tray["tray_info_idx"], "slicer_filament_name": variant, "tray_uuid": cleanTag(tray["tray_uuid"], 32), "tag_uid": cleanTag(tray["tag_uid"], 16)} {
		if str(x) != "" {
			d[k] = x
		}
	}
	for _, k := range []string{"nozzle_temp_min", "nozzle_temp_max"} {
		if x := num(tray[k]); x > 0 {
			d[k] = x
		}
	}
	if !coded && name != "" {
		d["color_name"] = name
	}
	if tx == nil {
		return d, nil
	}
	core, e := one(tx, "SELECT id,weight FROM spool_catalog WHERE lower(name)='bambu lab - plastic low temp' ORDER BY id LIMIT 1")
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if core != nil {
		d["core_weight"], d["core_weight_catalog_id"] = num(core["weight"]), num(core["id"])
	}
	if c := strings.TrimPrefix(str(d["rgba"]), "#"); len(c) >= 6 {
		ref, e := one(tx, `SELECT color_name,extra_colors,effect_type FROM color_catalog
			WHERE lower(manufacturer) LIKE '%bambu%' AND upper(substr(ltrim(hex_color,'#'),1,6))=?
			ORDER BY CASE WHEN upper(material)=upper(?) THEN 0 ELSE 1 END,id LIMIT 1`, strings.ToUpper(c[:6]), variant)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		for k, x := range ref {
			if x != nil {
				d[k] = x
			}
		}
	}
	return d, nil
}

func similarTrayColor(a, b string) bool {
	a, b = strings.TrimPrefix(strings.ToUpper(a), "#"), strings.TrimPrefix(strings.ToUpper(b), "#")
	if a == b {
		return true
	}
	if len(a) < 6 || len(b) < 6 {
		return false
	}
	x, e := hex.DecodeString(a[:6])
	y, f := hex.DecodeString(b[:6])
	if e != nil || f != nil {
		return false
	}
	d := 0
	for i := range x {
		v := int(x[i]) - int(y[i])
		d += v * v
	}
	return d <= 2500
}

func matchingTag(sp row, uuid, uid string) bool {
	if uuid != "" && equalText(sp["tray_uuid"], uuid) {
		return true
	}
	return uid != "" && nativeTag(uid) != "" && nativeTag(str(sp["tag_uid"])) == nativeTag(uid)
}

func slotPreset(r request, k slotKey, sp, tray row) error {
	reported := str(tray["tray_info_idx"])
	id, source := reported, "cloud"
	sf, _, _ := strings.Cut(str(sp["slicer_filament"]), "_")
	if _, e := strconv.ParseUint(sf, 10, 64); e == nil {
		id, source = "local_"+sf, "local"
	} else if strings.HasPrefix(id, "GFSNL") && len(id) > 5 && id[5] >= '0' && id[5] <= '9' {
		id = "GFSNLS" + id[5:]
	} else if strings.HasPrefix(id, "GF") && len(id) >= 4 && !(id[2] == 'S' && id[3] >= 'A' && id[3] <= 'Z') {
		id = "GFS" + id[2:]
	}
	if id == "" {
		return nil
	}
	name := str(sp["slicer_filament_name"])
	if name == "" {
		name = str(tray["tray_sub_brands"])
	}
	if name == "" {
		name = str(tray["tray_type"])
	}
	_, e := r.tx.Exec(`INSERT INTO slot_preset_mappings(printer_id,ams_id,tray_id,preset_id,preset_name,preset_source,tray_info_idx)
		VALUES(?,?,?,?,?,?,?) ON CONFLICT(printer_id,ams_id,tray_id) DO UPDATE SET preset_id=excluded.preset_id,preset_name=excluded.preset_name,preset_source=excluded.preset_source,tray_info_idx=excluded.tray_info_idx,updated_at=CURRENT_TIMESTAMP`, k.printer, k.ams, k.tray, id, name, source, reported)
	return e
}

func localRFID(r request, d row, auto bool) (row, error) {
	v, e := byTag(r, str(d["tray_uuid"]), str(d["tag_uid"]), true)
	if e == nil {
		return v, nil
	}
	var p *problem
	if !errors.As(e, &p) || p.code != 404 {
		return nil, e
	}
	// Bambu includes a model preset or a nonzero UUID. User-defined tags still resolve a known spool.
	if d["tray_uuid"] == nil && d["slicer_filament"] == nil {
		return nil, nil
	}
	v, e = one(r.tx, `SELECT s.* FROM spool s WHERE archived_at IS NULL AND tag_uid IS NULL AND tray_uuid IS NULL
		AND upper(material)=upper(?) AND upper(rgba)=upper(?) AND (coalesce(brand,'')='' OR lower(brand) LIKE '%bambu%')
		AND (coalesce(subtype,'')='' OR upper(subtype)=upper(?)) AND NOT EXISTS (SELECT 1 FROM spool_assignment a WHERE a.spool_id=s.id)
		ORDER BY CASE WHEN coalesce(subtype,'')='' THEN 1 ELSE 0 END,created_at,id LIMIT 1`, d["material"], d["rgba"], d["subtype"])
	if e == nil {
		_, e = r.tx.Exec("UPDATE spool SET tray_uuid=?,tag_uid=?,tag_type='bambulab',updated_at=CURRENT_TIMESTAMP WHERE id=?", d["tray_uuid"], d["tag_uid"], v["id"])
		return v, e
	}
	if !errors.Is(e, sql.ErrNoRows) {
		return nil, e
	}
	if auto {
		return saveSpool(r, 0, d)
	}
	return nil, nil
}

type remoteInventory struct {
	catalog []row
	loaded  bool
	err     error
}

func (x *remoteInventory) resolve(ctx context.Context, mode inventoryMode, tray row) (row, error) {
	if !x.loaded {
		var all []row
		x.err = remoteCall(ctx, mode.base, "GET", "spool?allow_archived=true", nil, &all)
		for _, v := range all {
			x.catalog = append(x.catalog, remoteSpool(v))
		}
		x.loaded = true
	}
	if x.err != nil {
		return nil, x.err
	}
	uid, uuid := str(cleanTag(tray["tag_uid"], 16)), str(cleanTag(tray["tray_uuid"], 32))
	var archived row
	for _, v := range x.catalog {
		if matchingTag(v, uuid, uid) {
			if v["archived_at"] == nil {
				return v, nil
			}
			archived = v
		}
	}
	if archived != nil || !mode.autoAdd || uuid == "" && str(tray["tray_info_idx"]) == "" {
		return archived, nil
	}
	d, e := materialFromTray(nil, tray)
	if e != nil {
		return nil, e
	}
	events := []row{}
	r := request{Request: (&http.Request{}).WithContext(ctx), events: &events, remoteEndpoint: mode.base}
	v, e := writeRemoteSpool(r, 0, d, true)
	if e == nil {
		x.catalog = append(x.catalog, v)
	}
	return v, e
}

func (s *Service) liveConflict(r request, table, col string, id int64, k slotKey, uuid, uid string) (bool, error) {
	others, e := list(r.tx, "SELECT printer_id,ams_id,tray_id FROM "+table+" WHERE "+col+"=? AND NOT(printer_id=? AND ams_id=? AND tray_id=?)", id, k.printer, k.ams, k.tray)
	if e != nil {
		return false, e
	}
	for _, v := range others {
		prev := slotKey{intID(v["printer_id"]), int64(num(v["ams_id"])), int64(num(v["tray_id"]))}
		slots, busy, online := s.slots(prev.printer)
		if online && (busy || matchingTag(slots[prev], uuid, uid)) {
			return true, nil
		}
		n, e := count(r.tx, "SELECT count(*) FROM print_log_entries WHERE printer_id=? AND status IN ('printing','running','paused')", prev.printer)
		if e != nil || n > 0 {
			return true, e
		}
	}
	return false, nil
}

func (s *Service) observeTray(ctx context.Context, k slotKey, tray row, mode inventoryMode, remotes *remoteInventory, prior trayObservation, at time.Time, trayCount int) (trayObservation, error) {
	uuid, uid := str(cleanTag(tray["tray_uuid"], 32)), str(cleanTag(tray["tag_uid"], 16))
	hasTag := uuid != "" || uid != ""
	blank := str(tray["tray_type"]) == "" && (str(tray["tray_color"]) == "" || str(tray["tray_color"]) == "00000000")
	if exists, ok := tray["exists"].(bool); ok {
		blank = !exists
	}
	var sp row
	var e error
	if mode.remote && hasTag && !blank {
		if sp, e = remotes.resolve(ctx, mode, tray); e != nil {
			return prior, e
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return prior, e
	}
	defer tx.Rollback()
	events := []row{}
	r := request{Request: (&http.Request{}).WithContext(ctx), tx: tx, events: &events}
	checked, e := currentInventoryMode(ctx, tx)
	if e != nil || checked != mode {
		return prior, e
	}
	n, e := count(tx, "SELECT count(*) FROM print_log_entries WHERE printer_id=? AND status IN ('printing','running','paused')", k.printer)
	if e != nil || n != 0 {
		return prior, e
	}
	next := prior
	next.emptySince = time.Time{}
	next.heldAssignment = 0
	next.prompted = ""
	table, col := "spool_assignment", "spool_id"
	if mode.remote {
		table, col = "spoolman_slot_assignments", "spoolman_spool_id"
	}
	as, e := one(tx, "SELECT * FROM "+table+" WHERE printer_id=? AND ams_id=? AND tray_id=?", k.printer, k.ams, k.tray)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return prior, e
	}
	assigned := intID(as[col])
	var current row
	configPending := false
	if !mode.remote && assigned > 0 {
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM bb_slot_config WHERE assignment_id=? AND phase<>'confirmed')", as["id"]).Scan(&configPending); err != nil {
			return prior, err
		}
	}
	if !mode.remote && assigned > 0 {
		current, e = get(tx, "spool", assigned)
		if e != nil {
			return prior, e
		}
	}
	unlink := false
	switch {
	case blank && !configPending && assigned > 0 && (mode.remote || str(as["fingerprint_type"]) != "" || current["tag_uid"] != nil || current["tray_uuid"] != nil):
		next.heldAssignment = intID(as["id"])
		next.emptySince = prior.emptySince
		if next.emptySince.IsZero() || prior.heldAssignment != next.heldAssignment {
			next.emptySince = at
		}
		unlink = at.Sub(next.emptySince) >= 2*time.Minute
	case !blank && hasTag:
		if !mode.remote {
			if matchingTag(current, uuid, uid) {
				sp = current
			} else {
				d, e := materialFromTray(tx, tray)
				if e != nil {
					return prior, e
				}
				sp, e = localRFID(r, d, mode.autoAdd)
				if e != nil {
					return prior, e
				}
			}
		}
		unlink = assigned > 0 && (sp == nil || sp["archived_at"] != nil || intID(sp["id"]) != assigned)
		if sp == nil {
			next.prompted = uuid + "/" + uid
			if prior.prompted != next.prompted {
				r.event(row{"type": "unknown_tag", "printer_id": k.printer, "ams_id": k.ams, "tray_id": k.tray, "tag_uid": uid, "tray_uuid": uuid, "tray_type": tray["tray_type"], "tray_color": tray["tray_color"], "tray_sub_brands": tray["tray_sub_brands"], "tray_count": trayCount})
			}
		}
	case !blank && !mode.remote && !configPending && assigned > 0 && str(as["fingerprint_type"]) != "" && str(tray["tray_type"]) != "":
		unlink = !equalText(as["fingerprint_type"], tray["tray_type"]) || !similarTrayColor(str(as["fingerprint_color"]), str(tray["tray_color"]))
	}
	if unlink {
		if _, e = tx.Exec("DELETE FROM "+table+" WHERE id=?", as["id"]); e != nil {
			return prior, e
		}
		if _, e = tx.Exec("DELETE FROM slot_preset_mappings WHERE printer_id=? AND ams_id=? AND tray_id=?", k.printer, k.ams, k.tray); e != nil {
			return prior, e
		}
		r.event(row{"type": "inventory_changed", "printer_id": k.printer})
		assigned = 0
		next.emptySince = time.Time{}
	}
	if !blank && sp != nil && sp["archived_at"] == nil && intID(sp["id"]) != assigned {
		id := intID(sp["id"])
		conflict, e := s.liveConflict(r, table, col, id, k, uuid, uid)
		if e != nil {
			return prior, e
		}
		if !conflict && id > 0 {
			if e = r.bind(mode.remote, id, k); e != nil {
				return prior, e
			}
			if e = slotPreset(r, k, sp, tray); e != nil {
				return prior, e
			}
			assigned = id
			as = nil
		}
	}
	if !blank && assigned > 0 && !mode.remote && !configPending && str(tray["tray_type"]) != "" {
		if !equalText(as["fingerprint_type"], tray["tray_type"]) || !equalText(as["fingerprint_color"], tray["tray_color"]) {
			if _, e = tx.Exec("UPDATE spool_assignment SET fingerprint_color=?,fingerprint_type=? WHERE printer_id=? AND ams_id=? AND tray_id=?", tray["tray_color"], tray["tray_type"], k.printer, k.ams, k.tray); e != nil {
				return prior, e
			}
		}
		if rem := num(tray["remain"]); rem >= 1 && rem <= 100 && rem == math.Trunc(rem) {
			out, e := tx.Exec(`UPDATE spool SET weight_used=label_weight*(100-?)/100.0,updated_at=CURRENT_TIMESTAMP
				WHERE id=? AND NOT weight_locked AND label_weight*(100-?)/100.0>weight_used+1`, rem, assigned, rem)
			if e != nil {
				return prior, e
			}
			if updated, e := out.RowsAffected(); e != nil {
				return prior, e
			} else if updated > 0 {
				r.event(row{"type": "inventory_changed", "printer_id": k.printer})
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return prior, e
	}
	if s.emit != nil {
		for _, v := range events {
			s.emit(v)
		}
	}
	return next, nil
}

func (s *Service) observeAMS(ctx context.Context, at time.Time, seen map[slotKey]trayObservation) error {
	if s.MaterialSlots == nil {
		return nil
	}
	mode, e := currentInventoryMode(ctx, s.db)
	if e != nil {
		return e
	}
	if !mode.enabled {
		clear(seen)
		return nil
	}
	ctx = access.With(ctx, access.Principal{Admin: true})
	// Exclude open runs even if the live printer just reported idle; settlement may still be charging usage.
	rows, e := s.db.QueryContext(ctx, `SELECT id FROM printers WHERE is_active AND NOT EXISTS(SELECT 1 FROM print_log_entries l WHERE l.printer_id=printers.id AND l.status IN ('printing','running','paused')) ORDER BY id`)
	if e != nil {
		return e
	}
	var printers []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		printers = append(printers, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	observed := map[slotKey]bool{}
	remote := remoteInventory{}
	var failures []error
	for _, id := range printers {
		slots, busy, online := s.slots(id)
		if !online || busy {
			continue
		}
		keys := make([]slotKey, 0, len(slots))
		counts := map[int64]int{}
		for k := range slots {
			if validSlot(row{"printer_id": k.printer, "ams_id": k.ams, "tray_id": k.tray}) {
				keys = append(keys, k)
				counts[k.ams]++
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].ams == keys[j].ams {
				return keys[i].tray < keys[j].tray
			}
			return keys[i].ams < keys[j].ams
		})
		for _, k := range keys {
			tray := slots[k]
			if tray["exists"] == nil && tray["tray_type"] == nil && tray["tray_color"] == nil {
				continue
			} // Partial report has no presence evidence.
			observed[k] = true
			prior := seen[k]
			sig := fmt.Sprintf("%v/%v/%v/%s/%s/%s/%s/%s/%s/%s/%s", mode.remote, mode.autoAdd, mode.base, str(tray["exists"]), str(tray["tray_type"]), str(tray["tray_color"]), str(tray["tray_uuid"]), str(tray["tag_uid"]), str(tray["tray_sub_brands"]), str(tray["tray_info_idx"]), str(tray["remain"]))
			if sig == prior.signature && at.Sub(prior.checked) < time.Minute && (prior.emptySince.IsZero() || at.Sub(prior.emptySince) < 2*time.Minute) {
				continue
			}
			next, e := s.observeTray(ctx, k, tray, mode, &remote, prior, at, counts[k.ams])
			if e != nil {
				failures = append(failures, fmt.Errorf("printer %d AMS %d tray %d: %w", k.printer, k.ams, k.tray, e))
				continue
			}
			next.signature, next.checked = sig, at
			seen[k] = next
		}
	}
	for k := range seen {
		if !observed[k] {
			delete(seen, k)
		}
	}
	return errors.Join(failures...)
}
