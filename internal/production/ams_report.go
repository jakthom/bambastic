package production

import (
	"math/big"
	"strconv"
	"strings"
)

// A push may contain one tray, one unit, or only humidity. Presence bits are
// authoritative when the firmware supplies them; omitted rows alone are not.
func mergeAMS(prior, delta map[string]any) map[string]any {
	out := cloneWire(prior)
	if out == nil {
		out = map[string]any{}
	}
	for k, v := range delta {
		if k != "ams" {
			out[k] = v
		}
	}
	units, _ := out["ams"].([]any)
	fresh, listed := delta["ams"].([]any)
	exists, hasBits := mask(delta["ams_exist_bits"])
	if listed {
		units = overlayIDs(units, fresh, true)
		if hasBits {
			seen := map[int64]bool{}
			for _, v := range fresh {
				if u, ok := v.(map[string]any); ok && u["id"] != nil {
					seen[num(u["id"])] = true
				}
			}
			kept := units[:0]
			for _, v := range units {
				u, ok := v.(map[string]any)
				if !ok {
					continue
				}
				id := num(u["id"])
				if !seen[id] && (len(fresh) == 0 && exists.Sign() == 0 || id >= 0 && id <= 3 && exists.Bit(int(id)) == 0) {
					continue
				}
				kept = append(kept, v)
			}
			units = kept
		}
	}
	occupied, hasBits := mask(out["tray_exist_bits"])
	powerOff := out["power_on_flag"] != nil && !flag(out["power_on_flag"])
	if hasBits && !(powerOff && occupied.Sign() == 0) {
		for _, v := range units {
			u, ok := v.(map[string]any)
			if !ok {
				continue
			}
			trays, _ := u["tray"].([]any)
			for _, v := range trays {
				t, ok := v.(map[string]any)
				if !ok || t["id"] == nil || u["id"] == nil {
					continue
				}
				unit, tray := num(u["id"]), num(t["id"])
				bit := int64(-1)
				switch {
				case unit >= 0 && unit <= 3 && tray >= 0 && tray < 4:
					bit = unit*4 + tray
				case (unit == 6 || unit == 16) && tray >= 0 && tray < 4:
					bit = 24 + tray
				case unit == 32 && tray >= 0 && tray < 9:
					bit = 28 + tray
				case unit >= 128 && unit <= 135 && tray == 0:
					bit = 16 + unit - 128
				}
				if bit < 0 || occupied.Bit(int(bit)) == 1 {
					continue
				}
				t["state"] = 9
				t["remain"] = 0
				for _, key := range []string{"tray_type", "tray_sub_brands", "tray_color", "tray_id_name", "tray_info_idx"} {
					t[key] = ""
				}
				t["tag_uid"] = strings.Repeat("0", 16)
				t["tray_uuid"] = strings.Repeat("0", 32)
			}
		}
	}
	if units != nil || listed {
		out["ams"] = units
	}
	return out
}

func mask(v any) (*big.Int, bool) {
	base := 10
	text := val(v)
	if _, ok := v.(string); ok {
		base = 16
		text = strings.TrimPrefix(strings.ToLower(text), "0x")
	} else if n, ok := v.(float64); ok {
		if n < 0 || n > 1<<53 {
			return nil, false
		}
		text = strconv.FormatUint(uint64(n), 10)
	}
	if len(text) == 0 || len(text) > 32 {
		return nil, false
	}
	m, ok := new(big.Int).SetString(text, base)
	return m, ok && m.Sign() >= 0
}

func overlayIDs(existing, delta []any, units bool) []any {
	indices := map[string]int{}
	for i, v := range existing {
		if m, ok := v.(map[string]any); ok {
			indices[val(m["id"])] = i
		}
	}
	for _, v := range delta {
		u, ok := v.(map[string]any)
		if !ok || u["id"] == nil {
			continue
		}
		id := val(u["id"])
		i, found := indices[id]
		if !found {
			indices[id] = len(existing)
			existing = append(existing, cloneWire(u))
			continue
		}
		old := existing[i].(map[string]any)
		for key, value := range u {
			if key == "tray" && units {
				trays, ok := value.([]any)
				if !ok {
					continue
				}
				prev, _ := old["tray"].([]any)
				old["tray"] = overlayIDs(prev, trays, false)
			} else {
				old[key] = value
			}
		}
	}
	return existing
}
