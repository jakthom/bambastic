package management

import (
	"database/sql"
	"net/http"
	"sort"
	"strings"

	"bambastic/internal/access"
)

type profileChild struct{ list, model, defaults, local, fields string }

var profileChildren = []profileChild{{"presets", "SpoolFilamentPresetBase", "filament_profile_default_preset", "spool_filament_preset", "printer_model nozzle_diameter slicer_filament slicer_filament_name"}, {"k_profiles", "SpoolKProfileBase", "filament_profile_default_k_profile", "spool_k_profile", "printer_id extruder nozzle_diameter nozzle_type k_value name cali_idx setting_id"}}

func productKey(v row, allColors bool) string {
	parts := []string{}
	for _, k := range []string{"brand", "material", "subtype", "color_name"} {
		x := strings.ToLower(strings.TrimSpace(str(v[k])))
		if allColors && k == "color_name" {
			x = ""
		}
		parts = append(parts, x)
	}
	return strings.Join(parts, "\x1f")
}
func childIdentity(c profileChild, v row) string {
	key := []string{str(v["printer_model"]), str(v["nozzle_diameter"])}
	if c.list == "k_profiles" {
		key = []string{str(v["printer_id"]), str(v["extruder"]), str(v["nozzle_diameter"])}
	}
	return strings.Join(key, "\x1f")
}
func defaultDetail(r request, v row) (row, error) {
	for _, c := range profileChildren {
		children, e := list(r.tx, "SELECT * FROM "+c.defaults+" WHERE profile_default_id=? ORDER BY id", v["id"])
		if e != nil {
			return nil, e
		}
		out := []row{}
		for _, a := range children {
			if c.list == "k_profiles" && !access.From(r.Context()).AllowsPrinter(intID(a["printer_id"])) {
				continue
			}
			out = append(out, obj(a, c.fields))
		}
		v[c.list] = out
	}
	delete(v, "match_key")
	return v, nil
}
func resolveDefault(r request, v row) (row, error) {
	base, specific := productKey(v, true), productKey(v, false)
	matches, e := list(r.tx, "SELECT * FROM filament_profile_default WHERE match_key IN (?,?) ORDER BY (match_key=?) DESC,id", base, specific, base)
	if e != nil || len(matches) == 0 {
		return nil, e
	}
	out := row{"default_ids": []int64{}, "slicer_filament": nil, "slicer_filament_name": nil}
	children := map[string]map[string]row{"presets": {}, "k_profiles": {}}
	for _, m := range matches {
		d, e := defaultDetail(r, m)
		if e != nil {
			return nil, e
		}
		out["default_ids"] = append(out["default_ids"].([]int64), intID(d["id"]))
		if str(d["slicer_filament"]) != "" {
			out["slicer_filament"], out["slicer_filament_name"] = d["slicer_filament"], d["slicer_filament_name"]
		}
		for _, c := range profileChildren {
			for _, child := range d[c.list].([]row) {
				children[c.list][childIdentity(c, child)] = child
			}
		}
	}
	for _, c := range profileChildren {
		items := []row{}
		keys := []string{}
		for k := range children[c.list] {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, children[c.list][k])
		}
		out[c.list] = items
	}
	return out, nil
}
func applyDefaults(r request, v row) (bool, error) {
	d, e := resolveDefault(r, v)
	if e != nil || d == nil {
		return false, e
	}
	changed := false
	if str(v["slicer_filament"]) == "" && d["slicer_filament"] != nil {
		if e = update(r.tx, "spool", intID(v["id"]), row{"slicer_filament": d["slicer_filament"], "slicer_filament_name": d["slicer_filament_name"]}); e != nil {
			return false, e
		}
		changed = true
	}
	for _, c := range profileChildren {
		old, e := list(r.tx, "SELECT * FROM "+c.local+" WHERE spool_id=?", v["id"])
		if e != nil {
			return false, e
		}
		have := map[string]bool{}
		for _, x := range old {
			have[childIdentity(c, x)] = true
		}
		for _, x := range d[c.list].([]row) {
			if !have[childIdentity(c, x)] {
				x["spool_id"] = v["id"]
				if _, e = insert(r.tx, c.local, x); e != nil {
					return false, e
				}
				changed = true
			}
		}
	}
	return changed, nil
}
func (s *Service) profileDefaults(m *http.ServeMux) {
	const path = "inventory/profile-defaults"
	s.route(m, "GET", path, inventoryRead, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT * FROM filament_profile_default ORDER BY brand,material,subtype,color_name")
		if e != nil {
			return nil, e
		}
		for _, d := range v {
			if _, e = defaultDetail(r, d); e != nil {
				return nil, e
			}
		}
		return v, nil
	})
	s.route(m, "GET", path+"/resolve", inventoryRead, func(r request) (any, error) {
		q := r.URL.Query()
		return resolveDefault(r, row{"material": q.Get("material"), "subtype": q.Get("subtype"), "brand": q.Get("brand"), "color_name": q.Get("color_name")})
	})
	s.route(m, "PUT", path, inventoryWrite, func(r request) (any, error) {
		d, e := r.input("FilamentProfileDefaultWrite")
		if e != nil {
			return nil, e
		}
		master := obj(d, "brand material subtype color_name slicer_filament slicer_filament_name")
		master["match_key"] = productKey(d, false)
		old, e := one(r.tx, "SELECT id FROM filament_profile_default WHERE match_key=?", master["match_key"])
		var id int64
		if e == sql.ErrNoRows {
			id, e = insert(r.tx, "filament_profile_default", master)
		} else if e == nil {
			id = intID(old["id"])
			master["updated_at"] = now()
			e = update(r.tx, "filament_profile_default", id, master)
		}
		if e != nil {
			return nil, e
		}
		for _, c := range profileChildren {
			if c.list == "k_profiles" {
				old, e := list(r.tx, "SELECT printer_id FROM "+c.defaults+" WHERE profile_default_id=?", id)
				if e != nil {
					return nil, e
				}
				if len(visibleUsage(r, old)) != len(old) {
					return nil, denied()
				}
			}
			if _, e = r.tx.Exec("DELETE FROM "+c.defaults+" WHERE profile_default_id=?", id); e != nil {
				return nil, e
			}
			items, _ := d[c.list].([]any)
			for _, x := range items {
				child, e := input(c.model, sub(x))
				if e != nil {
					return nil, e
				}
				if c.list == "k_profiles" && !access.From(r.Context()).AllowsPrinter(intID(child["printer_id"])) {
					return nil, denied()
				}
				child["profile_default_id"] = id
				if _, e = insert(r.tx, c.defaults, child); e != nil {
					return nil, e
				}
			}
		}
		v, e := get(r.tx, "filament_profile_default", id)
		if e != nil {
			return nil, e
		}
		return defaultDetail(r, v)
	})
	s.route(m, "DELETE", path+"/{id}", inventoryWrite, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT printer_id FROM filament_profile_default_k_profile WHERE profile_default_id=?", r.id())
		if e != nil {
			return nil, e
		}
		if len(v) != len(visibleUsage(r, v)) {
			return nil, denied()
		}
		return row{"status": "deleted"}, affect(r.tx, "DELETE FROM filament_profile_default WHERE id=?", r.id())
	})
	s.route(m, "POST", path+"/{id}/apply", inventoryWrite, func(r request) (any, error) {
		d, e := get(r.tx, "filament_profile_default", r.id())
		if e != nil {
			return nil, e
		}
		v, e := list(r.tx, "SELECT * FROM spool WHERE archived_at IS NULL")
		if e != nil {
			return nil, e
		}
		n := 0
		all := str(d["color_name"]) == ""
		for _, x := range v {
			if productKey(x, all) == str(d["match_key"]) {
				updated, e := applyDefaults(r, x)
				if e != nil {
					return nil, e
				}
				if updated {
					n++
				}
			}
		}
		return row{"updated": n}, nil
	})
}
