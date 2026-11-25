package management

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"bambastic/internal/access"
)

func remoteHolders(r request, id int64) error {
	rs, e := list(r.tx, "SELECT printer_id FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", id)
	if e != nil {
		return e
	}
	for _, v := range rs {
		if !access.From(r.Context()).AllowsPrinter(intID(v["printer_id"])) {
			return notfound()
		}
	}
	return nil
}
func remoteAction(r request, id int64, op string) (row, error) {
	if id <= 0 {
		return nil, invalid("Invalid spool ID")
	}
	if e := remoteHolders(r, id); e != nil {
		return nil, e
	}
	path := fmt.Sprintf("spool/%d", id)
	if op == "delete" {
		if e := remote(r, "DELETE", path, nil, nil); e != nil {
			return nil, e
		}
		for _, t := range []string{"spoolman_slot_assignments", "spoolman_k_profile", "spoolman_filament_preset", "spoolman_spool_suppliers"} {
			if _, e := r.tx.Exec("DELETE FROM "+t+" WHERE spoolman_spool_id=?", id); e != nil {
				return nil, e
			}
		}
		_, e := r.tx.Exec("UPDATE spool_usage_history SET spoolman_spool_id=NULL WHERE spoolman_spool_id=?", id)
		return row{"status": "deleted"}, e
	}
	body := row{"archived": op == "archive"}
	if op == "reset-consumed-counter" {
		v, e := rawSpool(r, id)
		if e != nil {
			return nil, e
		}
		if e = ensureExtras(r, []string{"bambu_weight_used_baseline"}); e != nil {
			return nil, e
		}
		extra := sub(v["extra"])
		b, _ := json.Marshal(str(num(v["used_weight"])))
		extra["bambu_weight_used_baseline"] = string(b)
		body = row{"extra": extra}
	}
	var updated row
	if e := remote(r, "PATCH", path, body, &updated); e != nil {
		return nil, e
	}
	if op == "archive" {
		if _, e := r.tx.Exec("DELETE FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", id); e != nil {
			return nil, e
		}
	}
	return remoteSpool(updated), nil
}
func linkedSpools(r request, linked bool) (any, error) {
	var spools []row
	if e := remote(r, "GET", "spool", nil, &spools); e != nil {
		return nil, e
	}
	ids := map[int64]bool{}
	invisible := map[int64]bool{}
	assigned, e := list(r.tx, "SELECT printer_id,spoolman_spool_id FROM spoolman_slot_assignments")
	if e != nil {
		return nil, e
	}
	for _, v := range assigned {
		id := intID(v["spoolman_spool_id"])
		ids[id] = true
		if !access.From(r.Context()).AllowsPrinter(intID(v["printer_id"])) {
			invisible[id] = true
		}
	}
	tags := row{}
	free := []row{}
	for _, v := range spools {
		id := intID(v["id"])
		if invisible[id] || truth(v["archived"]) {
			continue
		}
		f := sub(v["filament"])
		if linked {
			tag := strings.ToUpper(extraText(sub(v["extra"]), "tag"))
			if tag != "" {
				net, _, _ := remoteWeights(v)
				tags[tag] = row{"id": id, "filament_weight": net, "remaining_weight": v["remaining_weight"]}
			}
		} else if !ids[id] {
			free = append(free, row{"id": id, "filament_name": f["name"], "filament_material": f["material"], "filament_color_hex": f["color_hex"], "filament_vendor": sub(f["vendor"])["name"], "remaining_weight": v["remaining_weight"], "location": v["location"]})
		}
	}
	if linked {
		return row{"linked": tags}, nil
	}
	return free, nil
}
func normalizedFilament(v row) row {
	out := obj(v, "id name material color_hex weight spool_weight")
	out["vendor_id"] = sub(v["vendor"])["id"]
	out["vendor_name"] = sub(v["vendor"])["name"]
	out["vendor_empty_spool_weight"] = sub(v["vendor"])["empty_spool_weight"]
	return out
}
func patchRemoteFilament(r request) (any, error) {
	d, e := r.body()
	if e != nil {
		return nil, e
	}
	id := r.id()
	var old row
	if e = remote(r, "GET", fmt.Sprintf("filament/%d", id), nil, &old); e != nil {
		return nil, e
	}
	p := obj(d, "name spool_weight")
	if x, ok := d["name"]; ok && (str(x) == "" || len(str(x)) > 255) {
		return nil, invalid("Invalid filament name")
	}
	if v, ok := d["spool_weight"]; ok && v != nil {
		if n, ok := v.(float64); !ok || n < 0 {
			return nil, invalid("Invalid tare weight")
		}
	}
	if len(p) == 0 {
		return normalizedFilament(old), nil
	}
	// Stamp existing overrides before the shared default changes. Reissuing an
	// interrupted operation never loses the old, inherited tare.
	if weight, ok := p["spool_weight"]; ok {
		var spools []row
		if e = remote(r, "GET", "spool", nil, &spools); e != nil {
			return nil, e
		}
		keep := truth(d["keep_existing_spools"])
		for _, v := range spools {
			if intID(sub(v["filament"])["id"]) != id {
				continue
			}
			target := weight
			if keep {
				if v["spool_weight"] != nil {
					continue
				}
				_, targetWeight, _ := remoteWeights(v)
				target = targetWeight
			}
			if v["spool_weight"] == target {
				continue
			}
			if e = remote(r, "PATCH", fmt.Sprintf("spool/%d", intID(v["id"])), row{"spool_weight": target}, nil); e != nil {
				return nil, &problem{502, "Spoolman failed partway through updating spool tare overrides"}
			}
		}
	}
	var changed row
	if e = remote(r, "PATCH", fmt.Sprintf("filament/%d", id), p, &changed); e != nil {
		return nil, e
	}
	return normalizedFilament(changed), nil
}
func applyRemoteDefault(r request) (any, error) {
	d, e := get(r.tx, "filament_profile_default", r.id())
	if e != nil {
		return nil, e
	}
	var spools []row
	if e = remote(r, "GET", "spool", nil, &spools); e != nil {
		return nil, e
	}
	n := 0
	for _, raw := range spools {
		if truth(raw["archived"]) {
			continue
		}
		sp := remoteSpool(raw)
		if productKey(sp, str(d["color_name"]) == "") != str(d["match_key"]) {
			continue
		}
		detail, e := resolveDefault(r, sp)
		if e != nil {
			return nil, e
		}
		if detail == nil {
			continue
		}
		changed := false
		if str(sp["slicer_filament"]) == "" && str(detail["slicer_filament"]) != "" {
			if _, e = saveRemoteSpool(r, intID(sp["id"]), obj(detail, "slicer_filament slicer_filament_name")); e != nil {
				return nil, e
			}
			changed = true
		}
		for _, c := range profileChildren {
			table := strings.Replace(c.local, "spool_", "spoolman_", 1)
			existing, e := list(r.tx, "SELECT * FROM "+table+" WHERE spoolman_spool_id=?", sp["id"])
			if e != nil {
				return nil, e
			}
			have := map[string]bool{}
			for _, x := range existing {
				have[childIdentity(c, x)] = true
			}
			for _, x := range detail[c.list].([]row) {
				if have[childIdentity(c, x)] {
					continue
				}
				v := obj(x, c.fields)
				v["spoolman_spool_id"] = sp["id"]
				if _, e = insert(r.tx, table, v); e != nil {
					return nil, e
				}
				changed = true
			}
		}
		if changed {
			n++
		}
	}
	return row{"updated": n}, nil
}
func nativeTag(v string) string {
	t := strings.ToUpper(v)
	if len(t) < 8 || len(t) > 32 || len(t)%2 != 0 || strings.Trim(t, "0123456789ABCDEF") != "" || strings.Trim(t, "0") == "" {
		return ""
	}
	if len(t) == 16 && strings.HasSuffix(t, "00000100") {
		return t[:8]
	}
	return t
}
func migrateRemoteTags(r request) (any, error) {
	dry := r.URL.Query().Get("dry_run") != "false"
	if e := remote(r, "GET", "tag/reader", nil, nil); e != nil {
		return nil, &problem{409, "Spoolman native tags unavailable"}
	}
	var spools []row
	if e := remote(r, "GET", "spool?allow_archived=true", nil, &spools); e != nil {
		return nil, e
	}
	owners := map[string]int64{}
	archived := map[int64]bool{}
	for _, v := range spools {
		id := intID(v["id"])
		archived[id] = truth(v["archived"])
		ts, _ := v["tags"].([]any)
		for _, t := range ts {
			owners[strings.ToUpper(str(sub(t)["uid"]))] = id
		}
	}
	moved, conflicts := []row{}, []row{}
	already, slotIDs := 0, 0
	for _, v := range spools {
		id := intID(v["id"])
		tag := extraText(sub(v["extra"]), "tag")
		if archived[id] || tag == "" {
			continue
		}
		uid := nativeTag(tag)
		if uid == "" {
			slotIDs++
			continue
		}
		if e := remoteHolders(r, id); e != nil {
			continue
		}
		holder := owners[uid]
		if holder == id {
			already++
			continue
		}
		if holder > 0 && !archived[holder] {
			conflicts = append(conflicts, row{"spool_id": id, "holder": holder, "tag": uid})
			continue
		}
		if !dry {
			if holder > 0 {
				if e := remote(r, "DELETE", fmt.Sprintf("spool/%d/tag/%s", holder, uid), nil, nil); e != nil {
					return nil, e
				}
			}
			body := row{"uid": uid}
			if len(tag) == 32 || len(tag) == 16 && len(uid) == 8 {
				body["format"] = "bambu"
			}
			if e := remote(r, "POST", fmt.Sprintf("spool/%d/tag", id), body, nil); e != nil {
				return nil, e
			}
		}
		moved = append(moved, row{"spool_id": id, "tag": uid})
		owners[uid] = id
	}
	return row{"dry_run": dry, "already": already, "slot_ids": slotIDs, "moved": moved, "conflicts": conflicts}, nil
}
func (s *Service) remoteLink(r request, unlink bool) (any, error) {
	id := r.id()
	if e := remoteHolders(r, id); e != nil {
		return nil, e
	}
	v, e := rawSpool(r, id)
	if e != nil {
		return nil, e
	}
	extra := sub(v["extra"])
	tag := ""
	var key *slotKey
	if !unlink {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		for _, field := range []string{"tray_uuid", "tag_uid", "spool_tag"} {
			if t := strings.TrimSpace(str(d[field])); t != "" {
				tag = strings.ToUpper(t)
				break
			}
		}
		if d["printer_id"] != nil || d["ams_id"] != nil || d["tray_id"] != nil {
			if !validSlot(d) {
				return nil, invalid("Provide a complete AMS slot")
			}
			k := slotKey{intID(d["printer_id"]), int64(num(d["ams_id"])), int64(num(d["tray_id"]))}
			key = &k
			if !access.From(r.Context()).AllowsPrinter(k.printer) {
				return nil, notfound()
			}
			pr, e := get(r.tx, "printers", k.printer)
			if e != nil {
				return nil, e
			}
			if tag == "" {
				tag = fmt.Sprintf("%s_%d_%d", str(pr["serial_number"]), k.ams, k.tray)
			}
		}
		if tag == "" || len(tag) > 128 {
			return nil, invalid("Provide a tag or AMS slot")
		}
		var raw []row
		if e = remote(r, "GET", "spool", nil, &raw); e != nil {
			return nil, e
		}
		for _, x := range raw {
			if !truth(x["archived"]) && intID(x["id"]) != id && strings.EqualFold(extraText(sub(x["extra"]), "tag"), tag) {
				return nil, &tagConflict{"tag", intID(x["id"])}
			}
		}
	}
	b, _ := json.Marshal(tag)
	extra["tag"] = string(b)
	if e = ensureExtras(r, []string{"tag"}); e != nil {
		return nil, e
	}
	if e = remote(r, "PATCH", fmt.Sprintf("spool/%d", id), row{"extra": extra}, nil); e != nil {
		return nil, e
	}
	if key != nil {
		if e = r.bind(true, id, *key); e != nil {
			return nil, e
		}
	} else if unlink {
		if ts, ok := v["tags"].([]any); ok {
			for _, v := range ts {
				uid := str(sub(v)["uid"])
				if t := nativeTag(uid); t != "" {
					if e = remote(r, "DELETE", fmt.Sprintf("spool/%d/tag/%s", id, url.PathEscape(uid)), nil, nil); e != nil {
						return nil, e
					}
				}
			}
		}
		if _, e = r.tx.Exec("DELETE FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", id); e != nil {
			return nil, e
		}
	}
	r.event(row{"type": "inventory_changed"})
	return row{"success": true, "message": "Spool link updated"}, nil
}
func (s *Service) syncRemoteSlots(r request, all bool) (any, error) {
	if _, e := remoteBase(r); e != nil {
		return nil, e
	}
	actor := access.From(r.Context())
	var ps []row
	var e error
	if all {
		ps, e = list(r.tx, "SELECT id,name FROM printers WHERE is_active ORDER BY id")
	} else {
		if !actor.AllowsPrinter(r.id()) {
			return nil, notfound()
		}
		var v row
		v, e = get(r.tx, "printers", r.id())
		ps = []row{v}
	}
	if e != nil {
		return nil, e
	}
	synced := 0
	skipped := []row{}
	errs := []string{}
	for _, p := range ps {
		id := intID(p["id"])
		if !actor.AllowsPrinter(id) {
			continue
		}
		slots, busy, online := s.slots(id)
		if !online {
			if !all {
				return nil, &problem{409, "Printer is disconnected"}
			}
			continue
		}
		keys := []slotKey{}
		for k := range slots {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { return keys[i].ams*10+keys[i].tray < keys[j].ams*10+keys[j].tray })
		for _, k := range keys {
			tray := slots[k]
			exists, e := one(r.tx, "SELECT spoolman_spool_id FROM spoolman_slot_assignments WHERE printer_id=? AND ams_id=? AND tray_id=?", id, k.ams, k.tray)
			if e != nil && !errors.Is(e, sql.ErrNoRows) {
				return nil, e
			}
			if str(tray["tray_type"]) == "" {
				if !busy && exists != nil && tray["tray_type"] != nil {
					if _, e = r.tx.Exec("DELETE FROM spoolman_slot_assignments WHERE printer_id=? AND ams_id=? AND tray_id=?", id, k.ams, k.tray); e != nil {
						return nil, e
					}
				}
				continue
			}
			if cleanTag(tray["tray_uuid"], 32) == nil && cleanTag(tray["tag_uid"], 16) == nil {
				if exists != nil {
					synced++
					continue
				}
				skipped = append(skipped, row{"location": fmt.Sprintf("%s AMS %d slot %d", str(p["name"]), k.ams, k.tray), "reason": "No RFID tag and no slot assignment", "filament_type": tray["tray_type"], "color": tray["tray_color"]})
				continue
			}
			if _, e = s.fromSlot(r, k, true); e != nil {
				errs = append(errs, fmt.Sprintf("%s AMS %d slot %d: %s", str(p["name"]), k.ams, k.tray, e))
				continue
			}
			synced++
		}
	}
	return row{"success": len(errs) == 0, "synced_count": synced, "skipped_count": len(skipped), "skipped": skipped, "errors": errs}, nil
}
func (s *Service) remoteFlows(m *http.ServeMux) {
	s.route(m, "POST", "spoolman/connect", "settings:update", func(r request) (any, error) {
		if _, e := r.tx.Exec("DELETE FROM settings WHERE key='bambastic_spoolman_disconnected'"); e != nil {
			return nil, e
		}
		if e := remote(r, "GET", "health", nil, nil); e != nil {
			return nil, e
		}
		if e := ensureExtras(r, []string{"tag", "bambu_slicer_filament", "bambu_slicer_filament_name"}); e != nil {
			return nil, e
		}
		return row{"success": true, "message": "Connected to Spoolman"}, nil
	})
	s.route(m, "POST", "spoolman/disconnect", "settings:update", func(r request) (any, error) {
		_, e := r.tx.Exec("INSERT INTO settings(key,value) VALUES('bambastic_spoolman_disconnected','true') ON CONFLICT(key) DO UPDATE SET value='true'")
		return row{"success": true, "message": "Disconnected from Spoolman"}, e
	})
	for _, b := range []bool{false, true} {
		action := "link"
		endpoint := "unlinked"
		if b {
			action = "unlink"
			endpoint = "linked"
		}
		s.route(m, "GET", "spoolman/spools/"+endpoint, "filaments:read", func(r request) (any, error) { return linkedSpools(r, b) })
		s.route(m, "POST", "spoolman/spools/{id}/"+action, "filaments:update", func(r request) (any, error) { return s.remoteLink(r, b) })
	}
	s.route(m, "POST", "spoolman/sync/{id}", "filaments:update", func(r request) (any, error) { return s.syncRemoteSlots(r, false) })
	s.route(m, "POST", "spoolman/sync-all", "filaments:update", func(r request) (any, error) { return s.syncRemoteSlots(r, true) })
	const prefix = "spoolman/inventory/"
	s.route(m, "PATCH", prefix+"filaments/{id}", inventoryWrite, patchRemoteFilament)
	s.route(m, "POST", prefix+"tags/migrate", inventoryWrite, migrateRemoteTags)
	s.route(m, "POST", prefix+"profile-defaults/{id}/apply", inventoryWrite, applyRemoteDefault)
	for _, op := range []string{"delete", "archive", "restore", "reset-consumed-counter"} {
		key := map[string]string{"delete": "deleted", "archive": "archived", "restore": "restored", "reset-consumed-counter": "reset"}[op]
		path := "bulk-" + op
		if op == "reset-consumed-counter" {
			path = op + "-bulk"
		}
		s.route(m, "POST", prefix+"spools/"+path, inventoryWrite, func(r request) (any, error) {
			d, e := r.body()
			if e != nil {
				return nil, e
			}
			ids, ok := d["ids"].([]any)
			if op == "reset-consumed-counter" {
				ids, ok = d["spool_ids"].([]any)
			}
			if !ok || len(ids) == 0 || len(ids) > 2048 {
				return nil, invalid("Provide a nonempty list of spool IDs")
			}
			for _, v := range ids {
				if intID(v) <= 0 {
					return nil, invalid("Invalid spool ID")
				}
			}
			failures := []row{}
			changed := 0
			seen := map[int64]bool{}
			for _, v := range ids {
				id := intID(v)
				if seen[id] {
					continue
				}
				seen[id] = true
				if _, e := remoteAction(r, id, op); e != nil {
					var p *problem
					code := 500
					if errors.As(e, &p) {
						code = p.code
					}
					failures = append(failures, row{"id": id, "status": code, "detail": e.Error()})
				} else {
					changed++
				}
			}
			if changed > 0 {
				r.event(row{"type": "inventory_changed"})
			}
			return row{key: changed, "errors": failures}, nil
		})
	}
}
