package production

import (
	"bambastic/internal/access"
	"bambastic/internal/management"
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (s *Service) fleetMaterials(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		perm := "queue:create"
		if action == "warnings" {
			perm = "printers:read"
		}
		if e := permitted(r, perm); e != nil {
			return nil, e
		}
		rs, e := records(ctx, s.db, "SELECT * FROM printers WHERE is_active ORDER BY name,id")
		if e != nil {
			return nil, e
		}
		out := []row{}
		seen := map[string]bool{}
		model := modelName(r.URL.Query().Get("model"))
		if action == "materials" && model == "" {
			return nil, fail(422, "Choose a model")
		}
		for _, pr := range rs {
			k := num(pr["id"])
			if !p.AllowsPrinter(k) {
				continue
			}
			s.mu.RLock()
			d := s.devices[k]
			if d == nil || d.h == nil || !d.h.Online() {
				s.mu.RUnlock()
				continue
			}
			rp := row{}
			for f, x := range d.report {
				rp[f] = x
			}
			s.mu.RUnlock()
			if action == "warnings" {
				deny := rp["developer_mode"] != nil && !flag(rp["developer_mode"])
				for _, fault := range printerFaults(pr, rp) {
					deny = deny || fault["full_code"] == "0500050000010007"
				}
				if deny {
					out = append(out, row{"printer_id": k, "name": pr["name"]})
				}
				continue
			}
			if model != modelName(pr["model"]) || r.URL.Query().Get("location") != "" && r.URL.Query().Get("location") != val(pr["location"]) {
				continue
			}
			for _, slot := range liveTrays(rp) {
				unit, local := slotCoords(slot.address)
				if a, e := resolveSlot(pr, rp, unit, local); e == nil {
					t := a.current
					color := strings.TrimPrefix(val(t["tray_color"]), "#")
					if color == "" {
						color = "808080"
					}
					brand := val(t["tray_sub_brands"])
					var side any
					if dual(pr) && a.extruder >= 0 {
						side = a.extruder
					}
					id := jstr([]any{slot.material, rgb(color), strings.ToUpper(brand), side})
					if seen[id] {
						continue
					}
					seen[id] = true
					out = append(out, row{"type": slot.material, "color": "#" + color, "tray_info_idx": slot.preset, "tray_sub_brands": brand, "extruder_id": side})
				}
			}
		}
		return out, nil
	}
}
func slotCoords(address int64) (int64, int64) {
	switch {
	case address >= 254:
		return 255, address - 254
	case address >= 128:
		return address, 0
	case address >= 28:
		return 32, address - 28
	case address >= 24:
		return 16, address - 24
	default:
		return address / 4, address % 4
	}
}
func stockAddress(unit, local int64) int64 {
	switch {
	case unit == 255:
		return 254 + local
	case unit >= 128:
		return unit
	case unit == 32 || unit == 7:
		return 28 + local
	case unit == 16 || unit == 6:
		return 24 + local
	default:
		return unit*4 + local
	}
}
func withStock(ctx context.Context, tx *sql.Tx, p access.Principal, pr, rp row) (map[int64]row, error) {
	stock, e := management.RemainingStock(ctx, tx, num(pr["id"]), p)
	if e != nil {
		return nil, e
	}
	out := map[int64]row{}
	for c, g := range stock {
		global := stockAddress(c[0], c[1])
		a, e := resolveSlot(pr, rp, c[0], c[1])
		if e != nil {
			continue
		}
		t := a.current
		key := ""
		if typ := val(t["tray_type"]); typ != "" {
			key = jstr([]any{strings.ToUpper(typ), strings.ToUpper(val(t["tray_color"])), strings.ToUpper(val(t["tray_sub_brands"])), val(t["tray_info_idx"])})
		}
		out[global] = row{"ams_id": c[0], "tray_id": c[1], "global_tray_id": global, "material_key": key, "remaining_g": g, "extruder": a.extruder, "spool": nil, "cost_per_kg": nil}
	}
	return out, nil
}
func (s *Service) stockView(w http.ResponseWriter, r *http.Request) (any, error) {
	pr, e := s.printerRow(r, "printers:read")
	if e != nil {
		return nil, e
	}
	s.mu.RLock()
	rp := row{}
	if d := s.devices[pathID(r)]; d != nil {
		for k, v := range d.report {
			rp[k] = v
		}
	}
	s.mu.RUnlock()
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	stock, e := withStock(r.Context(), tx, access.From(r.Context()), pr, rp)
	if e != nil {
		return nil, e
	}
	remain := map[string]any{}
	items := []row{}
	for k, v := range stock {
		if val(v["material_key"]) != "" {
			remain[strconv.FormatInt(k, 10)] = v["remaining_g"]
		}
		items = append(items, v)
	}
	sort.Slice(items, func(i, j int) bool { return num(items[i]["global_tray_id"]) < num(items[j]["global_tray_id"]) })
	return row{"inventory_remain_g": remain, "slot_materials": items}, nil
}
func deficit(stock map[int64]row, pl plate, mapping []int64, backup bool) string {
	used := map[string]float64{}
	available := map[string]float64{}
	pool := map[int64]string{}
	for k, v := range stock {
		key := fmt.Sprintf("slot-%d", k)
		if backup && val(v["material_key"]) != "" && num(v["extruder"]) >= 0 {
			key = val(v["material_key"]) + "-" + val(v["extruder"])
		}
		pool[k] = key
		g, _ := decimal(v["remaining_g"])
		available[key] += g
	}
	for _, fil := range pl.Filaments {
		slot := num(fil["id"]) - 1
		if slot < 0 || slot >= int64(len(mapping)) {
			continue
		}
		group, ok := pool[mapping[slot]]
		if !ok {
			continue
		}
		g, _ := decimal(fil["used_g"])
		used[group] += g
		if used[group] > available[group] {
			return fmt.Sprintf("Insufficient %s filament: %.1fg required, %.1fg remaining", val(fil["type"]), used[group], available[group])
		}
	}
	return ""
}
