package production

import (
	"fmt"
	"sort"
	"strings"
)

type traySlot struct {
	address                 int64
	material, color, preset string
}

func rgb(v any) string {
	c := strings.TrimPrefix(strings.ToUpper(val(v)), "#")
	if len(c) > 6 {
		c = c[:6]
	}
	return c
}
func liveTrays(r row) []traySlot {
	out := []traySlot{}
	add := func(t map[string]any, n int64) {
		if material := strings.ToUpper(val(t["tray_type"])); material != "" {
			out = append(out, traySlot{n, material, rgb(t["tray_color"]), val(t["tray_info_idx"])})
		}
	}
	if ams, ok := r["ams"].(map[string]any); ok {
		if units, ok := ams["ams"].([]any); ok {
			for _, u := range units {
				unit, _ := u.(map[string]any)
				amsID := num(unit["id"])
				ts, _ := unit["tray"].([]any)
				for _, v := range ts {
					t, _ := v.(map[string]any)
					n := 4*amsID + num(t["id"])
					if amsID == 16 {
						n = 24 + num(t["id"])
					}
					if amsID == 32 {
						n = 28 + num(t["id"])
					}
					if amsID >= 128 {
						n = amsID
					}
					add(t, n)
				}
			}
		}
	}
	for i, raw := range holders(r) {
		if t, ok := raw.(map[string]any); ok {
			n := int64(254 + i)
			if id := num(t["id"]); id >= 254 && id <= 255 {
				n = id
			}
			add(t, n)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].address < out[j].address })
	return out
}
func plateRequirements(pl plate, force bool) ([]string, []row) {
	unique := map[string]bool{}
	out := []row{}
	for _, f := range pl.Filaments {
		typ := strings.ToUpper(val(f["type"]))
		if typ == "" {
			continue
		}
		unique[typ] = true
		if force && val(f["color"]) != "" {
			out = append(out, row{"slot_id": num(f["id"]), "type": typ, "color": f["color"], "tray_info_idx": f["tray_info_idx"], "force_color_match": true})
		}
	}
	types := []string{}
	for t := range unique {
		types = append(types, t)
	}
	sort.Strings(types)
	return types, out
}
func matchFilaments(v, r row, pl plate) ([]int64, string) {
	trays := liveTrays(r)
	if types, ok := unpack(v["required_filament_types"]).([]any); ok {
		for _, typ := range types {
			found := false
			for _, t := range trays {
				if strings.EqualFold(val(typ), t.material) {
					found = true
					break
				}
			}
			if !found {
				return nil, "Waiting for " + val(typ) + " filament"
			}
		}
	}
	overrides := map[int64]map[string]any{}
	if list, ok := unpack(v["filament_overrides"]).([]any); ok {
		for _, x := range list {
			m, _ := x.(map[string]any)
			overrides[num(m["slot_id"])] = m
		}
	}
	explicit, _ := unpack(v["ams_mapping"]).([]any)
	mapping := []int64{}
	for _, fil := range pl.Filaments {
		slot := num(fil["id"])
		if slot < 1 || slot > 64 {
			continue
		}
		for len(mapping) < int(slot) {
			mapping = append(mapping, -1)
		}
		typ := strings.ToUpper(val(fil["type"]))
		color := rgb(fil["color"])
		preset := val(fil["tray_info_idx"])
		force := false
		if m := overrides[slot]; m != nil {
			if val(m["type"]) != "" {
				typ = strings.ToUpper(val(m["type"]))
			}
			if val(m["color"]) != "" {
				color = rgb(m["color"])
			}
			if val(m["tray_info_idx"]) != "" {
				preset = val(m["tray_info_idx"])
			}
			force = flag(m["force_color_match"])
		}
		if typ == "" {
			continue
		}
		preferred := int64(-1)
		if len(explicit) >= int(slot) {
			preferred = num(explicit[slot-1])
		}
		chosen := -1
		best := -1
		for i, t := range trays {
			if t.material != typ || preferred >= 0 && !force && t.address != preferred || force && (color != "" && color != t.color || preset != "" && preset != t.preset) {
				continue
			}
			score := 0
			if color != "" && color == t.color {
				score += 2
			}
			if preset != "" && preset == t.preset {
				score++
			}
			if score > best {
				best = score
				chosen = i
			}
		}
		if chosen < 0 {
			return nil, fmt.Sprintf("Waiting for matching %s filament in slot %d", typ, slot)
		}
		mapping[slot-1] = trays[chosen].address
	}
	if len(explicit) > len(mapping) {
		for _, x := range explicit[len(mapping):] {
			mapping = append(mapping, num(x))
		}
	}
	return mapping, ""
}
