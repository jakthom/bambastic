package management

import (
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strings"

	"bambastic/internal/access"
)

const projectPath = "projects/{id}"

func (s *Service) projects(m *http.ServeMux) {
	s.projectExtras(m)
	read := "projects:read"
	write := "projects:update"
	for _, path := range []string{"projects", "projects/{$}", "projects/templates"} {
		s.route(m, "GET", path, read, func(r request) (any, error) {
			q := "SELECT * FROM projects WHERE is_template=?"
			args := []any{path == "projects/templates"}
			if v := r.URL.Query().Get("status"); v != "" {
				q += " AND status=?"
				args = append(args, v)
			}
			rows, e := list(r.tx, q+" ORDER BY updated_at DESC,id DESC", args...)
			if e != nil {
				return nil, e
			}
			for _, p := range rows {
				if e = projectSummary(r, p); e != nil {
					return nil, e
				}
			}
			return rows, nil
		})
	}
	s.route(m, "POST", "projects/{$}", "projects:create", func(r request) (any, error) { return saveProject(r, 0) })
	s.route(m, "GET", projectPath, read, func(r request) (any, error) { return projectView(r, r.id()) })
	s.route(m, "PATCH", projectPath, write, func(r request) (any, error) { return saveProject(r, r.id()) })
	s.route(m, "DELETE", projectPath, "projects:delete", func(r request) (any, error) {
		p, e := get(r.tx, "projects", r.id())
		if e != nil {
			return nil, e
		}
		if _, e = r.tx.Exec("UPDATE projects SET parent_id=? WHERE parent_id=?", p["parent_id"], r.id()); e != nil {
			return nil, e
		}
		return row{"message": "Project deleted"}, affect(r.tx, "DELETE FROM projects WHERE id=?", r.id())
	})
	s.route(m, "GET", projectPath+"/bom", read, func(r request) (any, error) {
		if _, e := get(r.tx, "projects", r.id()); e != nil {
			return nil, e
		}
		return projectBOM(r, r.id())
	})
	s.route(m, "POST", projectPath+"/{action}", write, func(r request) (any, error) {
		switch op := r.PathValue("action"); op {
		case "cover-image":
			return s.coverUpload(r)
		case "bom":
			return saveBOM(r, false)
		case "create-template":
			return cloneProject(r, true)
		case "add-archives", "add-queue", "remove-archives":
			return relink(r, op)
		default:
			return nil, notfound()
		}
	})
	s.route(m, "PATCH", projectPath+"/bom/{item}", write, func(r request) (any, error) { return saveBOM(r, true) })
	s.route(m, "DELETE", projectPath+"/bom/{item}", write, func(r request) (any, error) {
		return row{"status": "success"}, affect(r.tx, "DELETE FROM project_bom_items WHERE project_id=? AND id=?", r.id(), r.PathValue("item"))
	})
	for _, kind := range []string{"archives", "queue"} {
		s.route(m, "GET", projectPath+"/"+kind, read, func(r request) (any, error) { return projectMembers(r, kind) })
	}
	s.route(m, "GET", projectPath+"/file-progress", read, func(r request) (any, error) { return fileProgress(r) })
	s.route(m, "POST", "projects/from-template/{id}", "projects:create", func(r request) (any, error) { return cloneProject(r, false) })
	s.route(m, "GET", "archives/stats", "stats:read", archiveStats)
}
func saveProject(r request, id int64) (any, error) {
	model := "ProjectCreate"
	if id != 0 {
		model = "ProjectUpdate"
	}
	d, e := r.input(model)
	if e != nil {
		return nil, e
	}
	if p := intID(d["parent_id"]); p > 0 {
		if _, e = get(r.tx, "projects", p); e != nil {
			return nil, e
		}
		hit, e := count(r.tx, `WITH RECURSIVE ancestors(id) AS(VALUES(?) UNION SELECT p.parent_id FROM projects p JOIN ancestors a ON p.id=a.id WHERE p.parent_id IS NOT NULL) SELECT count(*) FROM ancestors WHERE id=?`, p, id)
		if e != nil {
			return nil, e
		}
		if hit > 0 {
			return nil, bad("A project cannot be its own ancestor")
		}
	}
	if id == 0 {
		d["is_template"] = false
		d["status"] = "active"
		id, e = insert(r.tx, "projects", d)
	} else {
		d["updated_at"] = now()
		e = update(r.tx, "projects", id, d)
	}
	if e != nil {
		return nil, e
	}
	return projectView(r, id)
}
func projectTotals(tx *sql.Tx, ids []any) (row, error) {
	filter := " IN (" + placeholders(len(ids)) + ")"
	v, e := one(tx, `SELECT count(*) total_archives,coalesce(sum(a.quantity),0) total_items,
 coalesce(sum(CASE WHEN l.status='completed' AND coalesce(l.user_verdict,'')!='reject' THEN a.quantity ELSE 0 END),0) completed_prints,
 coalesce(sum(CASE WHEN l.status IN ('failed','aborted') THEN 1 ELSE 0 END),0) failed_prints,
 round(coalesce(sum(l.duration_seconds),0)/3600.0,2) total_print_time_hours,round(coalesce(sum(l.filament_used_grams),0),2) total_filament_grams,
 round(coalesce(sum(l.cost),0),2) estimated_cost,round(coalesce(sum(l.energy_kwh),0),3) total_energy_kwh,round(coalesce(sum(l.energy_cost),0),3) total_energy_cost,
 round(coalesce(sum(l.wear_cost),0),3) total_wear_cost FROM print_log_entries l JOIN print_archives a ON a.id=l.archive_id
 WHERE a.deleted_at IS NULL AND l.completed_at IS NOT NULL AND a.project_id`+filter, ids...)
	if e != nil {
		return nil, e
	}
	for _, q := range []string{"SELECT count(*) bom_total_items,coalesce(sum(quantity_acquired>=quantity_needed),0) bom_completed_items,round(coalesce(sum(unit_price*quantity_needed),0),2) bom_cost FROM project_bom_items WHERE project_id", "SELECT coalesce(sum(status='pending'),0) queued_prints,coalesce(sum(status='printing'),0) in_progress_prints FROM print_queue WHERE project_id", "SELECT sum(target_count) plate_target,sum(target_parts_count) part_target FROM projects WHERE id"} {
		z, e := one(tx, q+filter, ids...)
		if e != nil {
			return nil, e
		}
		for k, x := range z {
			v[k] = x
		}
	}
	for _, t := range []struct{ goal, n, p, left string }{{"plate_target", "total_archives", "progress_percent", "remaining_prints"}, {"part_target", "completed_prints", "parts_progress_percent", "remaining_parts"}} {
		goal, n := num(v[t.goal]), num(v[t.n])
		v[t.p], v[t.left] = nil, nil
		if goal > 0 {
			v[t.p], v[t.left] = math.Round(n/goal*1000)/10, math.Max(0, goal-n)
		}
		delete(v, t.goal)
	}
	return v, nil
}
func projectSummary(r request, p row) error {
	st, e := projectTotals(r.tx, []any{p["id"]})
	if e != nil {
		return e
	}
	p["archive_count"] = st["total_archives"]
	p["completed_count"] = st["completed_prints"]
	p["total_items"] = st["total_items"]
	p["failed_count"] = st["failed_prints"]
	p["progress_percent"] = st["progress_percent"]
	p["queue_count"] = num(st["queued_prints"]) + num(st["in_progress_prints"])
	p["child_count"], e = count(r.tx, "SELECT count(*) FROM projects WHERE parent_id=?", p["id"])
	p["archives"] = []row{}
	return e
}
func projectView(r request, id int64) (row, error) {
	p, e := get(r.tx, "projects", id)
	if e != nil {
		return nil, e
	}
	st, e := projectTotals(r.tx, []any{id})
	if e != nil {
		return nil, e
	}
	p["stats"] = st
	children, e := list(r.tx, "SELECT id,name,color,status FROM projects WHERE parent_id=? ORDER BY name,id", id)
	if e != nil {
		return nil, e
	}
	for _, c := range children {
		if e = projectSummary(r, c); e != nil {
			return nil, e
		}
	}
	p["children"] = children
	p["rollup_stats"] = nil
	p["descendant_count"] = 0
	if len(children) > 0 {
		tree, e := list(r.tx, `WITH RECURSIVE tree(id) AS(VALUES(?) UNION SELECT p.id FROM projects p JOIN tree t ON p.parent_id=t.id) SELECT id FROM tree`, id)
		if e != nil {
			return nil, e
		}
		ids := []any{}
		for _, c := range tree {
			ids = append(ids, c["id"])
		}
		p["descendant_count"] = len(ids) - 1
		p["rollup_stats"], e = projectTotals(r.tx, ids)
		if e != nil {
			return nil, e
		}
	}
	if p["parent_id"] != nil {
		par, e := one(r.tx, "SELECT name FROM projects WHERE id=?", p["parent_id"])
		if e != nil {
			return nil, e
		}
		p["parent_name"] = par["name"]
	}
	return p, nil
}
func projectBOM(r request, id int64) ([]row, error) {
	v, e := list(r.tx, "SELECT * FROM project_bom_items WHERE project_id=? ORDER BY sort_order,id", id)
	if e != nil {
		return nil, e
	}
	for _, b := range v {
		b["is_complete"] = num(b["quantity_acquired"]) >= num(b["quantity_needed"])
		b["archive_name"] = nil
	}
	return v, nil
}
func saveBOM(r request, patch bool) (any, error) {
	model := "BOMItemCreate"
	if patch {
		model = "BOMItemUpdate"
	}
	d, e := r.input(model)
	if e != nil {
		return nil, e
	}
	for _, k := range []string{"quantity_needed", "quantity_acquired", "unit_price"} {
		if num(d[k]) < 0 {
			return nil, invalid(k + " cannot be negative")
		}
	}
	var id int64
	if !patch {
		d["project_id"] = r.id()
		d["quantity_acquired"] = 0
		sort, e := count(r.tx, "SELECT coalesce(max(sort_order)+1,0) FROM project_bom_items WHERE project_id=?", r.id())
		if e != nil {
			return nil, e
		}
		d["sort_order"] = sort
		id, e = insert(r.tx, "project_bom_items", d)
		if e != nil {
			return nil, e
		}
	} else {
		b, e := one(r.tx, "SELECT id FROM project_bom_items WHERE project_id=? AND id=?", r.id(), r.PathValue("item"))
		if e != nil {
			return nil, e
		}
		id = intID(b["id"])
		d["updated_at"] = now()
		if e = update(r.tx, "project_bom_items", id, d); e != nil {
			return nil, e
		}
	}
	b, e := get(r.tx, "project_bom_items", id)
	if e != nil {
		return nil, e
	}
	b["is_complete"] = num(b["quantity_acquired"]) >= num(b["quantity_needed"])
	b["archive_name"] = nil
	return b, nil
}
func cloneProject(r request, toTemplate bool) (any, error) {
	p, e := get(r.tx, "projects", r.id())
	if e != nil {
		return nil, e
	}
	if !toTemplate && !truth(p["is_template"]) {
		return nil, bad("Source is not a template")
	}
	d := obj(p, "name description color target_count target_parts_count target_sets notes tags priority budget url")
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name != "" {
		d["name"] = name
	} else if toTemplate {
		d["name"] = str(d["name"]) + " (Template)"
	}
	d["is_template"] = toTemplate
	d["status"] = "active"
	if !toTemplate {
		d["template_source_id"] = r.id()
	}
	id, e := insert(r.tx, "projects", d)
	if e != nil {
		return nil, e
	}
	items, e := projectBOM(r, r.id())
	if e != nil {
		return nil, e
	}
	for _, item := range items {
		d := obj(item, "name quantity_needed unit_price sourcing_url stl_filename remarks sort_order")
		d["quantity_acquired"] = 0
		d["project_id"] = id
		if _, e = insert(r.tx, "project_bom_items", d); e != nil {
			return nil, e
		}
	}
	return projectView(r, id)
}
func visibleMember(r request, k string, v row, write bool) bool {
	a := access.From(r.Context())
	pid := intID(v["printer_id"])
	if a.PrinterIDs != nil && !a.Admin {
		if k == "archives" {
			runs, e := list(r.tx, "SELECT DISTINCT printer_id FROM print_log_entries WHERE archive_id=? AND printer_id IS NOT NULL", v["id"])
			if e != nil {
				return false
			}
			anyAllowed, anyDenied := false, false
			for _, run := range runs {
				if a.AllowsPrinter(intID(run["printer_id"])) {
					anyAllowed = true
				} else {
					anyDenied = true
				}
			}
			if write && (anyDenied || pid != 0 && !a.AllowsPrinter(pid)) {
				return false
			}
			if !write && !(anyAllowed || pid != 0 && a.AllowsPrinter(pid) || pid == 0 && len(runs) == 0) {
				return false
			}
		} else if pid != 0 && !a.AllowsPrinter(pid) {
			return false
		}
	}
	action := "read"
	if write {
		action = "update"
	}
	return a.Has(k+":"+action+"_all") || (!write && a.Has(k+":read") && !a.Has(k+":read_own")) || a.Has(k+":"+action+"_own") && a.ID > 0 && intID(v["created_by_id"]) == a.ID
}
func projectMembers(r request, k string) (any, error) {
	if _, e := get(r.tx, "projects", r.id()); e != nil {
		return nil, e
	}
	t := "print_queue"
	filter := ""
	if k == "archives" {
		t = "print_archives"
		filter = " AND deleted_at IS NULL"
	}
	v, e := list(r.tx, "SELECT * FROM "+t+" WHERE project_id=?"+filter+" ORDER BY created_at DESC", r.id())
	if e != nil {
		return nil, e
	}
	out := []row{}
	for _, x := range v {
		if visibleMember(r, k, x, false) {
			if k == "archives" {
				x = obj(x, "id printer_id project_id library_file_id filename file_size print_name thumbnail_path print_time_seconds filament_used_grams filament_type status started_at completed_at plate_id is_favorite tags notes cost created_at created_by_id quantity")
			}
			out = append(out, x)
		}
	}
	return out, nil
}
func relink(r request, method string) (any, error) {
	if _, e := get(r.tx, "projects", r.id()); e != nil {
		return nil, e
	}
	d, e := r.body()
	if e != nil {
		return nil, e
	}
	k, t, key := "archives", "print_archives", "archive_ids"
	if method == "add-queue" {
		k, t, key = "queue", "print_queue", "queue_item_ids"
	}
	ids, ok := d[key].([]any)
	if !ok {
		return nil, invalid("Expected " + key)
	}
	n := 0
	for _, x := range ids {
		v, e := get(r.tx, t, intID(x))
		if e != nil {
			return nil, e
		}
		if !visibleMember(r, k, v, true) {
			return nil, notfound()
		}
		if k == "archives" && access.From(r.Context()).PrinterIDs != nil {
			runs, e := list(r.tx, "SELECT DISTINCT printer_id FROM print_log_entries WHERE archive_id=?", v["id"])
			if e != nil {
				return nil, e
			}
			for _, run := range runs {
				if !access.From(r.Context()).AllowsPrinter(intID(run["printer_id"])) {
					return nil, notfound()
				}
			}
		}
		var id any = r.id()
		if method == "remove-archives" {
			if intID(v["project_id"]) != r.id() {
				continue
			}
			id = nil
		}
		if e = update(r.tx, t, intID(x), row{"project_id": id}); e != nil {
			return nil, e
		}
		n++
	}
	return row{"message": fmt.Sprintf("Updated %d records", n), "count": n}, nil
}
func fileProgress(r request) (any, error) {
	if _, e := get(r.tx, "projects", r.id()); e != nil {
		return nil, e
	}
	files, e := list(r.tx, `SELECT f.id,f.file_hash,f.filename,f.created_by_id,d.shared FROM library_files f JOIN library_folders d ON f.folder_id=d.id WHERE d.project_id=? AND f.deleted_at IS NULL ORDER BY f.id`, r.id())
	if e != nil {
		return nil, e
	}
	actor := access.From(r.Context())
	visible := []row{}
	for _, f := range files {
		if actor.Has("library:read_all") || actor.Has("library:read_own") && (truth(f["shared"]) || actor.ID > 0 && intID(f["created_by_id"]) == actor.ID) {
			visible = append(visible, f)
		}
	}
	files = visible
	ids, hash, name := map[int64]int{}, map[string]int64{}, map[string]int64{}
	for _, f := range files {
		id := intID(f["id"])
		ids[id] = 0
		if h := str(f["file_hash"]); h != "" && hash[h] == 0 {
			hash[h] = id
		}
		if name[str(f["filename"])] == 0 {
			name[str(f["filename"])] = id
		}
	}
	runs, e := list(r.tx, `SELECT a.library_file_id,a.content_hash,a.filename,count(*) n FROM print_archives a JOIN print_log_entries l ON a.id=l.archive_id WHERE a.project_id=? AND a.deleted_at IS NULL AND l.status='completed' AND l.completed_at IS NOT NULL AND coalesce(l.user_verdict,'')!='reject' GROUP BY a.id`, r.id())
	if e != nil {
		return nil, e
	}
	for _, v := range runs {
		id := intID(v["library_file_id"])
		if _, in := ids[id]; !in {
			id = hash[str(v["content_hash"])]
			if id == 0 {
				id = name[str(v["filename"])]
			}
		}
		if id > 0 {
			ids[id] += int(num(v["n"]))
		}
	}
	out := []row{}
	for _, f := range files {
		id := intID(f["id"])
		if ids[id] > 0 {
			out = append(out, row{"file_id": id, "completed_count": ids[id]})
		}
	}
	return out, nil
}
