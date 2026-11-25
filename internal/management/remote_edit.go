package management

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strings"

	"bambastic/internal/access"
)

func equalText(a, b any) bool {
	return strings.EqualFold(strings.TrimSpace(str(a)), strings.TrimSpace(str(b)))
}
func colorHex(v any) string {
	c := strings.ToUpper(strings.TrimPrefix(strings.TrimSpace(str(v)), "#"))
	if len(c) == 8 && strings.HasSuffix(c, "FF") {
		c = c[:6]
	}
	return c
}
func resolveFilament(r request, d row) (int64, error) {
	if id := intID(d["spoolman_filament_id"]); id != 0 {
		return id, nil
	}
	material, subtype, brand := strings.TrimSpace(str(d["material"])), strings.TrimSpace(str(d["subtype"])), strings.TrimSpace(str(d["brand"]))
	if material == "" {
		return 0, invalid("material or spoolman_filament_id is required")
	}
	rgba := colorHex(d["rgba"])
	if rgba == "" {
		rgba = "808080"
	}
	name := strings.TrimSpace(material + " " + subtype)
	var existing []row
	if e := remote(r, "GET", "filament", nil, &existing); e != nil {
		return 0, e
	}
	for _, f := range existing {
		if !equalText(f["material"], material) || (!equalText(f["name"], name) && !equalText(f["name"], subtype)) || colorHex(f["color_hex"]) != rgba || brand != "" && !equalText(sub(f["vendor"])["name"], brand) {
			continue
		}
		if !equalText(d["color_name"], "") && !equalText(f["color_name"], d["color_name"]) {
			continue
		}
		return intID(f["id"]), nil
	}
	f := row{"material": material, "name": name, "diameter": 1.75, "density": 1.24, "color_hex": rgba}
	if c := str(d["color_name"]); c != "" {
		f["color_name"] = c
	}
	if n := num(d["label_weight"]); n > 0 {
		f["weight"] = n
	}
	if brand != "" {
		var vendors []row
		if e := remote(r, "GET", "vendor", nil, &vendors); e != nil {
			return 0, e
		}
		for _, v := range vendors {
			if equalText(v["name"], brand) {
				f["vendor_id"] = v["id"]
				break
			}
		}
		if f["vendor_id"] == nil {
			var v row
			if e := remote(r, "POST", "vendor", row{"name": brand}, &v); e != nil {
				return 0, e
			}
			f["vendor_id"] = v["id"]
		}
	}
	var out row
	if e := remote(r, "POST", "filament", f, &out); e != nil {
		return 0, e
	}
	if intID(out["id"]) == 0 {
		return 0, &problem{502, "Spoolman did not create a filament"}
	}
	return intID(out["id"]), nil
}
func ensureExtras(r request, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	var fields []row
	if e := remote(r, "GET", "field/spool", nil, &fields); e != nil {
		return e
	}
	have := map[string]bool{}
	for _, f := range fields {
		have[str(f["key"])] = true
	}
	for _, k := range keys {
		if !have[k] {
			if e := remote(r, "POST", "field/spool/"+k, row{"name": k, "field_type": "text", "default_value": nil}, nil); e != nil {
				return e
			}
		}
	}
	return nil
}
func saveRemoteSpool(r request, id int64, raw row) (row, error) {
	return writeRemoteSpool(r, id, raw, false)
}

// A printer-originated tag has to be included in the original Spoolman create
// request. Otherwise a lost response could create a second roll on the next scan.
func writeRemoteSpool(r request, id int64, raw row, fromReader bool) (row, error) {
	model := "SpoolmanInventoryCreate"
	if id > 0 {
		model = "SpoolmanInventoryUpdate"
	}
	d, e := input(model, raw)
	if e != nil {
		return nil, e
	}
	if fromReader && id == 0 {
		for key, length := range map[string]int{"tag_uid": 16, "tray_uuid": 32} {
			if uid := cleanTag(raw[key], length); uid != nil {
				d[key] = uid
			}
		}
	}
	payload := row{}
	var old row
	record := row{}
	extra := row{}
	net := num(d["label_weight"])
	if id != 0 {
		old, e = rawSpool(r, id)
		if e != nil {
			return nil, e
		}
		record = remoteSpool(old)
		net = num(record["label_weight"])
		extra = sub(old["extra"])
	}
	if label := d["label_weight"]; label != nil {
		net = num(label)
		payload["initial_weight"] = label
	}
	if cost, ok := d["cost_per_kg"]; ok {
		if cost == nil {
			payload["price"] = nil
		} else {
			payload["price"] = math.Round(num(cost)*net/1000*10000) / 10000
		}
	}
	if w, ok := d["weight_used"]; ok && w != nil {
		payload["remaining_weight"] = math.Max(0, net-num(w))
	}
	for k, remoteKey := range map[string]string{"note": "comment", "core_weight": "spool_weight", "storage_location": "location"} {
		if v, ok := d[k]; ok {
			payload[remoteKey] = v
		}
	}
	if loc, ok := d["location_id"]; ok {
		if loc == nil {
			if _, explicit := d["storage_location"]; !explicit {
				payload["location"] = nil
			}
		} else {
			v, e := get(r.tx, "locations", intID(loc))
			if e != nil {
				return nil, e
			}
			payload["location"] = v["name"]
		}
	}
	changeFilament := id == 0
	for _, k := range []string{"brand", "material", "subtype", "rgba", "color_name"} {
		if v, ok := d[k]; ok && !equalText(v, record[k]) {
			changeFilament = true
		}
	}
	if changeFilament {
		for k, v := range d {
			record[k] = v
		}
		f, e := resolveFilament(r, record)
		if e != nil {
			return nil, e
		}
		payload["filament_id"] = f
	}
	keys := []string{}
	for _, k := range []string{"color_name", "slicer_filament", "slicer_filament_name", "last_dried_at"} {
		if v, ok := d[k]; ok {
			field := "bambu_" + k
			text, _ := json.Marshal(str(v))
			extra[field] = string(text)
			keys = append(keys, field)
		}
	}
	for _, key := range []string{"tray_uuid", "tag_uid"} {
		if val, ok := d[key]; ok && val != nil {
			uid := strings.ToUpper(str(val))
			if strings.Trim(uid, "0123456789ABCDEF") != "" || strings.Trim(uid, "0") == "" {
				return nil, invalid("Invalid tag")
			}
			match, e := remoteTag(r, str(d["tray_uuid"]), str(d["tag_uid"]))
			if e != nil {
				p, ok := e.(*problem)
				if !ok || p.code != 404 {
					return nil, e
				}
			}
			if match != nil && intID(match["id"]) != id {
				return nil, &tagConflict{key, intID(match["id"])}
			}
			text, _ := json.Marshal(uid)
			extra["tag"] = string(text)
			keys = append(keys, "tag")
			break
		}
	}
	if len(keys) > 0 {
		if e = ensureExtras(r, keys); e != nil {
			return nil, e
		}
		payload["extra"] = extra
	}
	var out row
	method, path := "POST", "spool"
	if id > 0 {
		method, path = "PATCH", fmt.Sprintf("spool/%d", id)
	}
	if e = remote(r, method, path, payload, &out); e != nil {
		return nil, e
	}
	r.event(row{"type": "inventory_changed"})
	return remoteSpool(out), nil
}
func (s *Service) remoteEdit(m *http.ServeMux) {
	const base = "spoolman/inventory/spools"
	for _, op := range []string{"POST", "PATCH"} {
		path := base
		if op == "PATCH" {
			path += "/{id}"
		}
		s.route(m, op, path, inventoryWrite, func(r request) (any, error) {
			d, e := r.body()
			if e != nil {
				return nil, e
			}
			if op == "PATCH" && r.id() == 0 {
				return nil, notfound()
			}
			return saveRemoteSpool(r, r.id(), d)
		})
	}
	s.route(m, "PATCH", base+"/{id}/tag", inventoryWrite, func(r request) (any, error) {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		if intID(r.id()) == 0 {
			return nil, notfound()
		}
		if d["tag_uid"] == nil && d["tray_uuid"] == nil {
			return nil, invalid("Tag identifier required")
		}
		_, e = saveRemoteSpool(r, r.id(), obj(d, "tag_uid tray_uuid"))
		return row{"status": "ok"}, e
	})
	s.route(m, "POST", base+"/bulk", inventoryWrite, func(r request) (any, error) {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		n := 1
		if d["quantity"] != nil {
			n = int(num(d["quantity"]))
		}
		if n < 1 || n > 100 {
			return nil, invalid("quantity must be between 1 and 100")
		}
		sp := sub(d["spool"])
		out, failures := []row{}, []row{}
		for i := 0; i < n; i++ {
			s, e := saveRemoteSpool(r, 0, sp)
			if e != nil {
				failures = append(failures, row{"index": i, "detail": e.Error()})
			} else {
				out = append(out, s)
			}
		}
		if len(failures) > 0 {
			return response{207, row{"created": out, "requested_count": n, "failed_count": len(failures), "failures": failures}}, nil
		}
		return out, nil
	})
	s.route(m, "POST", base+"/bulk-update", inventoryWrite, func(r request) (any, error) {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		ids, ok := d["spool_ids"].([]any)
		if !ok {
			ids, ok = d["ids"].([]any)
		}
		if !ok || len(ids) > 100 || len(ids) == 0 {
			return nil, invalid("Expected a list of spool IDs")
		}
		out := []int64{}
		failures := []row{}
		for _, k := range ids {
			id := intID(k)
			if id == 0 {
				return nil, invalid("Invalid spool ID")
			}
			_, e := saveRemoteSpool(r, id, sub(d["update"]))
			if e != nil {
				code := 500
				var p *problem
				if errors.As(e, &p) {
					code = p.code
				}
				failures = append(failures, row{"id": id, "status": code, "detail": e.Error()})
			} else {
				out = append(out, id)
			}
		}
		return row{"updated": len(out), "errors": failures}, nil
	})
	for _, child := range []struct{ path, table, model string }{{"k-profiles", "spoolman_k_profile", "SpoolKProfileBase"}, {"filament-presets", "spoolman_filament_preset", "SpoolFilamentPresetBase"}, {"suppliers", "spoolman_spool_suppliers", "SpoolSupplierLinkInput"}} {
		read := func(r request) ([]row, error) {
			q := "SELECT * FROM " + child.table + " WHERE spoolman_spool_id=? ORDER BY id"
			if child.path == "suppliers" {
				q = "SELECT l.*,s.name supplier_name FROM spoolman_spool_suppliers l JOIN suppliers s ON s.id=l.supplier_id WHERE spoolman_spool_id=? ORDER BY l.id"
			}
			v, e := list(r.tx, q, r.id())
			if child.path == "k-profiles" {
				v = visibleUsage(r, v)
			}
			return v, e
		}
		s.route(m, "GET", base+"/{id}/"+child.path, inventoryRead, func(r request) (any, error) { return read(r) })
		s.route(m, "PUT", base+"/{id}/"+child.path, inventoryWrite, func(r request) (any, error) {
			var items []row
			if e := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20)).Decode(&items); e != nil {
				return nil, invalid("Expected an array")
			}
			if _, e := rawSpool(r, r.id()); e != nil {
				return nil, e
			}
			if child.path == "k-profiles" {
				old, e := list(r.tx, "SELECT printer_id FROM spoolman_k_profile WHERE spoolman_spool_id=?", r.id())
				if e != nil {
					return nil, e
				}
				if len(visibleUsage(r, old)) != len(old) {
					return nil, denied()
				}
			}
			if _, e := r.tx.Exec("DELETE FROM "+child.table+" WHERE spoolman_spool_id=?", r.id()); e != nil {
				return nil, e
			}
			for _, v := range items {
				d, e := input(child.model, v)
				if e != nil {
					return nil, e
				}
				if pid := intID(d["printer_id"]); pid > 0 && !access.From(r.Context()).AllowsPrinter(pid) {
					return nil, sql.ErrNoRows
				}
				d["spoolman_spool_id"] = r.id()
				if _, e = insert(r.tx, child.table, d); e != nil {
					return nil, e
				}
			}
			return read(r)
		})
	}
}
