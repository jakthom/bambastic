package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/lanhttp"
)

func stockStats(r request, supplier bool) (any, error) {
	group := "s.material_number"
	name := "s.material_number"
	join := ""
	filter := " AND s.material_number IS NOT NULL AND s.material_number!=''"
	if supplier {
		group = "p.id"
		name = "p.name"
		join = " JOIN spool_suppliers link ON link.spool_id=s.id AND link.is_purchase_source JOIN suppliers p ON p.id=link.supplier_id"
		filter = ""
	}
	loc := time.Local
	if zone := r.URL.Query().Get("tz"); zone != "" {
		var e error
		loc, e = time.LoadLocation(zone)
		if e != nil {
			return nil, invalid("Unknown time zone")
		}
	}
	params := []any{}
	window := ""
	for _, key := range []string{"date_from", "date_to"} {
		v := r.URL.Query().Get(key)
		if v == "" {
			continue
		}
		t, e := time.ParseInLocation("2006-01-02", v, loc)
		if e != nil {
			return nil, invalid("Invalid calendar date")
		}
		op := ">="
		if key == "date_to" {
			op = "<"
			t = t.AddDate(0, 0, 1)
		}
		window += " AND julianday(h.created_at)" + op + "julianday(?)"
		params = append(params, t.UTC().Format(time.RFC3339))
	}
	stock, e := list(r.tx, "SELECT "+group+" grp,"+name+" name,count(*) spool_count,sum(max(0,s.label_weight-s.weight_used)) remaining_g FROM spool s"+join+" WHERE s.archived_at IS NULL"+filter+" GROUP BY "+group)
	if e != nil {
		return nil, e
	}
	hist, e := list(r.tx, "SELECT "+group+" grp,"+name+" name,h.printer_id,sum(h.weight_used) grams,sum(h.cost) cost FROM spool_usage_history h JOIN spool s ON s.id=h.spool_id"+join+" WHERE 1=1"+filter+window+" GROUP BY "+group+",h.printer_id", params...)
	if e != nil {
		return nil, e
	}
	actor := access.From(r.Context())
	entries := map[string]row{}
	makeRow := func(v row) row {
		k := str(v["grp"])
		a := entries[k]
		if a == nil {
			a = row{"spool_count": 0, "remaining_g": 0.0, "consumed_g": 0.0, "cost": 0.0}
			if supplier {
				a["supplier_id"] = v["grp"]
				a["supplier_name"] = v["name"]
			} else {
				a["material_number"] = v["name"]
			}
			entries[k] = a
		}
		return a
	}
	for _, v := range stock {
		a := makeRow(v)
		a["spool_count"], a["remaining_g"] = v["spool_count"], v["remaining_g"]
	}
	for _, v := range hist {
		if !actor.AllowsPrinter(intID(v["printer_id"])) {
			continue
		}
		a := makeRow(v)
		a["consumed_g"] = num(a["consumed_g"]) + num(v["grams"])
		a["cost"] = num(a["cost"]) + num(v["cost"])
	}
	rs := []row{}
	for _, v := range entries {
		rs = append(rs, v)
	}
	sort.Slice(rs, func(i, j int) bool {
		l, m := rs[i], rs[j]
		if num(l["consumed_g"]) == num(m["consumed_g"]) {
			if supplier {
				return str(l["supplier_name"]) < str(m["supplier_name"])
			}
			return str(l["material_number"]) < str(m["material_number"])
		}
		return num(l["consumed_g"]) > num(m["consumed_g"])
	})
	return rs, nil
}
func (s *Service) colorsSync(w http.ResponseWriter, r *http.Request) {
	if !access.From(r.Context()).Has(inventoryWrite) {
		writeError(w, denied())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	stream := func(v row) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "data: %s\n\n", b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	root := s.colorsURL
	if root == "" {
		root = "https://filamentcolors.xyz/api"
	}
	added, skipped, fetched, total := 0, 0, 0, 0
	for page := 1; page <= 10000; page++ {
		req, e := http.NewRequestWithContext(r.Context(), "GET", fmt.Sprintf("%s/swatch/?page=%d", root, page), nil)
		if e != nil {
			stream(row{"type": "error", "error": "Invalid swatch endpoint"})
			return
		}
		req.Header.Set("User-Agent", "Bambastic")
		resp, e := lanhttp.Client.Do(req)
		if e != nil {
			stream(row{"type": "error", "error": "Color catalog unavailable"})
			return
		}
		var data struct {
			Count   int
			Next    string
			Results []row
		}
		e = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&data)
		resp.Body.Close()
		if e != nil || resp.StatusCode != 200 {
			stream(row{"type": "error", "error": "Invalid color catalog response"})
			return
		}
		total = data.Count
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e == nil {
			for _, v := range data.Results {
				fetched++
				brand := str(sub(v["manufacturer"])["name"])
				mat := str(sub(v["filament_type"])["name"])
				name := str(v["color_name"])
				rgba, ok := csvRGBA(str(v["hex_color"]))
				if !ok || brand == "" || name == "" {
					skipped++
					continue
				}
				res, err := tx.Exec("INSERT INTO color_catalog(manufacturer,color_name,material,hex_color,is_default) SELECT ?,?,?,?,0 WHERE NOT EXISTS(SELECT 1 FROM color_catalog WHERE manufacturer=? COLLATE NOCASE AND color_name=? COLLATE NOCASE AND coalesce(material,'')=? COLLATE NOCASE)", brand, name, mat, "#"+strings.ToUpper(rgba[:6]), brand, name, mat)
				if err != nil {
					e = err
					break
				}
				n, _ := res.RowsAffected()
				if n == 1 {
					added++
				} else {
					skipped++
				}
			}
			if e == nil {
				e = tx.Commit()
			} else {
				tx.Rollback()
			}
		}
		if e != nil {
			stream(row{"type": "error", "error": "Color catalog update failed"})
			return
		}
		stage := "progress"
		if data.Next == "" || len(data.Results) == 0 {
			stage = "complete"
		}
		stream(row{"type": stage, "added": added, "skipped": skipped, "total_fetched": fetched, "total_available": total})
		if stage == "complete" {
			if s.emit != nil {
				s.emit(row{"type": "inventory_changed"})
			}
			return
		}
	}
	stream(row{"type": "error", "error": "Color catalog pagination limit exceeded"})
}

type slotKey struct{ printer, ams, tray int64 }

func (r request) canMove(table, col string, id int64, key slotKey) error {
	ex, e := list(r.tx, "SELECT printer_id FROM "+table+" WHERE "+col+"=?", id)
	if e != nil {
		return e
	}
	a := access.From(r.Context())
	for _, x := range ex {
		if !a.AllowsPrinter(intID(x["printer_id"])) {
			return denied()
		}
	}
	if !a.AllowsPrinter(key.printer) {
		return notfound()
	}
	return nil
}
func (r request) bind(remote bool, id int64, key slotKey) error {
	table, col := "spool_assignment", "spool_id"
	if remote {
		table, col = "spoolman_slot_assignments", "spoolman_spool_id"
	}
	if e := r.canMove(table, col, id, key); e != nil {
		return e
	}
	if _, e := r.tx.Exec("DELETE FROM "+table+" WHERE "+col+"=? OR (printer_id=? AND ams_id=? AND tray_id=?)", id, key.printer, key.ams, key.tray); e != nil {
		return e
	}
	v := row{col: id, "printer_id": key.printer, "ams_id": key.ams, "tray_id": key.tray}
	if !remote {
		v["assigned_by_user_id"] = nullID(access.From(r.Context()).ID)
	}
	_, e := insert(r.tx, table, v)
	if e == nil {
		e = noteAssignment(r, key, id, remote)
	}
	if e == nil {
		r.event(row{"type": "spool_auto_assigned", "printer_id": key.printer, "ams_id": key.ams, "tray_id": key.tray, col: id})
	}
	return e
}
func (s *Service) slots(id int64) (map[slotKey]row, bool, bool) {
	if s.MaterialSlots == nil {
		return nil, false, false
	}
	report, ok := s.MaterialSlots(id)
	if !ok {
		return nil, false, false
	}
	return reportSlots(id, report)
}
func reportSlots(id int64, report map[string]any) (map[slotKey]row, bool, bool) {
	out := map[slotKey]row{}
	var units []any
	var bits uint64
	var hasMask bool
	switch a := report["ams"].(type) {
	case []any:
		units = a
	case map[string]any:
		if raw := a["tray_exist_bits"]; raw != nil {
			var err error
			switch val := raw.(type) {
			case string:
				bits, err = strconv.ParseUint(strings.TrimPrefix(val, "0x"), 16, 64)
			case float64:
				bits = uint64(val)
			default:
				err = errors.New("unknown presence mask")
			}
			hasMask = err == nil && (bits != 0 || report["power_on_flag"] == nil || truth(report["power_on_flag"]))
		}
		units, _ = a["ams"].([]any)
		if units == nil && a["tray"] != nil {
			units = []any{a}
		}
	}
	for _, item := range units {
		u := sub(item)
		unit, err := strconv.ParseInt(str(u["id"]), 10, 64)
		if err != nil {
			continue
		}
		if unit == 16 {
			unit = 6
		}
		trays, _ := u["tray"].([]any)
		for _, raw := range trays {
			t := sub(raw)
			slot, err := strconv.ParseInt(str(t["id"]), 10, 64)
			if err != nil || slot < 0 || slot > 8 {
				continue
			}
			if hasMask {
				bit := int64(-1)
				switch {
				case unit >= 0 && unit <= 7:
					bit = unit*4 + slot
				case unit == 32:
					bit = 28 + slot
				case unit >= 128 && unit <= 135:
					bit = 16 + unit - 128
				}
				if bit >= 0 && bit < 64 {
					cp := row{}
					for key, val := range t {
						cp[key] = val
					}
					t = cp
					t["exists"] = bits&(1<<uint(bit)) != 0
					if t["exists"] == false {
						for _, field := range []string{"tray_type", "tray_sub_brands", "tray_color", "tray_uuid", "tag_uid", "tray_info_idx", "tray_id_name"} {
							t[field] = ""
						}
						t["remain"] = nil
					}
				}
			}
			out[slotKey{id, unit, slot}] = t
		}
	}
	for _, k := range []string{"vt_tray", "vt_tray_list"} {
		a, ok := report[k].([]any)
		if !ok && report[k] != nil {
			a = []any{report[k]}
		}
		for pos, x := range a {
			t := sub(x)
			if t["tray_type"] != nil {
				idx := int64(pos)
				if n := int(num(t["id"])); n >= 254 && n <= 255 {
					idx = int64(n - 254)
				} else if n >= 247 && n <= 253 {
					idx = int64(n - 245)
				}
				out[slotKey{id, 255, idx}] = t
			}
		}
	}
	busy := true
	switch strings.ToUpper(str(report["gcode_state"])) {
	case "IDLE", "FINISH", "FAILED", "READY":
		busy = false
	}
	return out, busy, true
}
func (s *Service) fromSlot(r request, key slotKey, isRemote bool) (row, error) {
	if !access.From(r.Context()).AllowsPrinter(key.printer) {
		return nil, notfound()
	}
	slots, _, online := s.slots(key.printer)
	if !online {
		return nil, &problem{404, "No connected printer status"}
	}
	tray := slots[key]
	if str(tray["tray_type"]) == "" {
		return nil, bad("Slot is empty")
	}
	tag, uuid := cleanTag(tray["tag_uid"], 16), cleanTag(tray["tray_uuid"], 32)
	if tag == nil && uuid == nil {
		return nil, bad("Slot has no RFID tag")
	}
	var v row
	var e error
	if isRemote {
		v, e = remoteTag(r, str(uuid), str(tag))
	} else {
		v, e = byTag(r, str(uuid), str(tag), false)
	}
	if e != nil {
		var err *problem
		if !errors.Is(e, sql.ErrNoRows) && !(errors.As(e, &err) && err.code == 404) {
			return nil, e
		}
		d, e := materialFromTray(r.tx, tray)
		if e != nil {
			return nil, e
		}
		if isRemote {
			v, e = writeRemoteSpool(r, 0, d, true)
		} else {
			v, e = saveSpool(r, 0, d)
		}
		if e != nil {
			return nil, e
		}
	}
	if e = r.bind(isRemote, intID(v["id"]), key); e != nil {
		return nil, e
	}
	return v, nil
}
func (s *Service) syncWeights(r request, isRemote bool) (any, error) {
	table := "spool_assignment"
	col := "spool_id"
	if isRemote {
		table = "spoolman_slot_assignments"
		col = "spoolman_spool_id"
	}
	rs, e := list(r.tx, "SELECT * FROM "+table)
	if e != nil {
		return nil, e
	}
	synced, skipped, printing := 0, 0, 0
	for _, v := range rs {
		pid := intID(v["printer_id"])
		if !access.From(r.Context()).AllowsPrinter(pid) {
			continue
		}
		slots, busy, online := s.slots(pid)
		if !online {
			skipped++
			continue
		}
		open, e := count(r.tx, "SELECT count(*) FROM print_log_entries WHERE printer_id=? AND status IN ('printing','running','paused')", pid)
		if e != nil {
			return nil, e
		}
		if busy || open > 0 {
			skipped++
			printing++
			continue
		}
		tray := slots[slotKey{pid, int64(num(v["ams_id"])), int64(num(v["tray_id"]))}]
		rem := num(tray["remain"])
		if tray["remain"] == nil || rem < 0 || rem > 100 || str(tray["tray_type"]) == "" {
			skipped++
			continue
		}
		id := intID(v[col])
		if isRemote {
			sp, e := rawSpool(r, id)
			if e != nil {
				skipped++
				continue
			}
			net, _, _ := remoteWeights(sp)
			if e = remote(r, "PATCH", fmt.Sprintf("spool/%d", id), row{"remaining_weight": net * rem / 100}, nil); e != nil {
				skipped++
				continue
			}
		} else {
			if _, e = r.tx.Exec("UPDATE spool SET weight_used=label_weight*(100-?)/100.0,weight_locked=0,updated_at=CURRENT_TIMESTAMP WHERE id=?", rem, id); e != nil {
				return nil, e
			}
		}
		synced++
	}
	return row{"synced": synced, "skipped": skipped, "printing": printing, "skipped_printing": printing}, nil
}
func (s *Service) materialRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/inventory/colors/sync", s.colorsSync)
	for _, name := range []string{"suppliers", "material-numbers"} {
		s.route(m, "GET", "inventory/stats/"+name, inventoryRead, func(r request) (any, error) { return stockStats(r, name == "suppliers") })
	}
	for _, isRemote := range []bool{false, true} {
		pre := "inventory"
		from := "inventory/spools/from-slot"
		perm := inventoryWrite
		if isRemote {
			pre = "spoolman/inventory"
			from = "spoolman/spools/from-slot"
			perm = "filaments:update"
		}
		s.route(m, "POST", pre+"/sync-ams-weights", inventoryWrite, func(r request) (any, error) { return s.syncWeights(r, isRemote) })
		s.route(m, "POST", from, perm, func(r request) (any, error) {
			d, e := r.body()
			if e != nil {
				return nil, e
			}
			if !validSlot(d) {
				return nil, invalid("Invalid AMS slot")
			}
			v, e := s.fromSlot(r, slotKey{intID(d["printer_id"]), int64(num(d["ams_id"])), int64(num(d["tray_id"]))}, isRemote)
			if isRemote && e == nil {
				return row{"success": true, "spool_id": v["id"]}, nil
			}
			return v, e
		})
	}
}
func validSlot(d row) bool {
	p := intID(d["printer_id"])
	a, t := num(d["ams_id"]), num(d["tray_id"])
	return d["ams_id"] != nil && d["tray_id"] != nil && p > 0 && a == math.Trunc(a) && t == math.Trunc(t) && (a >= 0 && a <= 7 || a == 32 || a == 255 || a >= 128 && a <= 191) && t >= 0 && (t <= 3 || (a == 32 || a == 255) && t <= 8)
}

// RemainingStock maps occupied AMS slots to a known net weight in grams.
// Unassigned slots have no entry. Remote failures preserve the distinction
// between a known empty spool and a temporarily unreadable inventory.
func RemainingStock(ctx context.Context, tx *sql.Tx, printerID int64, p access.Principal) (map[[2]int64]float64, error) {
	out := map[[2]int64]float64{}
	if printerID <= 0 || !p.AllowsPrinter(printerID) {
		return nil, notfound()
	}
	if !remoteEnabled(ctx, tx) {
		rows, e := list(tx, "SELECT a.ams_id,a.tray_id,max(0,s.label_weight-s.weight_used) remaining FROM spool_assignment a JOIN spool s ON a.spool_id=s.id WHERE a.printer_id=? AND s.archived_at IS NULL", printerID)
		if e != nil {
			return nil, e
		}
		for _, v := range rows {
			out[[2]int64{int64(num(v["ams_id"])), int64(num(v["tray_id"]))}] = num(v["remaining"])
		}
		return out, nil
	}
	assigned, e := list(tx, "SELECT ams_id,tray_id,spoolman_spool_id FROM spoolman_slot_assignments WHERE printer_id=?", printerID)
	if e != nil || len(assigned) == 0 {
		return out, e
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "/", nil)
	r := request{Request: req, tx: tx}
	var spools []row
	if e = remote(r, "GET", "spool", nil, &spools); e != nil {
		return nil, e
	}
	weights := map[int64]float64{}
	for _, s := range spools {
		if truth(s["archived"]) {
			continue
		}
		if v := s["remaining_weight"]; v != nil {
			weights[intID(s["id"])] = math.Max(0, num(v))
		} else if s["used_weight"] != nil && (s["initial_weight"] != nil || sub(s["filament"])["weight"] != nil) {
			net, _, _ := remoteWeights(s)
			weights[intID(s["id"])] = math.Max(0, net-num(s["used_weight"]))
		}
	}
	for _, v := range assigned {
		if g, known := weights[intID(v["spoolman_spool_id"])]; known {
			out[[2]int64{int64(num(v["ams_id"])), int64(num(v["tray_id"]))}] = g
		}
	}
	return out, nil
}

// MaterialSlotReports returns the normalized unit and local holder for each observed slot.
// A missing slot has no presence evidence. The calibrated firmware mask, when available,
// is reflected in the per-slot exists field.
func MaterialSlotReports(printerID int64, report map[string]any) map[[2]int64]map[string]any {
	rows, _, _ := reportSlots(printerID, report)
	normalized := make(map[[2]int64]map[string]any, len(rows))
	for key, slot := range rows {
		normalized[[2]int64{key.ams, key.tray}] = slot
	}
	return normalized
}
