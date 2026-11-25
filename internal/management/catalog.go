package management

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"

	"net/http"
	"strconv"
	"strings"

	"bambastic/internal/access"
)

// catalogSeeds contains the release's spool-core weights and manufacturer color catalog.
//
//go:embed catalogs.json
var catalogSeeds []byte

func seedCatalog(tx *sql.Tx, table string) error {
	var seeds map[string][][]any
	if e := json.Unmarshal(catalogSeeds, &seeds); e != nil {
		return e
	}
	k, sqltext := "DEFAULT_SPOOL_CATALOG", "INSERT INTO spool_catalog(name,weight,is_default) VALUES(?,?,1)"
	if table == "color_catalog" {
		k = "DEFAULT_COLOR_CATALOG"
		sqltext = "INSERT INTO color_catalog(manufacturer,color_name,hex_color,material,is_default) VALUES(?,?,?,?,1)"
	}
	st, e := tx.Prepare(sqltext)
	if e != nil {
		return e
	}
	defer st.Close()
	for _, r := range seeds[k] {
		if _, e = st.Exec(r...); e != nil {
			return e
		}
	}
	return nil
}
func initializeCatalogs(db *sql.DB) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	n, e := count(tx, "SELECT count(*) FROM settings WHERE key='bambastic_catalogs_initialized'")
	if e != nil {
		return e
	}
	if n == 0 {
		for _, t := range []string{"spool_catalog", "color_catalog"} {
			n, e = count(tx, "SELECT count(*) FROM "+t)
			if e != nil {
				return e
			}
			if n == 0 {
				if e = seedCatalog(tx, t); e != nil {
					return e
				}
			}
		}
		if _, e = tx.Exec("INSERT INTO settings(key,value) VALUES('bambastic_catalogs_initialized','true')"); e != nil {
			return e
		}
	}
	return tx.Commit()
}

type catalog struct{ path, table, create, patch, method, order string }

func (s *Service) referenceData(m *http.ServeMux) {
	for _, c := range []catalog{
		{"inventory/catalog", "spool_catalog", "CatalogEntryCreate", "CatalogEntryUpdate", "PUT", "name"},
		{"inventory/colors", "color_catalog", "ColorEntryCreate", "ColorEntryUpdate", "PUT", "manufacturer,color_name"},
		{"inventory/locations", "locations", "LocationCreate", "LocationUpdate", "PATCH", "name"},
		{"inventory/suppliers", "suppliers", "SupplierCreate", "SupplierUpdate", "PATCH", "name"},
		{"filament-catalog", "filaments", "FilamentCreate", "FilamentUpdate", "PATCH", "name"},
	} {
		read, create, modify, del := inventoryRead, inventoryWrite, inventoryWrite, inventoryWrite
		base := c.path
		if c.table == "filaments" {
			read, create, modify, del = "filaments:read", "filaments:create", "filaments:update", "filaments:delete"
			base += "/{$}"
		}
		s.route(m, "GET", base, read, func(r request) (any, error) { return catalogList(r, c) })
		s.route(m, "POST", base, create, func(r request) (any, error) { return catalogSave(r, c, false) })
		s.route(m, c.method, c.path+"/{id}", modify, func(r request) (any, error) { return catalogSave(r, c, true) })
		s.route(m, "DELETE", c.path+"/{id}", del, func(r request) (any, error) {
			if c.table == "locations" {
				loc, e := get(r.tx, "locations", r.id())
				if e != nil {
					return nil, e
				}
				n, e := count(r.tx, "SELECT count(*) FROM spool WHERE location_id=? OR lower(storage_location)=lower(?)", r.id(), loc["name"])
				if e != nil {
					return nil, e
				}
				if n > 0 {
					return nil, &problem{409, "Location still has spools"}
				}
				if remoteEnabled(r.Context(), r.tx) {
					var spools []row
					if e = remote(r, "GET", "spool?allow_archived=true", nil, &spools); e != nil {
						return nil, e
					}
					for _, sp := range spools {
						if strings.EqualFold(str(sp["location"]), str(loc["name"])) {
							return nil, &problem{409, "Location still has spools"}
						}
					}
				}
				if e != nil {
					return nil, e
				}
			}
			if c.table == "suppliers" {
				n, e := count(r.tx, "SELECT (SELECT count(*) FROM spool_suppliers WHERE supplier_id=?)+(SELECT count(*) FROM spoolman_spool_suppliers WHERE supplier_id=?)", r.id(), r.id())
				if e != nil {
					return nil, e
				}
				if n > 0 {
					return nil, &problem{409, "Supplier still has spools"}
				}
			}
			return row{"status": "deleted"}, affect(r.tx, "DELETE FROM "+c.table+" WHERE id=?", r.id())
		})
		if c.table == "color_catalog" || c.table == "spool_catalog" {
			s.route(m, "POST", c.path+"/reset", inventoryWrite, func(r request) (any, error) {
				if _, e := r.tx.Exec("DELETE FROM " + c.table + " WHERE is_default=1"); e != nil {
					return nil, e
				}
				return row{"status": "reset"}, seedCatalog(r.tx, c.table)
			})
			s.route(m, "POST", c.path+"/bulk-delete", inventoryWrite, func(r request) (any, error) {
				d, e := r.body()
				if e != nil {
					return nil, e
				}
				ids, ok := d["ids"].([]any)
				if !ok {
					return nil, invalid("ids is required")
				}
				if len(ids) == 0 {
					return row{"deleted": 0}, nil
				}
				res, e := r.tx.Exec("DELETE FROM "+c.table+" WHERE id IN ("+placeholders(len(ids))+")", ids...)
				if e != nil {
					return nil, e
				}
				n, e := res.RowsAffected()
				return row{"deleted": n}, e
			})
		}
	}
	s.route(m, "POST", "filament-catalog/seed-defaults", "filaments:create", seedFilaments)
	s.route(m, "GET", "filament-catalog/{id}", "filaments:read", func(r request) (any, error) { return get(r.tx, "filaments", r.id()) })
	s.route(m, "GET", "filament-catalog/by-type/{type}", "filaments:read", func(r request) (any, error) {
		return list(r.tx, "SELECT * FROM filaments WHERE lower(type)=lower(?) ORDER BY name", r.PathValue("type"))
	})
	s.route(m, "POST", "filament-catalog/calculate-cost", "filaments:read", func(r request) (any, error) {
		g, e := strconv.ParseFloat(r.URL.Query().Get("weight_grams"), 64)
		if e != nil || g < 0 {
			return nil, invalid("Invalid weight_grams")
		}
		v, e := one(r.tx, "SELECT * FROM filaments WHERE id=?", r.URL.Query().Get("filament_id"))
		if e != nil {
			return nil, e
		}
		return row{"filament_id": v["id"], "filament_name": v["name"], "currency": v["currency"], "weight_grams": g, "cost": cents(g * num(v["cost_per_kg"]) / 1000)}, nil
	})
	s.route(m, "GET", "inventory/colors/map", "", colorMap)
	s.route(m, "GET", "inventory/colors/search", inventoryRead, func(r request) (any, error) {
		return list(r.tx, "SELECT * FROM color_catalog WHERE instr(lower(manufacturer),lower(?)) AND instr(lower(coalesce(material,'')),lower(?)) ORDER BY manufacturer,color_name", r.URL.Query().Get("manufacturer"), r.URL.Query().Get("material"))
	})
	s.route(m, "GET", "inventory/colors/lookup", inventoryRead, func(r request) (any, error) {
		q := r.URL.Query()
		v, e := one(r.tx, "SELECT hex_color,material FROM color_catalog WHERE lower(manufacturer)=lower(?) AND lower(color_name)=lower(?) LIMIT 1", q.Get("manufacturer"), q.Get("color_name"))
		if e == sql.ErrNoRows {
			return row{"found": false}, nil
		}
		if e != nil {
			return nil, e
		}
		v["found"] = true
		return v, nil
	})
	s.route(m, "GET", "inventory/colors/by-material", inventoryRead, func(r request) (any, error) {
		q := r.URL.Query()
		hex := strings.TrimPrefix(q.Get("hex"), "#")
		v, e := one(r.tx, "SELECT color_name FROM color_catalog WHERE substr(lower(ltrim(hex_color,'#')),1,6)=substr(lower(?),1,6) ORDER BY lower(material)=lower(?) DESC,lower(manufacturer)='bambu lab' DESC,is_default DESC,id LIMIT 1", hex, q.Get("material"))
		if e == sql.ErrNoRows {
			return row{"color_name": nil}, nil
		}
		return v, e
	})
}
func catalogList(r request, c catalog) (any, error) {
	selectSQL := "SELECT * FROM " + c.table
	if c.table == "locations" {
		selectSQL = "SELECT l.*,(SELECT count(*) FROM spool s WHERE s.location_id=l.id AND archived_at IS NULL) AS spool_count FROM locations l"
	}
	if c.table == "suppliers" {
		selectSQL = "SELECT s.*,(SELECT count(*) FROM spool_suppliers l JOIN spool sp ON sp.id=l.spool_id WHERE l.supplier_id=s.id AND sp.archived_at IS NULL) AS spool_count FROM suppliers s"
	}
	rows, e := list(r.tx, selectSQL+" ORDER BY "+c.order)
	for _, v := range rows {
		delete(v, "name_key")
	}
	return rows, e
}
func catalogSave(r request, c catalog, patch bool) (any, error) {
	name := c.create
	if patch {
		name = c.patch
	}
	d, e := r.input(name)
	if e != nil {
		return nil, e
	}
	id := r.id()
	if c.table == "locations" || c.table == "suppliers" {
		if v, ok := d["name"]; ok {
			d["name_key"] = strings.ToLower(str(v))
		}
	}
	if patch {
		if c.table == "locations" || c.table == "suppliers" || c.table == "filaments" {
			d["updated_at"] = now()
		}
		e = update(r.tx, c.table, id, d)
	} else {
		if c.table == "color_catalog" || c.table == "spool_catalog" {
			d["is_default"] = false
		}
		id, e = insert(r.tx, c.table, d)
	}
	if e != nil {
		return nil, e
	}
	if c.table == "locations" && patch && d["name"] != nil {
		if _, e = r.tx.Exec("UPDATE spool SET storage_location=? WHERE location_id=?", d["name"], id); e != nil {
			return nil, e
		}
	}
	v, e := get(r.tx, c.table, id)
	if e != nil {
		return nil, e
	}
	delete(v, "name_key")
	return v, nil
}
func (s *Service) stock(m *http.ServeMux) {
	s.spoolChildren(m, "inventory/spools/{id}/k-profiles", "spool_k_profile", "SpoolKProfileBase")
	s.spoolChildren(m, "inventory/spools/{id}/filament-presets", "spool_filament_preset", "SpoolFilamentPresetBase")
	s.spoolChildren(m, "inventory/spools/{id}/suppliers", "spool_suppliers", "SpoolSupplierLinkInput")
	s.route(m, "GET", "inventory/usage", inventoryRead, usage)
	s.route(m, "GET", "inventory/spools/{id}/usage", inventoryRead, usage)
	s.route(m, "DELETE", "inventory/spools/{id}/usage", inventoryWrite, func(r request) (any, error) {
		hist, e := usage(r)
		if e != nil {
			return nil, e
		}
		for _, item := range hist.([]row) {
			if _, e = r.tx.Exec("DELETE FROM spool_usage_history WHERE id=?", item["id"]); e != nil {
				return nil, e
			}
		}
		return row{"deleted": len(hist.([]row))}, nil
	})
	const shop = "inventory/shopping-list"
	const wr = "inventory:forecast_write inventory:update"
	const rd = "inventory:forecast_read inventory:read"
	s.route(m, "GET", shop, rd, func(r request) (any, error) {
		return list(r.tx, "SELECT * FROM filament_shopping_list ORDER BY added_at DESC,id DESC")
	})
	s.route(m, "POST", shop, wr, func(r request) (any, error) {
		d, e := r.input("ShoppingListItemCreate")
		if e != nil {
			return nil, e
		}
		d["status"] = "pending"
		id, e := insert(r.tx, "filament_shopping_list", d)
		if e != nil {
			return nil, e
		}
		return get(r.tx, "filament_shopping_list", id)
	})
	s.route(m, "PATCH", shop+"/{id}/status", wr, func(r request) (any, error) {
		v, e := r.input("ShoppingListItemStatusUpdate")
		if e != nil {
			return nil, e
		}
		switch v["status"] {
		case "pending":
			v["purchased_at"] = nil
		case "purchased", "received":
			old, e := get(r.tx, "filament_shopping_list", r.id())
			if e != nil {
				return nil, e
			}
			if old["purchased_at"] == nil {
				v["purchased_at"] = now()
			}
		default:
			return nil, bad("Unknown shopping status")
		}
		if e = update(r.tx, "filament_shopping_list", r.id(), v); e != nil {
			return nil, e
		}
		return get(r.tx, "filament_shopping_list", r.id())
	})
	s.route(m, "DELETE", shop+"/{id}", wr, func(r request) (any, error) {
		return row{"status": "deleted"}, affect(r.tx, "DELETE FROM filament_shopping_list WHERE id=?", r.id())
	})
	s.route(m, "DELETE", shop, wr, func(r request) (any, error) {
		res, e := r.tx.Exec("DELETE FROM filament_shopping_list")
		if e != nil {
			return nil, e
		}
		n, e := res.RowsAffected()
		return row{"deleted": n}, e
	})
	s.route(m, "GET", "inventory/sku-settings", rd, func(r request) (any, error) { return list(r.tx, "SELECT * FROM filament_sku_settings ORDER BY id") })
	s.route(m, "POST", "inventory/sku-settings", wr, func(r request) (any, error) {
		d, e := r.input("FilamentSkuSettingsUpsert")
		if e != nil {
			return nil, e
		}
		old, e := one(r.tx, "SELECT id FROM filament_sku_settings WHERE material=? AND subtype IS ? AND brand IS ? AND color_name IS ?", d["material"], d["subtype"], d["brand"], d["color_name"])
		var id int64
		if e == sql.ErrNoRows {
			id, e = insert(r.tx, "filament_sku_settings", d)
		} else if e == nil {
			id = intID(old["id"])
			d["updated_at"] = now()
			e = update(r.tx, "filament_sku_settings", id, d)
		}
		if e != nil {
			return nil, e
		}
		return get(r.tx, "filament_sku_settings", id)
	})
}
func usage(r request) (any, error) {
	q := "SELECT id,spool_id,spoolman_spool_id,printer_id,print_name,weight_used,percent_used,status,cost,created_at FROM spool_usage_history"
	var a []any
	if r.PathValue("id") != "" {
		q += " WHERE spool_id=?"
		a = append(a, r.id())
	}
	items, e := list(r.tx, q+" ORDER BY created_at DESC,id DESC", a...)
	if e != nil {
		return nil, e
	}
	result := []row{}
	for _, v := range items {
		if v["printer_id"] == nil || access.From(r.Context()).AllowsPrinter(intID(v["printer_id"])) {
			result = append(result, v)
		}
	}
	return result, nil
}

//go:embed preset_materials.json
var presetMaterials json.RawMessage

func colorMap(r request) (any, error) {
	v, e := list(r.tx, "SELECT hex_color,color_name,manufacturer,material,is_default FROM color_catalog ORDER BY id")
	if e != nil {
		return nil, e
	}
	type candidate struct {
		name, brand string
		priority    int
	}
	bins, qualified := map[string]candidate{}, map[string]candidate{}
	bambu := []row{}
	for _, c := range v {
		hex := strings.ToLower(strings.TrimPrefix(str(c["hex_color"]), "#"))
		if len(hex) < 6 || str(c["color_name"]) == "" {
			continue
		}
		hex = hex[:6]
		material := strings.TrimSpace(str(c["material"]))
		brand := strings.ToLower(strings.TrimSpace(str(c["manufacturer"])))
		priority := 0
		if brand == "bambu lab" {
			priority = 2
			bambu = append(bambu, row{"hex": hex, "name": c["color_name"], "material": material})
		}
		if truth(c["is_default"]) {
			priority++
		}
		match := candidate{str(c["color_name"]), brand, priority}
		if old, ok := bins[hex]; !ok || old.priority < priority {
			bins[hex] = match
		}
		key := strings.ToLower(material) + "|" + hex
		if material != "" {
			if old, ok := qualified[key]; !ok || old.priority < priority {
				qualified[key] = match
			}
		}
	}
	colors, byMaterial := map[string]string{}, map[string]string{}
	for k, c := range bins {
		colors[k] = c.name
	}
	for k, c := range qualified {
		base := bins[k[strings.LastIndex(k, "|")+1:]]
		if c.name != base.name && c.brand == base.brand {
			byMaterial[k] = c.name
		}
	}
	return row{"colors": colors, "by_material": byMaterial, "bambu": bambu, "preset_materials": presetMaterials}, nil
}

// seedFilaments preserves custom prices and settings on existing name/type pairs.
func seedFilaments(r request) (any, error) {
	defaults := [][]any{
		{"Generic PLA", "PLA", nil, 20.0, 1.24, 190, 220, 50, 60},
		{"Generic PETG", "PETG", nil, 25.0, 1.27, 230, 250, 70, 80},
		{"Generic ABS", "ABS", nil, 22.0, 1.04, 230, 260, 90, 110},
		{"Generic TPU", "TPU", nil, 35.0, 1.21, 220, 250, 40, 60},
		{"Generic ASA", "ASA", nil, 28.0, 1.07, 240, 260, 90, 110},
		{"Bambu PLA Basic", "PLA", "Bambu Lab", 20.0, 1.24, 190, 220, 35, 55},
		{"Bambu PLA Matte", "PLA", "Bambu Lab", 25.0, 1.24, 190, 220, 35, 55},
		{"Bambu PETG Basic", "PETG", "Bambu Lab", 25.0, 1.27, 250, 270, 70, 80},
		{"Bambu ABS", "ABS", "Bambu Lab", 30.0, 1.04, 260, 280, 90, 100},
	}
	var created int64
	for _, d := range defaults {
		values := append(append([]any{}, d...), d[0], d[1])
		res, err := r.tx.Exec(`INSERT INTO filaments(name,type,brand,cost_per_kg,density,print_temp_min,print_temp_max,bed_temp_min,bed_temp_max,spool_weight_g,currency)
            SELECT ?,?,?,?,?,?,?,?,?,1000,'USD' WHERE NOT EXISTS(SELECT 1 FROM filaments WHERE name=? AND type=?)`, values...)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		created += n
	}
	return row{"created": created, "message": fmt.Sprintf("Created %d default filaments", created)}, nil
}
