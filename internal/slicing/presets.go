package slicing

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

//go:embed models.json
var printerModels []byte

const presetCols = "id,name,preset_type,source,filament_type,filament_vendor,nozzle_temp_min,nozzle_temp_max,pressure_advance,default_filament_colour,filament_cost,filament_density,compatible_printers,inherits,version,created_at,updated_at"

type presetRef struct {
	Source string `json:"source"`
	ID     string `json:"id"`
}

func slots() map[string][]object {
	return map[string][]object{"printer": {}, "filament": {}, "process": {}}
}
func rows(db *sql.DB, ctx context.Context, q string, args ...any) ([]object, error) {
	r, e := db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(r)
}
func (s *Service) listPresets(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:read") {
		return
	}
	data, e := rows(s.db, r.Context(), "SELECT "+presetCols+" FROM local_presets ORDER BY lower(name),id")
	if e != nil {
		httpx.Error(w, 500, "Could not read presets")
		return
	}
	out := slots()
	for _, v := range data {
		k := httpx.Str(v["preset_type"])
		out[k] = append(out[k], v)
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) preset(ctx context.Context, id string) (object, error) {
	a, e := rows(s.db, ctx, "SELECT "+presetCols+",setting FROM local_presets WHERE id=?", id)
	if e != nil {
		return nil, e
	}
	if len(a) == 0 {
		return nil, sql.ErrNoRows
	}
	o := a[0]
	var setting object
	e = json.Unmarshal([]byte(httpx.Str(o["setting"])), &setting)
	o["setting"] = setting
	return o, e
}
func (s *Service) getPreset(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:read") {
		return
	}
	v, e := s.preset(r.Context(), r.PathValue("id"))
	if e != nil {
		presetError(w, e)
		return
	}
	httpx.JSON(w, 200, v)
}
func presetError(w http.ResponseWriter, e error) {
	if errors.Is(e, sql.ErrNoRows) {
		httpx.Error(w, 404, "Preset not found")
	} else {
		httpx.Error(w, 500, "Could not access preset storage")
	}
}
func (s *Service) deletePreset(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:update") {
		return
	}
	res, e := s.db.ExecContext(r.Context(), "DELETE FROM local_presets WHERE id=?", r.PathValue("id"))
	if e != nil {
		presetError(w, e)
		return
	}
	n, e := res.RowsAffected()
	if e != nil || n == 0 {
		presetError(w, sql.ErrNoRows)
		return
	}
	httpx.JSON(w, 200, object{"success": true})
}
func asText(v any) string {
	switch t := v.(type) {
	case []any:
		if len(t) > 0 {
			return asText(t[0])
		}
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		if t {
			return "1"
		}
		return "0"
	}
	return ""
}
func blankNull(v string) any {
	if v == "" {
		return nil
	}
	return v
}

var slotType = map[string]string{"printer": "machine", "filament": "filament", "process": "process"}

type editor struct {
	Name    string `json:"name"`
	Type    string `json:"preset_type"`
	Setting object `json:"setting"`
}

func persistPreset(ctx context.Context, tx *sql.Tx, id int64, v editor, source string) (int64, error) {
	if strings.TrimSpace(v.Name) == "" || len(v.Name) > 300 || slotType[v.Type] == "" || v.Setting == nil {
		return 0, errors.New("Each preset needs a name, a valid preset type and its settings")
	}
	raw, e := json.Marshal(v.Setting)
	if e != nil {
		return 0, e
	}
	if id == 0 {
		a, e := tx.ExecContext(ctx, "INSERT INTO local_presets(name,preset_type,source,setting) VALUES(?,?,?,?)", v.Name, v.Type, source, string(raw))
		if e != nil {
			return 0, e
		}
		id, e = a.LastInsertId()
		if e != nil {
			return 0, e
		}
	}
	var compatible any
	if a, ok := v.Setting["compatible_printers"].([]any); ok {
		b, _ := json.Marshal(a)
		compatible = string(b)
	}
	val := func(k string) any { return blankNull(asText(v.Setting[k])) }
	_, e = tx.ExecContext(ctx, `UPDATE local_presets SET name=?,setting=?,filament_type=?,filament_vendor=?,nozzle_temp_min=?,nozzle_temp_max=?,pressure_advance=?,default_filament_colour=?,filament_cost=?,filament_density=?,compatible_printers=?,inherits=?,version=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, v.Name, string(raw), val("filament_type"), val("filament_vendor"), val("nozzle_temperature_range_low"), val("nozzle_temperature_range_high"), val("pressure_advance"), val("default_filament_colour"), val("filament_cost"), val("filament_density"), compatible, val("inherits"), val("version"), id)
	return id, e
}
func (s *Service) savePreset(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:update") {
		return
	}
	var v editor
	if !httpx.Decode(w, r, &v) {
		return
	}
	var id int64
	if r.Method == "PUT" {
		current, e := s.preset(r.Context(), r.PathValue("id"))
		if e != nil {
			presetError(w, e)
			return
		}
		id, _ = strconv.ParseInt(r.PathValue("id"), 10, 64)
		if v.Name == "" {
			v.Name = httpx.Str(current["name"])
		}
		v.Type = httpx.Str(current["preset_type"])
		if v.Setting == nil {
			v.Setting, _ = current["setting"].(object)
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		presetError(w, e)
		return
	}
	defer tx.Rollback()
	id, e = persistPreset(r.Context(), tx, id, v, "manual")
	if e != nil {
		httpx.Error(w, 422, e.Error())
		return
	}
	if e = tx.Commit(); e != nil {
		presetError(w, e)
		return
	}
	out, e := s.preset(r.Context(), strconv.FormatInt(id, 10))
	if e != nil {
		presetError(w, e)
		return
	}
	delete(out, "setting")
	httpx.JSON(w, 200, out)
}
func classify(v object, filename string) string {
	switch strings.ToLower(asText(v["type"])) {
	case "machine", "printer":
		return "printer"
	case "filament":
		return "filament"
	case "process", "print":
		return "process"
	}
	for _, k := range strings.Split(strings.ToLower(strings.ReplaceAll(filename, "\\", "/")), "/") {
		switch k {
		case "filament", "printer", "process":
			return k
		case "machine":
			return "printer"
		}
	}
	for _, group := range []struct {
		kind string
		keys []string
	}{
		{"process", []string{"print_settings_id"}}, {"filament", []string{"filament_settings_id"}}, {"printer", []string{"printer_settings_id"}},
		{"process", []string{"layer_height", "first_layer_height", "wall_loops", "prime_tower_width", "outer_wall_speed", "inner_wall_speed", "sparse_infill_density", "initial_layer_print_height"}},
		{"printer", []string{"printer_model", "nozzle_diameter", "machine_start_gcode", "printable_area", "machine_end_gcode"}},
		{"filament", []string{"filament_type", "filament_vendor", "filament_cost", "nozzle_temperature", "filament_density"}},
	} {
		for _, k := range group.keys {
			if _, ok := v[k]; ok {
				return group.kind
			}
		}
	}
	return ""
}
func (s *Service) importPresets(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:update") {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 36<<20)
	file, h, e := r.FormFile("file")
	if e != nil {
		httpx.Error(w, 400, "Choose a preset file under 32 MiB")
		return
	}
	defer file.Close()
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	data, e := io.ReadAll(io.LimitReader(file, 32<<20+1))
	if e != nil || len(data) > 32<<20 {
		httpx.Error(w, 413, "Preset export exceeds 32 MiB")
		return
	}
	var parsed []editor
	imported, skipped := 0, 0
	errs := []string{}
	save := func(name string, data []byte) {
		var o object
		if json.Unmarshal(data, &o) != nil || o == nil {
			errs = append(errs, name+": invalid JSON preset")
			return
		}
		k := classify(o, name)
		n := asText(o["name"])
		if k == "" || n == "" {
			skipped++
			return
		}
		parsed = append(parsed, editor{n, k, o})
	}
	if strings.HasSuffix(strings.ToLower(h.Filename), ".json") {
		save(h.Filename, data)
	} else {
		z, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if e != nil {
			httpx.Error(w, 400, "Expected JSON or a ZIP preset export")
			return
		}
		total := uint64(0)
		for _, item := range z.File {
			if !strings.EqualFold(path.Ext(item.Name), ".json") || strings.Contains(item.Name, "bundle_structure") {
				continue
			}
			if item.UncompressedSize64 > 8<<20 || total > 64<<20-item.UncompressedSize64 {
				httpx.Error(w, 413, "Expanded preset export exceeds the limit")
				return
			}
			total += item.UncompressedSize64
			rd, e := item.Open()
			if e != nil {
				errs = append(errs, item.Name+": cannot open")
				continue
			}
			raw, e := io.ReadAll(io.LimitReader(rd, 8<<20+1))
			rd.Close()
			if e != nil || len(raw) > 8<<20 {
				errs = append(errs, item.Name+": invalid JSON member")
				continue
			}
			save(item.Name, raw)
		}
	}
	for i := range parsed {
		if parsed[i].Setting["inherits"] != nil {
			parsed[i].Setting = s.inheritBase(r.Context(), parsed[i].Setting, parsed[i].Type, map[string]bool{})
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		presetError(w, e)
		return
	}
	defer tx.Rollback()
	for _, v := range parsed {
		var existing int64
		err := tx.QueryRowContext(r.Context(), "SELECT id FROM local_presets WHERE name=? AND preset_type=? ORDER BY id LIMIT 1", v.Name, v.Type).Scan(&existing)
		if err == nil {
			skipped++
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			presetError(w, err)
			return
		}
		if _, err = persistPreset(r.Context(), tx, 0, v, "orcaslicer"); err != nil {
			presetError(w, err)
			return
		}
		imported++
	}
	if e = tx.Commit(); e != nil {
		presetError(w, e)
		return
	}
	httpx.JSON(w, 200, object{"success": len(errs) == 0, "imported": imported, "skipped": skipped, "errors": errs})
}
func (s *Service) resolve(ctx context.Context, slot string, ref presetRef) (object, error) {
	if ref.ID == "" {
		return nil, fmt.Errorf("Choose a %s preset", slot)
	}
	if ref.Source == "standard" {
		return object{"name": ref.ID, "inherits": ref.ID, "from": "system", "type": slotType[slot]}, nil
	}
	if (ref.Source == "cloud" || ref.Source == "orca_cloud") && s.cloud != nil {
		return s.cloud.ResolvePreset(ctx, access.From(ctx), ref.Source, ref.ID, slot)
	}
	if ref.Source != "local" {
		return nil, fmt.Errorf("The %s preset requires its cloud service to be connected", slot)
	}
	var raw string
	if e := s.db.QueryRowContext(ctx, "SELECT setting FROM local_presets WHERE id=? AND preset_type=?", ref.ID, slot).Scan(&raw); e != nil {
		return nil, fmt.Errorf("Cannot read %s preset %s", slot, ref.ID)
	}
	var o object
	if e := json.Unmarshal([]byte(raw), &o); e != nil || o == nil {
		return nil, errors.New("Invalid local preset settings")
	}
	return o, nil
}
func (s *Service) unified(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "library:upload") {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	data, e := rows(s.db, ctx, "SELECT "+presetCols+" FROM local_presets ORDER BY lower(name),id")
	if e != nil {
		presetError(w, e)
		return
	}
	loc, std := slots(), slots()
	for _, o := range data {
		k := httpx.Str(o["preset_type"])
		var compat []string
		_ = json.Unmarshal([]byte(httpx.Str(o["compatible_printers"])), &compat)
		loc[k] = append(loc[k], object{"id": fmt.Sprint(o["id"]), "name": o["name"], "source": "local", "inherits": o["inherits"], "compatible_printers": compat, "filament_type": o["filament_type"], "filament_colour": o["default_filament_colour"]})
	}
	_, base, e := s.endpoint(ctx)
	if e == nil {
		var bundled map[string][]object
		if _, e = s.jsonCall(ctx, "GET", base+"/profiles/bundled", nil, &bundled); e == nil {
			for k := range std {
				for _, item := range bundled[k] {
					name := httpx.Str(item["name"])
					if name != "" {
						std[k] = append(std[k], object{"name": name, "id": name, "source": "standard", "compatible_printers": item["compatible_printers"], "filament_type": item["filament_type"], "inherits": item["inherits"]})
					}
				}
			}
		}
	}
	cloud, orca := slots(), slots()
	cs, os := "not_authenticated", "not_authenticated"
	aliases := map[string]presetRef{}
	if s.cloud != nil {
		type tier struct {
			source string
			groups map[string][]object
			status string
		}
		ch := make(chan tier, 2)
		for _, source := range []string{"cloud", "orca_cloud"} {
			go func() {
				v, status := s.cloud.CloudProfiles(r.Context(), access.From(r.Context()), source)
				ch <- tier{source, v, status}
			}()
		}
		for i := 0; i < 2; i++ {
			v := <-ch
			if v.source == "cloud" {
				cloud = v.groups
				cs = v.status
			} else {
				orca = v.groups
				os = v.status
			}
		}
		for k, standard := range std {
			stock := map[string]object{}
			for _, entry := range cloud[k] {
				if custom, ok := entry["is_custom"].(bool); ok && !custom {
					stock[httpx.Str(entry["name"])] = entry
				}
			}
			remaining := []object{}
			for _, row := range standard {
				if other, ok := stock[httpx.Str(row["name"])]; ok {
					aliases["standard:"+httpx.Str(row["id"])] = presetRef{"cloud", fmt.Sprint(other["id"])}
				} else {
					remaining = append(remaining, row)
				}
			}
			std[k] = remaining
		}
	}
	httpx.JSON(w, 200, object{"local": loc, "standard": std, "cloud": cloud, "orca_cloud": orca, "stock_aliases": aliases, "cloud_status": cs, "orca_cloud_status": os})
}
func (s *Service) values(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "library:upload") {
		return
	}
	q := r.URL.Query()
	slot := q.Get("slot")
	if slot == "" {
		slot = "process"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	result := object{"resolved": false, "reason": "preset_unresolved", "values": object{}}
	profile, e := s.resolve(ctx, slot, presetRef{q.Get("source"), q.Get("id")})
	if e == nil && slot == "process" {
		_, base, e := s.endpoint(ctx)
		if e == nil {
			var res struct{ Profile object }
			code, e := s.jsonCall(ctx, "POST", base+"/profiles/resolve", object{"category": slot, "profile": profile}, &res)
			switch {
			case e == nil && res.Profile != nil:
				result = object{"resolved": true, "values": res.Profile, "reason": "ok"}
			case code == 404:
				result["reason"] = "sidecar_outdated"
			default:
				result["reason"] = "sidecar_unavailable"
			}
		} else {
			result["reason"] = "not_configured"
		}
	}
	httpx.JSON(w, 200, result)
}
