package management

import (
	"bambastic/internal/lanhttp"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

func remoteEnabled(ctx context.Context, tx *sql.Tx) bool {
	return strings.EqualFold(setting(ctx, tx, "spoolman_enabled", "false"), "true")
}
func remoteBase(r request) (string, error) {
	if r.remoteEndpoint != "" {
		return r.remoteEndpoint, nil
	}
	if !remoteEnabled(r.Context(), r.tx) {
		return "", bad("Spoolman is disabled")
	}
	if setting(r.Context(), r.tx, "bambastic_spoolman_disconnected", "") == "true" {
		return "", bad("Spoolman is disconnected")
	}
	base := setting(r.Context(), r.tx, "spoolman_url", "")
	if base == "" {
		return "", &problem{503, "Spoolman URL is not configured"}
	}
	return strings.TrimRight(base, "/"), nil
}

func remoteCall(ctx context.Context, base, method, path string, body any, out any) error {
	u, e := url.Parse(base)
	if e != nil || u.Hostname() == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return bad("Invalid Spoolman URL")
	}
	var reader io.Reader
	if body != nil {
		b, e := json.Marshal(body)
		if e != nil {
			return e
		}
		reader = bytes.NewReader(b)
	}
	req, e := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+"/api/v1/"+path, reader)
	if e != nil {
		return e
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, e := lanhttp.Client.Do(req)
	if e != nil {
		return &problem{503, "Spoolman is unreachable"}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 404:
		return notfound()
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		return &problem{resp.StatusCode, "Spoolman rejected the request"}
	case resp.StatusCode < 200 || resp.StatusCode >= 300:
		return &problem{502, "Spoolman returned an unsuccessful response"}
	}
	if out != nil && resp.StatusCode != 204 {
		if e = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); e != nil {
			return &problem{502, "Spoolman returned invalid JSON"}
		}
	}
	return nil
}
func remote(r request, method, path string, body, out any) error {
	base, e := remoteBase(r)
	if e != nil {
		return e
	}
	return remoteCall(r.Context(), base, method, path, body, out)
}
func sub(v any) row {
	switch v := v.(type) {
	case row:
		return v
	case map[string]any:
		return v
	}
	return row{}
}
func extraText(extra row, k string) string {
	v := str(extra[k])
	var raw string
	if json.Unmarshal([]byte(v), &raw) == nil {
		return raw
	}
	return v
}
func remoteWeights(v row) (netWeight, core float64, inherited bool) {
	f := sub(v["filament"])
	vendor := sub(f["vendor"])
	netWeight = num(v["initial_weight"])
	if netWeight <= 0 {
		netWeight = 1000
		if f["weight"] != nil {
			netWeight = num(f["weight"])
		}
	}
	for _, x := range []any{v["spool_weight"], f["spool_weight"], vendor["empty_spool_weight"]} {
		if x != nil {
			return netWeight, num(x), false
		}
	}
	return netWeight, 250, true
}
func remoteSpool(v row) row {
	f := sub(v["filament"])
	vendor := sub(f["vendor"])
	extra := sub(v["extra"])
	net, tare, _ := remoteWeights(v)
	realUsed := math.Max(0, num(v["used_weight"]))
	used := realUsed
	if v["remaining_weight"] != nil {
		used = math.Max(0, net-num(v["remaining_weight"]))
	}
	base := math.Max(0, num(extraText(extra, "bambu_weight_used_baseline")))
	base = math.Min(used, math.Max(0, used-realUsed+base))
	rgba := strings.ToUpper(strings.TrimPrefix(str(f["color_hex"]), "#"))
	if (len(rgba) != 6 && len(rgba) != 8) || strings.Trim(rgba, "0123456789ABCDEF") != "" {
		rgba = "808080"
	}
	if len(rgba) == 6 {
		rgba += "FF"
	}
	var tag, tray any
	apply := func(v string) {
		v = strings.ToUpper(strings.TrimSpace(v))
		if v == "" || strings.Trim(v, "0123456789ABCDEF") != "" {
			return
		}
		if len(v) == 32 && tray == nil {
			tray = v
		} else if len(v) >= 8 && len(v) <= 30 && tag == nil {
			tag = v
		}
	}
	apply(extraText(extra, "tag"))
	if tags, ok := v["tags"].([]any); ok {
		for _, a := range tags {
			apply(str(sub(a)["uid"]))
		}
	}
	material := strings.TrimSpace(str(f["material"]))
	subtype, nameColor := splitProductName(material, str(f["name"]))
	out := row{"id": v["id"], "material": material, "subtype": nil, "brand": vendor["name"], "color_name": f["color_name"], "rgba": rgba, "label_weight": net, "core_weight": tare, "core_weight_is_inherited": v["spool_weight"] == nil, "weight_used": used, "weight_used_baseline": base, "weight_locked": false, "storage_location": v["location"], "note": v["comment"], "created_at": v["registered"], "updated_at": v["last_used"], "last_used": v["last_used"], "first_used": v["first_used"], "archived_at": nil, "material_number": f["article_number"], "tag_uid": tag, "tray_uuid": tray, "k_profiles": []row{}, "suppliers": []row{}, "cost_per_kg": nil, "effective_cost_per_kg": nil, "slicer_filament_name": f["name"]}
	for _, k := range []string{"color_name", "slicer_filament", "slicer_filament_name", "last_dried_at"} {
		if val := extraText(extra, "bambu_"+k); val != "" {
			out[k] = val
		}
	}
	if subtype != "" {
		out["subtype"] = subtype
	}
	if str(out["color_name"]) == "" && nameColor != "" {
		out["color_name"] = nameColor
	}
	out["color_name_is_synthesized"] = str(out["color_name"]) == "" && subtype != ""
	if truth(out["color_name_is_synthesized"]) {
		out["color_name"] = subtype
	}
	if truth(v["archived"]) {
		out["archived_at"] = v["last_used"]
		if str(out["archived_at"]) == "" {
			out["archived_at"] = v["registered"]
		}
		if out["archived_at"] == nil {
			out["archived_at"] = "1970-01-01T00:00:00Z"
		}
	}
	if net > 0 && v["price"] != nil {
		out["cost_per_kg"] = num(v["price"]) * 1000 / net
	}
	if rate := spoolPerGram(v); rate > 0 {
		out["effective_cost_per_kg"] = rate * 1000
	}
	return out
}
func rawSpool(r request, id int64) (row, error) {
	if id < 1 {
		return nil, notfound()
	}
	var v row
	e := remote(r, "GET", fmt.Sprintf("spool/%d", id), nil, &v)
	if e == nil && intID(v["id"]) != id {
		return nil, &problem{502, "Spoolman returned an unexpected spool"}
	}
	return v, e
}
func weighRemote(r request, id int64, gross float64) (row, error) {
	if gross < 0 {
		return nil, invalid("Weight must be nonnegative")
	}
	v, e := rawSpool(r, id)
	if e != nil {
		return nil, e
	}
	net, tare, fallback := remoteWeights(v)
	rem := math.Max(0, gross-tare)
	if e = remote(r, "PATCH", fmt.Sprintf("spool/%d", id), row{"remaining_weight": rem}, nil); e != nil {
		return nil, e
	}
	reply := row{"status": "ok", "weight_used": math.Max(0, net-rem)}
	if fallback {
		reply["warnings"] = []string{"spool_weight_not_set: used 250 g fallback tare"}
	}
	r.event(row{"type": "inventory_changed"})
	return reply, nil
}
func remoteTag(r request, tray, tag string) (row, error) {
	var items []row
	if e := remote(r, "GET", "spool", nil, &items); e != nil {
		return nil, e
	}
	for _, raw := range items {
		v := remoteSpool(raw)
		if v["archived_at"] != nil {
			continue
		}
		if (tray != "" && strings.EqualFold(str(v["tray_uuid"]), tray)) || (tag != "" && strings.EqualFold(str(v["tag_uid"]), tag)) {
			return v, nil
		}
	}
	return nil, notfound()
}
func (s *Service) spoolman(m *http.ServeMux) {
	const prefix = "spoolman/inventory"
	s.route(m, "GET", "spoolman/status", "filaments:read", func(r request) (any, error) {
		base, e := remoteBase(r)
		connected := e == nil && remoteCall(r.Context(), base, "GET", "health", nil, nil) == nil
		native := connected && remoteCall(r.Context(), base, "GET", "tag/reader", nil, nil) == nil
		return row{"enabled": remoteEnabled(r.Context(), r.tx), "url": setting(r.Context(), r.tx, "spoolman_url", ""), "connected": connected, "native_tags": native}, nil
	})
	for _, resource := range []string{"spool", "filament"} {
		s.route(m, "GET", "spoolman/"+resource+"s", "filaments:read", func(r request) (any, error) {
			var items []row
			e := remote(r, "GET", resource, nil, &items)
			return row{resource + "s": items}, e
		})
	}
	s.route(m, "GET", prefix+"/filaments", inventoryRead, func(r request) (any, error) {
		var items []row
		e := remote(r, "GET", "filament", nil, &items)
		return items, e
	})
	const path = prefix + "/spools"
	s.route(m, "GET", path, inventoryRead, func(r request) (any, error) {
		var items []row
		endpoint := "spool"
		all := r.URL.Query().Get("include_archived") == "true"
		if all {
			endpoint += "?allow_archived=true"
		}
		if e := remote(r, "GET", endpoint, nil, &items); e != nil {
			return nil, e
		}
		spools := []row{}
		for _, raw := range items {
			if all || !truth(raw["archived"]) {
				spools = append(spools, remoteSpool(raw))
			}
		}
		return spools, nil
	})
	s.route(m, "GET", path+"/{id}", inventoryRead, func(r request) (any, error) {
		v, e := rawSpool(r, r.id())
		if e != nil {
			return nil, e
		}
		return remoteSpool(v), nil
	})
	s.route(m, "PATCH", path+"/{id}/weight", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("SpoolWeightUpdate")
		if e != nil {
			return nil, e
		}
		return weighRemote(r, r.id(), num(d["weight_grams"]))
	})
	for _, op := range []string{"archive", "restore", "reset-consumed-counter", "delete"} {
		method, pathName := "POST", path+"/{id}/"+op
		if op == "delete" {
			method, pathName = "DELETE", path+"/{id}"
		}
		s.route(m, method, pathName, inventoryWrite, func(r request) (any, error) {
			v, e := remoteAction(r, r.id(), op)
			if e == nil {
				r.event(row{"type": "inventory_changed"})
			}
			return v, e
		})
	}
	s.route(m, "GET", path+"/{id}/usage", inventoryRead, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT * FROM spool_usage_history WHERE spoolman_spool_id=? ORDER BY created_at DESC,id DESC", r.id())
		return visibleUsage(r, v), e
	})
	s.route(m, "DELETE", path+"/{id}/usage", inventoryWrite, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT id,printer_id FROM spool_usage_history WHERE spoolman_spool_id=?", r.id())
		if e != nil {
			return nil, e
		}
		visible := visibleUsage(r, v)
		for _, u := range visible {
			if _, e = r.tx.Exec("DELETE FROM spool_usage_history WHERE id=?", u["id"]); e != nil {
				return nil, e
			}
		}
		return row{"deleted": len(visible)}, nil
	})
	s.remoteEdit(m)
	s.remoteSlots(m)
}
func visibleUsage(r request, v []row) []row {
	visible := []row{}
	for _, u := range v {
		if access.From(r.Context()).AllowsPrinter(intID(u["printer_id"])) {
			visible = append(visible, u)
		}
	}
	return visible
}
func (s *Service) remoteSlots(m *http.ServeMux) {
	const slot = "spoolman/inventory/slot-assignments"
	s.route(m, "GET", slot+"/all", inventoryRead, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT s.*,p.name printer_name,NULL ams_label FROM spoolman_slot_assignments s JOIN printers p ON p.id=s.printer_id WHERE (?='' OR p.id=?) ORDER BY printer_id,ams_id,tray_id", r.URL.Query().Get("printer_id"), r.URL.Query().Get("printer_id"))
		return visibleUsage(r, v), e
	})
	s.route(m, "GET", slot, inventoryRead, func(r request) (any, error) {
		q := r.URL.Query()
		p, _ := strconv.ParseInt(q.Get("printer_id"), 10, 64)
		if !access.From(r.Context()).AllowsPrinter(p) {
			return nil, notfound()
		}
		v, e := one(r.tx, "SELECT * FROM spoolman_slot_assignments WHERE printer_id=? AND ams_id=? AND tray_id=?", p, q.Get("ams_id"), q.Get("tray_id"))
		if errors.Is(e, sql.ErrNoRows) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		raw, e := rawSpool(r, intID(v["spoolman_spool_id"]))
		if e != nil {
			return nil, e
		}
		return remoteSpool(raw), nil
	})
	s.route(m, "POST", slot, inventoryWrite, func(r request) (any, error) {
		d, e := r.input("SpoolSlotAssignmentRequest")
		if e != nil {
			return nil, e
		}
		a := access.From(r.Context())
		p, id := intID(d["printer_id"]), intID(d["spoolman_spool_id"])
		if !a.AllowsPrinter(p) {
			return nil, notfound()
		}
		if _, e = get(r.tx, "printers", p); e != nil {
			return nil, e
		}
		v, e := rawSpool(r, id)
		if e != nil {
			return nil, e
		}
		if truth(v["archived"]) {
			return nil, bad("Cannot assign an archived spool")
		}
		moving, e := list(r.tx, "SELECT * FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", id)
		if e != nil {
			return nil, e
		}
		for _, old := range moving {
			if !a.AllowsPrinter(intID(old["printer_id"])) {
				return nil, denied()
			}
			if (p != intID(old["printer_id"]) || num(old["ams_id"]) != num(d["ams_id"]) || num(old["tray_id"]) != num(d["tray_id"])) && !truth(d["move"]) {
				return nil, &problem{409, "Spool is assigned elsewhere"}
			}
		}
		if _, e = r.tx.Exec("DELETE FROM spoolman_slot_assignments WHERE spoolman_spool_id=? OR (printer_id=? AND ams_id=? AND tray_id=?)", id, p, d["ams_id"], d["tray_id"]); e != nil {
			return nil, e
		}
		delete(d, "move")
		if _, e = insert(r.tx, "spoolman_slot_assignments", d); e != nil {
			return nil, e
		}
		if e = noteAssignment(r, slotKey{p, int64(num(d["ams_id"])), int64(num(d["tray_id"]))}, id, true); e != nil {
			return nil, e
		}
		r.event(row{"type": "inventory_changed"})
		return row{"status": "ok", "configured": false, "pending_config": true}, nil
	})
	s.route(m, "DELETE", slot+"/{id}", inventoryWrite, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT printer_id FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", r.id())
		if e != nil {
			return nil, e
		}
		for _, a := range v {
			if !access.From(r.Context()).AllowsPrinter(intID(a["printer_id"])) {
				return nil, notfound()
			}
		}
		return row{"status": "ok"}, affect(r.tx, "DELETE FROM spoolman_slot_assignments WHERE spoolman_spool_id=?", r.id())
	})
}

// SpoolmanRates reads one inventory snapshot, including archived purchases. A
// zero rate denotes a known spool whose price is unset. An absent ID denotes a
// spool that is no longer in the inventory.
func SpoolmanRates(ctx context.Context, base string) (map[int64]float64, error) {
	if err := remoteCall(ctx, base, "GET", "health", nil, nil); err != nil {
		return nil, err
	}
	var spools []row
	if err := remoteCall(ctx, base, "GET", "spool?allow_archived=true", nil, &spools); err != nil {
		return nil, err
	}
	rates := map[int64]float64{}
	for _, v := range spools {
		if k := intID(v["id"]); k > 0 {
			rates[k] = spoolPerGram(v)
		}
	}
	return rates, nil
}
func spoolPerGram(v row) float64 {
	f := sub(v["filament"])
	positive := func(v any) float64 {
		if _, ok := v.(bool); ok {
			return 0
		}
		n := num(v)
		if n > 0 && !math.IsNaN(n) && !math.IsInf(n, 0) {
			return n
		}
		return 0
	}
	price := positive(v["price"])
	weight := positive(f["weight"])
	if price > 0 {
		if w := positive(v["initial_weight"]); w > 0 {
			weight = w
		}
	} else {
		price = positive(f["price"])
	}
	rate := price / weight
	if rate <= 0 || math.IsNaN(rate) || math.IsInf(rate, 0) {
		return 0
	}
	return rate
}

// SpoolmanMaterialIdentity resolves the assigned roll against the currently selected
// remote inventory. It is used by idle device profile reconciliation.
func (s *Service) SpoolmanMaterialIdentity(ctx context.Context, id int64) (map[string]any, error) {
	enabled, e := access.Setting(ctx, s.db, "spoolman_enabled", "false")
	if e != nil {
		return nil, e
	}
	disconnected, e := access.Setting(ctx, s.db, "bambastic_spoolman_disconnected", "false")
	if e != nil {
		return nil, e
	}
	base, e := access.Setting(ctx, s.db, "spoolman_url", "")
	if e != nil {
		return nil, e
	}
	if id <= 0 || !strings.EqualFold(enabled, "true") || strings.EqualFold(disconnected, "true") || base == "" {
		return nil, bad("Spoolman is unavailable")
	}
	var raw row
	if e = remoteCall(ctx, base, "GET", fmt.Sprintf("spool/%d", id), nil, &raw); e != nil {
		return nil, e
	}
	if intID(raw["id"]) != id || truth(raw["archived"]) {
		return nil, notfound()
	}
	return remoteSpool(raw), nil
}
