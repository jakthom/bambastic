package production

import (
	"bambastic/internal/access"
	"context"
	"database/sql"
	"net/http"
	"sort"
	"strings"
)

var variantFields = []string{"library_file_id", "target_model", "plate_id", "ams_mapping", "nozzle_mapping", "nozzle_rack_choice", "filament_overrides", "required_filament_types", "print_time_seconds"}

func (s *Service) variants(ctx context.Context, d database, p access.Principal, raw any, group bool) ([]row, error) {
	entries, ok := raw.([]any)
	if !ok || len(entries) < 2 || len(entries) > 24 {
		return nil, fail(422, "Choose two to twenty-four alternative files")
	}
	models := map[string]bool{}
	used := map[int64]bool{}
	out := []row{}
	for _, item := range entries {
		m, ok := item.(map[string]any)
		if !ok {
			return nil, fail(422, "Invalid alternative")
		}
		k := num(m["library_file_id"])
		if used[k] {
			return nil, fail(422, "File appears twice")
		}
		used[k] = true
		action := "read"
		if group {
			action = "update"
		}
		file, e := s.getAsset(ctx, d, p, "library", k, action)
		if e != nil {
			return nil, e
		}
		if group && num(file["variant_group_id"]) != 0 {
			return nil, fail(409, "File already belongs to a variant group")
		}
		info, e := s.meta("library", file)
		if e != nil {
			return nil, e
		}
		model := modelName(info.Model)
		given := modelName(m["target_model"])
		if given != "" && model != "" && given != model {
			return nil, fail(422, "Target differs from the sliced printer model")
		}
		if model == "" {
			model = given
		}
		if model == "" || model == "ANY" || models[model] {
			return nil, fail(422, "Each alternative needs a distinct printer model")
		}
		models[model] = true
		plateID := num(m["plate_id"])
		var selected *plate
		for i, pl := range info.Plates {
			if pl.Printable && (plateID == 0 || plateID == pl.Index) {
				selected = &info.Plates[i]
				break
			}
		}
		if selected == nil {
			return nil, fail(422, "Alternative has no printable selected plate")
		}
		v := row{"library_file_id": k, "target_model": model, "plate_id": selected.Index, "print_time_seconds": selected.Seconds, "filename": file["filename"], "position": len(out)}
		for _, key := range []string{"ams_mapping", "nozzle_mapping", "nozzle_rack_choice", "filament_overrides", "required_filament_types"} {
			if m[key] != nil {
				v[key] = m[key]
			}
		}
		if v["required_filament_types"] == nil {
			types, _ := plateRequirements(*selected, false)
			if len(types) > 0 {
				v["required_filament_types"] = types
			}
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) alternativeOrder(ctx context.Context, tx *sql.Tx, p access.Principal, m row) ([]int64, error) {
	if num(m["printer_id"]) != 0 || num(m["archive_id"]) != 0 || num(m["library_file_id"]) != 0 {
		return nil, fail(422, "Alternatives are mutually exclusive with a printer or single file")
	}
	variants, e := s.variants(ctx, tx, p, m["variants"], false)
	if e != nil {
		return nil, e
	}
	base := row{}
	for k, x := range m {
		if k != "variants" {
			base[k] = x
		}
	}
	for _, key := range variantFields {
		if x := variants[0][key]; x != nil {
			base[key] = x
		}
	}
	ids, e := s.createCopies(ctx, tx, p, base)
	if e != nil {
		return nil, e
	}
	for _, qid := range ids {
		for _, v := range variants {
			entry := row{"queue_item_id": qid, "position": v["position"]}
			for _, key := range variantFields {
				if value := v[key]; value != nil {
					switch key {
					case "ams_mapping", "nozzle_mapping", "nozzle_rack_choice", "filament_overrides", "required_filament_types":
						entry[key] = jstr(value)
					default:
						entry[key] = value
					}
				}
			}
			if _, e = insert(ctx, tx, "print_queue_variants", entry); e != nil {
				return nil, e
			}
		}
	}
	return ids, nil
}
func (s *Service) alternativeFor(ctx context.Context, k int64, pr row) (row, bool, error) {
	rs, e := records(ctx, s.db, "SELECT * FROM print_queue_variants WHERE queue_item_id=? ORDER BY position,id", k)
	if e != nil {
		return nil, false, e
	}
	for _, candidate := range rs {
		if modelName(candidate["target_model"]) == modelName(pr["model"]) {
			return candidate, true, nil
		}
	}
	return nil, len(rs) > 0, nil
}
func (s *Service) variantRoutes(rt register) {
	rt("POST", "/library/variant-groups", s.variantGroup)
	rt("POST", "/library/variant-groups/{$}", s.variantGroup)
	rt("GET", "/library/variant-groups/by-file/{id}", s.variantGroup)
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		rt(m, "/library/variant-groups/{id}", s.variantGroup)
	}
	rt("POST", "/library/variant-groups/{id}/members", s.variantGroup)
	rt("DELETE", "/library/variant-groups/{id}/members/{file}", s.variantGroup)
}
func (s *Service) variantGroup(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	perm := "read"
	if r.Method != "GET" {
		perm = "update"
	}
	if !p.Has("library:"+perm+"_all") && !p.Has("library:"+perm+"_own") {
		return nil, fail(403, "Library permission required")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	k := pathID(r)
	if strings.Contains(r.URL.Path, "/by-file/") {
		f, e := s.getAsset(ctx, tx, p, "library", k, "read")
		if e != nil {
			return nil, e
		}
		k = num(f["variant_group_id"])
		if k == 0 {
			return nil, sql.ErrNoRows
		}
	}
	attached := []row{}
	if k != 0 {
		attached, e = records(ctx, tx, "SELECT * FROM library_files WHERE variant_group_id=? AND deleted_at IS NULL ORDER BY variant_position,id", k)
		if e != nil {
			return nil, e
		}
		if len(attached) == 0 {
			return nil, sql.ErrNoRows
		}
		for _, f := range attached {
			if !s.accessible(ctx, tx, p, "library", f, perm) {
				return nil, sql.ErrNoRows
			}
		}
	}
	if r.Method == "DELETE" {
		if f := num(r.PathValue("file")); f != 0 {
			res, e := tx.ExecContext(ctx, "UPDATE library_files SET variant_group_id=NULL,variant_target_model=NULL,variant_position=0 WHERE variant_group_id=? AND id=?", k, f)
			if e != nil {
				return nil, e
			}
			n, _ := res.RowsAffected()
			if n == 0 {
				return nil, sql.ErrNoRows
			}
			if len(attached) > 2 {
				return row{"removed": true}, tx.Commit()
			}
		}
		if _, e = tx.ExecContext(ctx, "UPDATE library_files SET variant_group_id=NULL,variant_target_model=NULL,variant_position=0 WHERE variant_group_id=?", k); e != nil {
			return nil, e
		}
		_, e = tx.ExecContext(ctx, "DELETE FROM file_variant_groups WHERE id=?", k)
		if e != nil {
			return nil, e
		}
		return row{"deleted": true}, tx.Commit()
	}
	if r.Method == "POST" || r.Method == "PATCH" {
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		name := strings.TrimSpace(val(m["name"]))
		if len(name) > 255 {
			return nil, fail(422, "Group name too long")
		}
		if r.Method == "PATCH" {
			if name != "" {
				_, e = tx.ExecContext(ctx, "UPDATE file_variant_groups SET name=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", name, k)
				if e != nil {
					return nil, e
				}
			}
			if order, ok := m["member_file_ids"]; ok {
				ids, e := idsOf(order)
				if e != nil {
					return nil, e
				}
				if len(ids) != len(attached) {
					return nil, fail(422, "Order must include every current member")
				}
				for i, f := range ids {
					res, e := tx.ExecContext(ctx, "UPDATE library_files SET variant_position=? WHERE id=? AND variant_group_id=?", i, f, k)
					if e != nil {
						return nil, e
					}
					n, _ := res.RowsAffected()
					if n != 1 {
						return nil, fail(422, "Unknown group member")
					}
				}
			}
		} else {
			var vs []row
			if k == 0 {
				vs, e = s.variants(ctx, tx, p, m["members"], true)
				if e != nil {
					return nil, e
				}
				if name == "" {
					name = val(vs[0]["filename"])
				}
				k, e = insert(ctx, tx, "file_variant_groups", row{"name": name, "created_by_id": id(p.ID)})
				if e != nil {
					return nil, e
				}
			} else {
				first := attached[0]
				input := []any{map[string]any{"library_file_id": first["id"], "target_model": first["variant_target_model"]}, map[string]any(m)}
				pair, e := s.variants(ctx, tx, p, input, false)
				if e != nil {
					return nil, e
				}
				v := pair[1]
				f, e := s.getAsset(ctx, tx, p, "library", num(v["library_file_id"]), "update")
				if e != nil {
					return nil, e
				}
				if num(f["variant_group_id"]) != 0 {
					return nil, fail(409, "File already belongs to a group")
				}
				for _, old := range attached {
					if modelName(old["variant_target_model"]) == v["target_model"] {
						return nil, fail(422, "Model already represented")
					}
				}
				vs = []row{v}
			}
			for i, v := range vs {
				if _, e = tx.ExecContext(ctx, "UPDATE library_files SET variant_group_id=?,variant_position=?,variant_target_model=? WHERE id=?", k, len(attached)+i, v["target_model"], v["library_file_id"]); e != nil {
					return nil, e
				}
			}
		}
	}
	group, e := one(ctx, tx, "SELECT * FROM file_variant_groups WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	group["members"], e = records(ctx, tx, "SELECT id library_file_id,filename,variant_target_model target_model,variant_position position FROM library_files WHERE variant_group_id=? AND deleted_at IS NULL ORDER BY variant_position,id", k)
	if e != nil {
		return nil, e
	}
	return group, tx.Commit()
}
func boundPage(items []row, offset, limit int64) []row {
	offset = min(max(offset, 0), int64(len(items)))
	if limit < 1 {
		limit = 100
	}
	return items[offset:min(int64(len(items)), offset+min(1000, limit))]
}
func sortedNames(m map[string]bool) []string {
	out := []string{}
	for name := range m {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
