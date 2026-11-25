package management

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

const inventoryRead = "inventory:read"
const inventoryWrite = "inventory:update"

func (s *Service) inventory(m *http.ServeMux) {
	const path = "inventory/spools"
	s.route(m, "GET", path, inventoryRead, func(r request) (any, error) {
		filter := "WHERE archived_at IS NULL"
		if r.URL.Query().Get("include_archived") == "true" {
			filter = ""
		}
		rows, e := list(r.tx, "SELECT * FROM spool "+filter+" ORDER BY material,brand,color_name,id")
		if e != nil {
			return nil, e
		}
		for _, v := range rows {
			if e = spoolLinks(r, v); e != nil {
				return nil, e
			}
		}
		return rows, nil
	})
	s.route(m, "POST", path, inventoryWrite, func(r request) (any, error) {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		return saveSpool(r, 0, d)
	})
	s.route(m, "GET", path+"/{id}", inventoryRead, func(r request) (any, error) { return readSpool(r, r.id()) })
	s.route(m, "PATCH", path+"/{id}", inventoryWrite, func(r request) (any, error) {
		d, e := r.body()
		if e != nil {
			return nil, e
		}
		return saveSpool(r, r.id(), d)
	})
	s.route(m, "DELETE", path+"/{id}", inventoryWrite, func(r request) (any, error) {
		return row{"status": "deleted"}, affect(r.tx, "DELETE FROM spool WHERE id=?", r.id())
	})
	s.route(m, "GET", path+"/by-tag", inventoryRead+" "+inventoryWrite, func(r request) (any, error) {
		return byTag(r, r.URL.Query().Get("tray_uuid"), r.URL.Query().Get("tag_uid"), r.URL.Query().Get("include_archived") == "true")
	})
	s.route(m, "PATCH", path+"/{id}/link-tag", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("LinkTagRequest")
		if e != nil {
			return nil, e
		}
		v, e := get(r.tx, "spool", r.id())
		if e != nil {
			return nil, e
		}
		if v["archived_at"] != nil {
			return nil, bad("Cannot link a tag to an archived spool")
		}
		for _, k := range []string{"tag_uid", "tray_uuid"} {
			if d[k] == nil {
				continue
			}
			length := 16
			if k == "tray_uuid" {
				length = 32
			}
			key := cleanTag(d[k], length)
			if str(d[k]) != "" && (key == nil || len(str(key))%2 != 0 || k == "tray_uuid" && len(str(key)) != 32) {
				return nil, invalid("Invalid " + k)
			}
			d[k] = key
			if key == nil {
				continue
			}
			holder, e := one(r.tx, "SELECT id FROM spool WHERE id!=? AND archived_at IS NULL AND upper("+k+")=? LIMIT 1", r.id(), key)
			if e == nil {
				return nil, &tagConflict{k, intID(holder["id"])}
			}
			if e != sql.ErrNoRows {
				return nil, e
			}
		}
		return saveSpool(r, r.id(), d)
	})
	for _, op := range []string{"archive", "restore", "reset-consumed-counter"} {
		s.route(m, "POST", path+"/{id}/"+op, inventoryWrite, func(r request) (any, error) {
			if e := spoolAction(r, r.id(), op); e != nil {
				return nil, e
			}
			return readSpool(r, r.id())
		})
	}
	s.route(m, "POST", path+"/bulk", inventoryWrite, func(r request) (any, error) {
		body, e := r.body()
		if e != nil {
			return nil, e
		}
		d, ok := body["spool"].(map[string]any)
		if !ok {
			return nil, invalid("spool is required")
		}
		count := int64(1)
		if body["quantity"] != nil {
			count = intID(body["quantity"])
		}
		if count < 1 || count > 100 {
			return nil, invalid("quantity must be between 1 and 100")
		}
		result := []row{}
		for i := int64(0); i < count; i++ {
			v, e := saveSpool(r, 0, d)
			if e != nil {
				return nil, e
			}
			result = append(result, v)
		}
		return result, nil
	})
	for _, op := range []string{"archive", "restore", "update", "delete", "reset-consumed-counter"} {
		suffix := "bulk-" + op
		if op == "reset-consumed-counter" {
			suffix = op + "-bulk"
		}
		s.route(m, "POST", path+"/"+suffix, inventoryWrite, func(r request) (any, error) { return bulkSpools(r, op) })
	}
	s.profileDefaults(m)
	s.referenceData(m)
	s.assignments(m)
	s.stock(m)
}

func cleanTag(v any, max int) any {
	val := strings.Map(func(c rune) rune {
		if (c >= '0' && c <= '9') || (c >= 'A' && c <= 'F') {
			return c
		}
		return -1
	}, strings.ToUpper(str(v)))
	if len(val) > max {
		if max == 16 {
			val = val[len(val)-max:]
		} else {
			val = val[:max]
		}
	}
	if strings.Trim(val, "0") == "" {
		return nil
	}
	return val
}
func byTag(r request, tray, tag string, archived bool) (row, error) {
	filter := " AND archived_at IS NULL"
	if archived {
		filter = ""
	}
	keys := []struct {
		key string
		val any
	}{{"tray_uuid", cleanTag(tray, 32)}, {"tag_uid", cleanTag(tag, 16)}}
	supplied := false
	for _, k := range keys {
		if k.val == nil {
			continue
		}
		supplied = true
		v, e := one(r.tx, "SELECT * FROM spool WHERE upper("+k.key+")=?"+filter+" ORDER BY id LIMIT 1", k.val)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return nil, e
		}
		return v, spoolLinks(r, v)
	}
	if !supplied {
		return nil, bad("Provide tray_uuid or tag_uid")
	}
	return nil, notfound()
}
func readSpool(r request, id int64) (row, error) {
	v, e := get(r.tx, "spool", id)
	if e != nil {
		return nil, e
	}
	return v, spoolLinks(r, v)
}
func spoolLinks(r request, v row) error {
	profiles, e := list(r.tx, "SELECT * FROM spool_k_profile WHERE spool_id=? ORDER BY id", v["id"])
	if e != nil {
		return e
	}
	actor := access.From(r.Context())
	visible := []row{}
	for _, p := range profiles {
		if actor.AllowsPrinter(intID(p["printer_id"])) {
			visible = append(visible, p)
		}
	}
	v["k_profiles"] = visible
	suppliers, e := list(r.tx, "SELECT l.*,s.name AS supplier_name FROM spool_suppliers l JOIN suppliers s ON s.id=l.supplier_id WHERE spool_id=? ORDER BY l.id", v["id"])
	v["suppliers"] = suppliers
	return e
}
func saveSpool(r request, id int64, d row) (row, error) {
	schema := "SpoolCreate"
	if id != 0 {
		schema = "SpoolUpdate"
	}
	v, e := input(schema, d)
	if e != nil {
		return nil, e
	}
	if x, ok := v["tag_uid"]; ok {
		v["tag_uid"] = cleanTag(x, 16)
	}
	if x, ok := v["tray_uuid"]; ok {
		v["tray_uuid"] = cleanTag(x, 32)
	}
	for _, k := range []string{"weight_used", "core_weight", "label_weight", "cost_per_kg"} {
		if x, ok := v[k]; ok && num(x) < 0 {
			return nil, invalid(k + " cannot be negative")
		}
	}
	if weight, ok := d["weight_used"]; ok && weight != nil {
		if _, explicit := d["weight_locked"]; !explicit && (id != 0 || num(weight) > 0) {
			v["weight_locked"] = true
		}
	}
	if k, ok := v["location_id"]; ok && k != nil {
		loc, e := get(r.tx, "locations", intID(k))
		if e != nil {
			return nil, e
		}
		v["storage_location"] = loc["name"]
	} else if name, ok := v["storage_location"]; ok {
		name = strings.TrimSpace(str(name))
		if name != "" {
			_, e := r.tx.Exec("INSERT INTO locations(name,name_key) VALUES(?,?) ON CONFLICT(name_key) DO NOTHING", name, strings.ToLower(str(name)))
			if e != nil {
				return nil, e
			}
			loc, e := one(r.tx, "SELECT name,id FROM locations WHERE name_key=?", strings.ToLower(str(name)))
			if e != nil {
				return nil, e
			}
			v["storage_location"], v["location_id"] = loc["name"], loc["id"]
		} else {
			v["storage_location"], v["location_id"] = nil, nil
		}
	} else if _, ok := d["location_id"]; ok {
		v["storage_location"] = nil
	}
	// Optional "extra_colors" is a comma-separated storage field.
	if id == 0 {
		if str(v["material_number"]) == "" {
			var number string
			err := r.tx.QueryRow("SELECT material_number FROM spool WHERE material=? AND subtype IS ? AND brand IS ? AND color_name IS ? AND coalesce(material_number,'')!='' ORDER BY julianday(updated_at) DESC,id DESC LIMIT 1", v["material"], v["subtype"], v["brand"], v["color_name"]).Scan(&number)
			if err == nil {
				v["material_number"] = number
			} else if err != sql.ErrNoRows {
				return nil, err
			}
		}
		v["added_full"] = num(v["weight_used"]) == 0
		id, e = insert(r.tx, "spool", v)
		if e == nil {
			v["id"] = id
			_, e = applyDefaults(r, v)
		}
	} else {
		v["updated_at"] = now()
		e = update(r.tx, "spool", id, v)
	}
	if e != nil {
		return nil, e
	}
	return readSpool(r, id)
}
func spoolAction(r request, id int64, op string) error {
	switch op {
	case "archive":
		if _, e := r.tx.Exec("DELETE FROM spool_assignment WHERE spool_id=?", id); e != nil {
			return e
		}
		return affect(r.tx, "UPDATE spool SET archived_at=COALESCE(archived_at,CURRENT_TIMESTAMP),updated_at=CURRENT_TIMESTAMP WHERE id=?", id)
	case "restore":
		return affect(r.tx, "UPDATE spool SET archived_at=NULL,updated_at=CURRENT_TIMESTAMP WHERE id=?", id)
	case "reset-consumed-counter":
		return affect(r.tx, "UPDATE spool SET weight_used_baseline=weight_used,updated_at=CURRENT_TIMESTAMP WHERE id=?", id)
	case "delete":
		return affect(r.tx, "DELETE FROM spool WHERE id=?", id)
	}
	return bad("Unknown action")
}
func bulkSpools(r request, op string) (any, error) {
	d, e := r.body()
	if e != nil {
		return nil, e
	}
	ids, ok := d["ids"].([]any)
	if !ok || len(ids) == 0 || len(ids) > 10000 {
		return nil, invalid("Provide spool IDs")
	}
	key := map[string]string{"archive": "archived", "restore": "restored", "update": "updated", "delete": "deleted", "reset-consumed-counter": "reset"}[op]
	missing, already := []int64{}, []int64{}
	changed := 0
	seen := map[int64]bool{}
	for _, n := range ids {
		id := intID(n)
		if id == 0 {
			return nil, invalid("Invalid spool ID")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		spool, e := get(r.tx, "spool", id)
		if e == sql.ErrNoRows {
			missing = append(missing, id)
			continue
		}
		if e != nil {
			return nil, e
		}
		if op == "restore" && spool["archived_at"] == nil || op == "archive" && spool["archived_at"] != nil {
			already = append(already, id)
			continue
		}
		if op == "update" {
			fields, ok := d["update"].(map[string]any)
			if !ok {
				return nil, invalid("update is required")
			}
			_, e = saveSpool(r, id, fields)
		} else {
			e = spoolAction(r, id, op)
		}
		if e != nil {
			return nil, e
		}
		changed++
	}
	out := row{key: changed, "not_found": missing}
	if op == "restore" {
		out["already_active"] = already
	}
	if op == "archive" {
		out["already_archived"] = already
	}
	return out, nil
}
func (s *Service) assignments(m *http.ServeMux) {
	const path = "inventory/assignments"
	s.route(m, "GET", path, inventoryRead+" inventory:view_assignments", func(r request) (any, error) {
		items, e := list(r.tx, "SELECT a.*,p.name printer_name,p.model printer_model,c.phase config_phase FROM spool_assignment a JOIN printers p ON p.id=a.printer_id LEFT JOIN bb_slot_config c ON c.assignment_id=a.id ORDER BY a.printer_id,a.ams_id,a.tray_id")
		if e != nil {
			return nil, e
		}
		visible := []row{}
		for _, v := range items {
			if !access.From(r.Context()).AllowsPrinter(intID(v["printer_id"])) {
				continue
			}
			if phase := str(v["config_phase"]); phase != "" {
				v["configured"] = phase == "sent" || phase == "confirmed"
				v["pending_config"] = phase == "pending"
			}
			delete(v, "config_phase")
			v["spool"], e = readSpool(r, intID(v["spool_id"]))
			if e != nil {
				return nil, e
			}
			visible = append(visible, v)
		}
		return visible, nil
	})
	s.route(m, "POST", path, inventoryWrite, func(r request) (any, error) {
		d, e := r.input("SpoolAssignmentCreate")
		if e != nil {
			return nil, e
		}
		p := intID(d["printer_id"])
		if !access.From(r.Context()).AllowsPrinter(p) {
			return nil, notfound()
		}
		spool, e := get(r.tx, "spool", intID(d["spool_id"]))
		if e != nil {
			return nil, e
		}
		if spool["archived_at"] != nil {
			return nil, bad("Cannot assign an archived spool")
		}
		previous, e := list(r.tx, "SELECT * FROM spool_assignment WHERE spool_id=?", d["spool_id"])
		if e != nil {
			return nil, e
		}
		for _, old := range previous {
			if !access.From(r.Context()).AllowsPrinter(intID(old["printer_id"])) {
				return nil, denied()
			}
			same := p == intID(old["printer_id"]) && num(d["ams_id"]) == num(old["ams_id"]) && num(d["tray_id"]) == num(old["tray_id"])
			if !same && !truth(d["move"]) {
				return nil, &problem{409, "Spool is assigned elsewhere"}
			}
		}
		if _, e = r.tx.Exec("DELETE FROM spool_assignment WHERE spool_id=? OR (printer_id=? AND ams_id=? AND tray_id=?)", d["spool_id"], p, d["ams_id"], d["tray_id"]); e != nil {
			return nil, e
		}
		delete(d, "move")
		d["assigned_by_user_id"] = nullID(access.From(r.Context()).ID)
		id, e := insert(r.tx, "spool_assignment", d)
		if e != nil {
			return nil, e
		}
		if e = noteAssignment(r, slotKey{p, int64(num(d["ams_id"])), int64(num(d["tray_id"]))}, intID(d["spool_id"]), false); e != nil {
			return nil, e
		}
		out, e := get(r.tx, "spool_assignment", id)
		if e != nil {
			return nil, e
		}
		token := rand.Text()
		actor := access.From(r.Context())
		if _, e = r.tx.Exec("INSERT INTO bb_slot_config(assignment_id,token,owner,key_id,serial) SELECT ?,?,?,?,serial_number FROM printers WHERE id=?", id, token, actor.ID, actor.KeyID, p); e != nil {
			return nil, e
		}
		return afterCommit(func() any {
			sent := s.ConfigureNativeAssignment != nil && s.ConfigureNativeAssignment(r.Context(), id, token)
			out["spool"] = spool
			out["configured"] = sent
			out["pending_config"] = !sent
			return out
		}), nil
	})
	s.route(m, "DELETE", path+"/{printer}/{ams}/{tray}", inventoryWrite, func(r request) (any, error) {
		p, _ := strconv.ParseInt(r.PathValue("printer"), 10, 64)
		if !access.From(r.Context()).AllowsPrinter(p) {
			return nil, notfound()
		}
		return row{"status": "ok"}, affect(r.tx, "DELETE FROM spool_assignment WHERE printer_id=? AND ams_id=? AND tray_id=?", p, r.PathValue("ams"), r.PathValue("tray"))
	})
}
func (s *Service) spoolChildren(m *http.ServeMux, path, table, model string) {
	s.route(m, "GET", path, inventoryRead, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT * FROM "+table+" WHERE spool_id=? ORDER BY id", r.id())
		if table == "spool_k_profile" {
			v = visibleUsage(r, v)
		}
		return v, e
	})
	s.route(m, "PUT", path, inventoryWrite, func(r request) (any, error) {
		var in []row
		if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 2<<20)).Decode(&in); err != nil {
			return nil, invalid("Expected a list")
		}
		if _, e := get(r.tx, "spool", r.id()); e != nil {
			return nil, e
		}
		if table == "spool_k_profile" {
			old, e := list(r.tx, "SELECT printer_id FROM spool_k_profile WHERE spool_id=?", r.id())
			if e != nil {
				return nil, e
			}
			for _, p := range old {
				if !access.From(r.Context()).AllowsPrinter(intID(p["printer_id"])) {
					return nil, denied()
				}
			}
		}
		if _, e := r.tx.Exec("DELETE FROM "+table+" WHERE spool_id=?", r.id()); e != nil {
			return nil, e
		}
		for _, item := range in {
			d, e := input(model, item)
			if e != nil {
				return nil, e
			}
			if v := intID(d["printer_id"]); v != 0 && !access.From(r.Context()).AllowsPrinter(v) {
				return nil, notfound()
			}
			d["spool_id"] = r.id()
			if _, e = insert(r.tx, table, d); e != nil {
				return nil, e
			}
		}
		if table == "spool_suppliers" {
			return list(r.tx, "SELECT l.*,s.name supplier_name FROM spool_suppliers l JOIN suppliers s ON s.id=l.supplier_id WHERE spool_id=?", r.id())
		}
		return list(r.tx, "SELECT * FROM "+table+" WHERE spool_id=? ORDER BY id", r.id())
	})
}

type tagConflict struct {
	field  string
	holder int64
}

func (e *tagConflict) Error() string { return e.field + " already linked to spool " + str(e.holder) }
