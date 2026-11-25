package management

import (
	"bytes"
	"database/sql"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var csvColumns = strings.Fields("id material brand subtype color_name rgba extra_colors effect_type label_weight weight_used remaining cost_per_kg nozzle_temp_min nozzle_temp_max last_used note storage_location category low_stock_threshold_pct material_number suppliers purchase_supplier")

func formula(s string) bool { return s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) }
func (s *Service) inventoryCSV(m *http.ServeMux) {
	s.route(m, "GET", "inventory/spools/export", inventoryRead, func(r request) (any, error) {
		rs, e := list(r.tx, "SELECT s.*, max(0,label_weight-weight_used) remaining FROM spool s WHERE archived_at IS NULL ORDER BY material,brand,color_name,id")
		if e != nil {
			return nil, e
		}
		var buf bytes.Buffer
		buf.WriteString("\uFEFF")
		w := csv.NewWriter(&buf)
		w.UseCRLF = true
		w.Write(csvColumns)
		for _, v := range rs {
			links, e := list(r.tx, "SELECT p.name,l.is_purchase_source FROM spool_suppliers l JOIN suppliers p ON p.id=l.supplier_id WHERE l.spool_id=? ORDER BY p.name_key", v["id"])
			if e != nil {
				return nil, e
			}
			names := []string{}
			for _, l := range links {
				names = append(names, str(l["name"]))
				if truth(l["is_purchase_source"]) {
					v["purchase_supplier"] = l["name"]
				}
			}
			v["suppliers"] = strings.Join(names, "; ")
			cells := []string{}
			for _, k := range csvColumns {
				value := str(v[k])
				if formula(value) {
					value = "'" + value
				}
				cells = append(cells, value)
			}
			w.Write(cells)
		}
		w.Flush()
		return download{"text/csv; charset=utf-8", "bambastic_inventory_" + time.Now().UTC().Format("20060102") + ".csv", buf.Bytes()}, w.Error()
	})
	s.route(m, "POST", "inventory/spools/import", inventoryWrite, func(r request) (any, error) {
		mr, e := r.MultipartReader()
		if e != nil {
			return nil, invalid("Upload a CSV file as multipart form-data")
		}
		for {
			p, e := mr.NextPart()
			if e != nil {
				return nil, invalid("CSV file is required")
			}
			if p.FormName() != "file" {
				p.Close()
				continue
			}
			b, e := io.ReadAll(io.LimitReader(p, 5<<20+1))
			p.Close()
			if e != nil {
				return nil, e
			}
			if len(b) > 5<<20 {
				return nil, &problem{413, "CSV exceeds 5 MB"}
			}
			return importCSV(r, b, r.URL.Query().Get("dry_run") == "true")
		}
	})
}
func parseCSVDate(s string) (string, bool) {
	for _, l := range []string{time.RFC3339Nano, "2006-01-02", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05 -0700 MST"} {
		if t, e := time.Parse(l, s); e == nil {
			return t.UTC().Format(time.RFC3339), true
		}
	}
	return "", false
}
func csvRGBA(s string) (string, bool) {
	v := strings.TrimPrefix(s, "#")
	if len(v) == 6 {
		v += "FF"
	}
	if len(v) != 8 {
		return "", false
	}
	_, e := hex.DecodeString(v)
	return strings.ToLower(v), e == nil
}
func importCSV(r request, b []byte, dry bool) (any, error) {
	rows := []row{}
	failed := []row{}
	warnings := []string{}
	valid, skipped := 0, 0
	summary := func() row {
		if dry {
			return row{"columns": csvColumns, "total": len(rows), "valid_count": valid, "error_count": len(failed), "skipped_count": skipped, "rows": rows, "warnings": warnings}
		}
		return row{"created": valid, "errors": len(failed), "skipped": skipped, "error_rows": failed}
	}
	if !utf8.Valid(b) {
		warnings = append(warnings, "File is not UTF-8")
		return summary(), nil
	}
	text := strings.TrimPrefix(string(b), "\uFEFF")
	head, _, _ := strings.Cut(text, "\n")
	reader := csv.NewReader(strings.NewReader(text))
	reader.FieldsPerRecord = -1
	if strings.Count(head, ";") > strings.Count(head, ",") {
		reader.Comma = ';'
	}
	header, e := reader.Read()
	if e != nil {
		warnings = append(warnings, "Missing CSV header")
		return summary(), nil
	}
	idx := map[string]int{}
	for n, x := range header {
		k := strings.NewReplacer(" ", "_", "-", "_").Replace(strings.ToLower(strings.TrimSpace(x)))
		if !slices.Contains(csvColumns, k) {
			warnings = append(warnings, "Ignoring column "+k)
			continue
		}
		if _, ok := idx[k]; !ok {
			idx[k] = n
		}
	}
	if _, ok := idx["material"]; !ok {
		warnings = append(warnings, "Required column material is missing")
		return summary(), nil
	}
	maxID := int64(0)
	if e = r.tx.QueryRow("SELECT coalesce((SELECT seq FROM sqlite_sequence WHERE name='spool'),0)").Scan(&maxID); e != nil {
		return nil, e
	}
	seenID := map[int64]bool{}
	sup, err := list(r.tx, "SELECT id,name_key FROM suppliers")
	if err != nil {
		return nil, err
	}
	supMap := map[string]int64{}
	for _, x := range sup {
		supMap[str(x["name_key"])] = intID(x["id"])
	}
	cols, e := list(r.tx, "SELECT manufacturer,color_name,material,hex_color,extra_colors,effect_type FROM color_catalog ORDER BY id")
	if e != nil {
		return nil, e
	}
	catalog := map[string][]row{}
	for _, x := range cols {
		k := strings.ToLower(str(x["manufacturer"]) + "\n" + str(x["color_name"]))
		catalog[k] = append(catalog[k], x)
	}
	exRows, e := list(r.tx, "SELECT material,brand,color_name FROM spool WHERE archived_at IS NULL")
	if e != nil {
		return nil, e
	}
	exists := map[string]bool{}
	dupe := func(v row) string {
		return strings.ToLower(str(v["material"]) + "\n" + str(v["brand"]) + "\n" + str(v["color_name"]))
	}
	for _, v := range exRows {
		exists[dupe(v)] = true
	}
	for n := 1; ; n++ {
		cells, e := reader.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, invalid("Malformed CSV quoting")
		}
		getCell := func(k string) string {
			i, ok := idx[k]
			if !ok || i >= len(cells) {
				return ""
			}
			s := strings.TrimSpace(cells[i])
			if strings.HasPrefix(s, "'") && formula(s[1:]) {
				s = s[1:]
			}
			return s
		}
		raw := row{}
		v := row{"row_number": n, "resolved_color": false, "cross_material_color": false, "duplicate_of_existing": false, "status": "valid", "keeps_id": nil, "supplier_ids": []int64{}, "purchase_supplier_id": nil}
		rows = append(rows, v)
		if strings.TrimSpace(strings.Join(cells, "")) == "" {
			v["status"] = "skipped"
			v["reason"] = "Empty row"
			skipped++
			continue
		}
		reason := ""
		for _, k := range csvColumns {
			cell := getCell(k)
			if cell == "" {
				continue
			}
			switch k {
			case "id":
				id, err := strconv.ParseInt(cell, 10, 64)
				if err != nil || id <= 0 || id == math.MaxInt64 {
					reason = "id must be a positive whole number"
				} else if id > maxID && !seenID[id] {
					v["keeps_id"] = id
				} else {
					warnings = append(warnings, fmt.Sprintf("Row %d will receive a new spool ID", n))
				}
			case "suppliers", "purchase_supplier", "remaining":
				continue
			case "label_weight", "nozzle_temp_min", "nozzle_temp_max", "low_stock_threshold_pct":
				i, err := strconv.ParseInt(cell, 10, 32)
				if err != nil {
					reason = "Invalid " + k
				} else {
					raw[k] = float64(i)
				}
			case "weight_used", "cost_per_kg":
				if reader.Comma == ';' {
					cell = strings.ReplaceAll(cell, ",", ".")
				}
				v, err := strconv.ParseFloat(cell, 64)
				if err != nil || math.IsInf(v, 0) || math.IsNaN(v) || v < 0 {
					reason = "Invalid " + k
				} else {
					raw[k] = v
				}
			case "last_used":
				val, ok := parseCSVDate(cell)
				if !ok {
					reason = "Invalid last_used"
				} else {
					raw[k] = val
				}
			case "rgba":
				rgba, ok := csvRGBA(cell)
				if !ok {
					reason = "Invalid rgba"
				} else {
					raw[k] = rgba
				}
			default:
				raw[k] = cell
			}
		}
		for _, k := range []string{"material", "brand", "color_name"} {
			v[k] = raw[k]
		}
		sp, err := input("SpoolCreate", raw)
		if err != nil {
			reason = err.Error()
		}
		if reason == "" && (num(sp["weight_used"]) > num(sp["label_weight"]) || num(sp["label_weight"]) < 0) {
			reason = "Used weight exceeds label weight"
		}
		if reason != "" {
			v["status"] = "error"
			v["reason"] = reason
			v["keeps_id"] = nil
			failed = append(failed, v)
			continue
		}
		if id := intID(v["keeps_id"]); id > 0 {
			seenID[id] = true
		}
		if raw["last_used"] != nil {
			sp["last_used"] = raw["last_used"]
		}
		if sp["rgba"] == nil && str(raw["color_name"]) != "" {
			colors := catalog[strings.ToLower(str(raw["brand"])+"\n"+str(raw["color_name"]))]
			if len(colors) > 0 {
				best := colors[0]
				for _, x := range colors {
					if strings.EqualFold(str(x["material"]), str(raw["material"])) {
						best = x
						break
					}
				}
				if rgba, ok := csvRGBA(str(best["hex_color"])); ok {
					sp["rgba"] = rgba
					v["resolved_color"] = true
					v["cross_material_color"] = str(best["material"]) != "" && !strings.EqualFold(str(best["material"]), str(raw["material"]))
					for _, k := range []string{"extra_colors", "effect_type"} {
						if sp[k] == nil {
							sp[k] = best[k]
						}
					}
				}
			}
		}
		ids := []int64{}
		purchase := strings.ToLower(getCell("purchase_supplier"))
		for _, name := range append(strings.Split(getCell("suppliers"), ";"), purchase) {
			key := strings.ToLower(strings.TrimSpace(name))
			if key == "" {
				continue
			}
			id := supMap[key]
			if id == 0 {
				msg := "Unknown supplier: " + key
				if !slices.Contains(warnings, msg) {
					warnings = append(warnings, msg)
				}
				continue
			}
			if !slices.Contains(ids, id) {
				ids = append(ids, id)
			}
			if key == purchase {
				v["purchase_supplier_id"] = id
			}
		}
		v["supplier_ids"] = ids
		v["duplicate_of_existing"] = exists[dupe(raw)]
		v["rgba"] = sp["rgba"]
		v["spool"] = sp
		valid++
	}
	if dry {
		return summary(), nil
	}
	for _, keep := range []bool{true, false} {
		for _, v := range rows {
			if v["status"] != "valid" || (v["keeps_id"] != nil) != keep {
				continue
			}
			sp := v["spool"].(row)
			if keep {
				sp["id"] = v["keeps_id"]
			}
			sp["added_full"] = num(sp["weight_used"]) == 0
			sp["weight_locked"] = num(sp["weight_used"]) > 0
			if name := strings.TrimSpace(str(sp["storage_location"])); name != "" {
				if _, err := r.tx.Exec("INSERT INTO locations(name,name_key) VALUES(?,?) ON CONFLICT(name_key) DO NOTHING", name, strings.ToLower(name)); err != nil {
					return nil, err
				}
				if loc, e := one(r.tx, "SELECT id FROM locations WHERE name_key=?", strings.ToLower(name)); e == nil {
					sp["location_id"] = loc["id"]
				} else if !errors.Is(e, sql.ErrNoRows) {
					return nil, e
				}
			}
			id, e := insert(r.tx, "spool", sp)
			if e != nil {
				return nil, e
			}
			for _, supplier := range v["supplier_ids"].([]int64) {
				if _, e = r.tx.Exec("INSERT INTO spool_suppliers(spool_id,supplier_id,is_purchase_source) VALUES(?,?,?)", id, supplier, supplier == intID(v["purchase_supplier_id"])); e != nil {
					return nil, e
				}
			}
		}
	}
	return summary(), nil
}
