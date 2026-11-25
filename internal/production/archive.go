package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
)

func (s *Service) archiveRoutes(rt register) {
	s.archiveReports(rt)
	for _, kind := range []string{"archives", "library"} {
		base := "/archives"
		if kind == "library" {
			base = "/library/files"
		}
		for _, a := range []string{"gcode", "filament-requirements", "capabilities"} {
			rt("GET", base+"/{id}/"+a, s.inspectView(kind, a))
		}
		rt("POST", base+"/{id}/photos", s.attachment(kind, "photos"))
		for _, m := range []string{"GET", "DELETE"} {
			rt(m, base+"/{id}/photos/{filename}", s.attachment(kind, "photos"))
		}
	}
	rt("GET", "/archives/{id}/runs/{run}/confirmation", s.confirmationLinks)
	rt("GET", "/archives/prints", s.archiveRuns)
	rt("GET", "/archives/{id}/runs", s.archiveRuns)
	rt("GET", "/archives/slim", s.assetList("archives"))
	rt("GET", "/archives/tags", s.archiveTags)
	rt("PUT", "/archives/tags/{tag}", s.archiveTags)
	rt("DELETE", "/archives/{id}/{sub}", func(w http.ResponseWriter, r *http.Request) (any, error) {
		sub := r.PathValue("sub")
		if r.PathValue("id") == "tags" {
			r.SetPathValue("tag", sub)
			return s.archiveTags(w, r)
		}
		if sub == "source" || sub == "f3d" {
			return s.attachment("archives", sub)(w, r)
		}
		return nil, sql.ErrNoRows
	})
	rt("POST", "/archives/{id}/favorite", s.favorite)
	rt("GET", "/archives/{id}/duplicates", s.similar(false))
	rt("GET", "/archives/{id}/similar", s.similar(true))
	rt("GET", "/archives/compare", s.compareArchives)
	rt("POST", "/archives/{id}/reprint", func(http.ResponseWriter, *http.Request) (any, error) {
		return nil, fail(410, "Submit a print with POST /api/v1/queue/")
	})
	for _, kind := range []string{"source", "f3d"} {
		for _, m := range []string{"GET", "POST"} {
			rt(m, "/archives/{id}/"+kind, s.attachment("archives", kind))
		}
	}
	rt("GET", "/archives/{id}/source/{filename}", s.attachment("archives", "source"))
}
func (s *Service) presentAsset(ctx context.Context, d database, p access.Principal, kind string, v row) error {
	if kind == "library" {
		tags, e := records(ctx, d, "SELECT t.id,t.name FROM library_tags t JOIN library_file_tags ft ON ft.tag_id=t.id WHERE ft.file_id=? ORDER BY name_key", v["id"])
		v["tags"] = tags
		return e
	}
	runs, e := records(ctx, d, "SELECT * FROM print_log_entries WHERE archive_id=? ORDER BY coalesce(started_at,completed_at,created_at) DESC,id DESC", v["id"])
	if e != nil {
		return e
	}
	visible := []row{}
	for _, run := range runs {
		if scope(p, num(run["printer_id"])) && owned(p, "archives", "read", num(run["created_by_id"])) {
			visible = append(visible, run)
		}
	}
	v["print_count"] = len(visible)
	v["run_count"] = len(visible)
	if len(runs) > 0 && (len(visible) == 0 || num(visible[0]["id"]) != num(runs[0]["id"]) || !scope(p, num(v["printer_id"]))) {
		var latest row
		if len(visible) > 0 {
			latest = visible[0]
		}
		for _, field := range []string{"printer_id", "started_at", "completed_at", "subtask_id", "billing_run_id", "plate_id", "user_verdict", "user_verdict_at", "user_verdict_source", "cost", "energy_kwh", "energy_cost", "wear_cost", "photos", "timelapse_path", "failure_reason", "status"} {
			v[field] = latest[field]
		}
		if latest == nil {
			v["status"] = "archived"
		}
	}
	if len(visible) < len(runs) {
		if len(visible) > 0 {
			v["photos"] = visible[0]["photos"]
		} else {
			v["photos"] = nil
		}
	}
	return nil
}
func (s *Service) archiveRuns(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("archives:read_all") && !p.Has("archives:read_own") {
		return nil, fail(403, "Archive read permission required")
	}
	q := "SELECT l.* FROM print_log_entries l JOIN print_archives a ON l.archive_id=a.id WHERE a.deleted_at IS NULL"
	args := []any{}
	qs := r.URL.Query()
	if k := pathID(r); k != 0 {
		archive, e := s.getAsset(ctx, s.db, p, "archives", k, "read")
		if e != nil {
			return nil, e
		}
		q += " AND a.id=?"
		args = append(args, archive["id"])
	} else {
		q += " AND l.completed_at IS NOT NULL"
	}
	for _, k := range []string{"date_from", "date_to"} {
		if date := qs.Get(k); date != "" {
			op := ">="
			if k == "date_to" {
				op = "<="
			}
			q += " AND date(l.completed_at)" + op + "date(?)"
			args = append(args, date)
		}
	}
	if k := num(qs.Get("printer_id")); k != 0 {
		q += " AND l.printer_id=?"
		args = append(args, k)
	}
	q += " ORDER BY coalesce(l.completed_at,l.started_at) DESC,l.id DESC"
	rs, e := records(ctx, s.db, q, args...)
	if e != nil {
		return nil, e
	}
	out := []row{}
	for _, v := range rs {
		if scope(p, num(v["printer_id"])) && owned(p, "archives", "read", num(v["created_by_id"])) {
			delete(v, "confirm_token")
			delete(v, "timelapse_baseline")
			out = append(out, v)
		}
	}
	total := int64(len(out))
	off := min(max(num(qs.Get("offset")), 0), total)
	limit := num(qs.Get("limit"))
	if limit < 1 {
		limit = 500
	}
	return row{"items": out[off:min(off+min(limit, 1000), total)], "total": total, "deleted_archive_ids": []int{}}, nil
}
func (s *Service) favorite(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	k := pathID(r)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = s.getAsset(ctx, tx, p, "archives", k, "update"); e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET is_favorite=NOT is_favorite WHERE id=?", k); e != nil {
		return nil, e
	}
	v, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	decodeAsset(v)
	return v, tx.Commit()
}
func (s *Service) archiveTags(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !p.Has("archives:read_all") && !p.Has("archives:read_own") {
		return nil, fail(403, "Archive read permission required")
	}
	method := r.Method
	if method != "GET" && !p.Has("archives:update_all") {
		return nil, fail(403, "Cannot change shared tags")
	}
	name := r.PathValue("tag")
	next := ""
	if method == "PUT" {
		m, e := input(w, r)
		if e != nil {
			return nil, e
		}
		next = strings.TrimSpace(val(m["new_name"]))
		if len(next) > 100 || next == "" || strings.Contains(next, ",") {
			return nil, fail(422, "Invalid tag name")
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := records(ctx, tx, "SELECT * FROM print_archives WHERE deleted_at IS NULL AND tags IS NOT NULL")
	if e != nil {
		return nil, e
	}
	counts := map[string]int{}
	affected := 0
	for _, v := range rows {
		action := "read"
		if method != "GET" {
			action = "update"
		}
		if !s.accessible(ctx, tx, p, "archives", v, action) {
			continue
		}
		seen := map[string]bool{}
		tags := []string{}
		match := false
		for _, x := range strings.Split(val(v["tags"]), ",") {
			x = strings.TrimSpace(x)
			if method != "GET" && x == name {
				match = true
				x = next
			}
			if x == "" || seen[x] {
				continue
			}
			seen[x] = true
			counts[x]++
			tags = append(tags, x)
		}
		if match {
			if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET tags=? WHERE id=?", strings.Join(tags, ","), v["id"]); e != nil {
				return nil, e
			}
			affected++
		}
	}
	if method != "GET" {
		return row{"affected": affected}, tx.Commit()
	}
	out := []row{}
	for tag, n := range counts {
		out = append(out, row{"name": tag, "count": n})
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a["count"] == b["count"] {
			return val(a["name"]) < val(b["name"])
		}
		return num(a["count"]) > num(b["count"])
	})
	return out, nil
}
func (s *Service) similar(name bool) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		v, e := s.getAsset(ctx, s.db, p, "archives", pathID(r), "read")
		if e != nil {
			return nil, e
		}
		field := "content_hash"
		if name {
			field = "print_name"
		}
		if val(v[field]) == "" {
			return []row{}, nil
		}
		rs, e := records(ctx, s.db, "SELECT * FROM print_archives WHERE "+field+"=? AND id!=? AND deleted_at IS NULL ORDER BY id", v[field], v["id"])
		if e != nil {
			return nil, e
		}
		out := []row{}
		for _, x := range rs {
			if s.accessible(ctx, s.db, p, "archives", x, "read") {
				if e = s.presentAsset(ctx, s.db, p, "archives", x); e != nil {
					return nil, e
				}
				decodeAsset(x)
				out = append(out, x)
			}
		}
		return out, nil
	}
}
func (s *Service) compareArchives(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	ids := strings.Split(r.URL.Query().Get("archive_ids"), ",")
	if len(ids) < 2 || len(ids) > 5 {
		return nil, fail(422, "Choose two to five archives")
	}
	list := []row{}
	seen := map[int64]bool{}
	for _, x := range ids {
		k := num(strings.TrimSpace(x))
		if seen[k] || k == 0 {
			return nil, fail(422, "Invalid archive IDs")
		}
		seen[k] = true
		v, e := s.getAsset(ctx, s.db, p, "archives", k, "read")
		if e != nil {
			return nil, e
		}
		if e = s.presentAsset(ctx, s.db, p, "archives", v); e != nil {
			return nil, e
		}
		decodeAsset(v)
		list = append(list, v)
	}
	fields := []row{}
	diff := []row{}
	for _, field := range []string{"status", "sliced_for_model", "filament_type", "filament_color", "layer_height", "nozzle_diameter", "bed_temperature", "nozzle_temperature", "print_time_seconds", "filament_used_grams", "cost"} {
		vals := []any{}
		different := false
		for _, v := range list {
			if val(v[field]) != val(list[0][field]) {
				different = true
			}
			vals = append(vals, v[field])
		}
		d := row{"field": field, "label": strings.ReplaceAll(field, "_", " "), "values": vals, "raw_values": vals, "has_difference": different}
		fields = append(fields, d)
		if different {
			diff = append(diff, d)
		}
	}
	return row{"archives": list, "comparison": fields, "differences": diff, "success_correlation": row{"insights": []string{}}}, nil
}
func (s *Service) inspectView(kind, a string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		v, e := s.getAsset(r.Context(), s.db, access.From(r.Context()), kind, pathID(r), "read")
		if e != nil {
			return nil, e
		}
		inf, e := s.meta(kind, v)
		if e != nil {
			return nil, e
		}
		want := max(num(r.URL.Query().Get("plate_id")), num(r.URL.Query().Get("plate")))
		items := []row{}
		used := map[int64]bool{}
		colors := []string{}
		pick := int64(0)
		for _, pl := range inf.Plates {
			if want != 0 && want != pl.Index {
				continue
			}
			if pick == 0 && pl.Printable {
				pick = pl.Index
			}
			for _, f := range pl.Filaments {
				slot := num(f["id"])
				if used[slot] {
					continue
				}
				used[slot] = true
				item := row{"slot_id": slot, "type": f["type"], "color": f["color"], "used_grams": f["used_g"], "tray_info_idx": f["tray_info_idx"]}
				items = append(items, item)
				colors = append(colors, val(f["color"]))
			}
		}
		if a == "filament-requirements" {
			if s.preview != nil && (len(items) == 0 || r.URL.Query().Get("full_slots") == "true") {
				slots, err := s.preview(r.Context(), access.From(r.Context()), kind, num(v["id"]), int(max(1, want)), r.URL.Query().Get("request_id"), r.URL.Query().Get("full_slots") == "true")
				if err == nil && slots != nil {
					items = []row{}
					for _, slot := range slots {
						items = append(items, row(slot))
					}
				}
			}
			return row{"archive_id": v["id"], "file_id": v["id"], "filename": v["filename"], "plate_id": id(want), "filaments": items}, nil
		}
		path, e := s.assetPath(v["file_path"])
		if e != nil {
			return nil, e
		}
		if strings.EqualFold(filepath.Ext(path), ".gcode") {
			if a == "capabilities" {
				return row{"has_model": false, "has_gcode": true, "has_source": false, "filament_colors": colors}, nil
			}
			w.Header().Set("Content-Type", "text/plain")
			http.ServeFile(w, r, path)
			return nil, nil
		}
		z, e := zip.OpenReader(path)
		if e != nil {
			return nil, fail(422, "3MF unavailable")
		}
		defer z.Close()
		model := false
		var gc *zip.File
		for _, f := range z.File {
			if strings.HasSuffix(f.Name, ".model") {
				model = true
			}
			if f.Name == fmt.Sprintf("Metadata/plate_%d.gcode", pick) {
				gc = f
			}
		}
		if a == "capabilities" {
			return row{"has_model": model, "has_gcode": inf.Printable, "has_source": val(v["source_3mf_path"]) != "", "filament_colors": colors}, nil
		}
		if gc == nil {
			return nil, sql.ErrNoRows
		}
		if gc.UncompressedSize64 > 1<<30 {
			return nil, fail(413, "G-code exceeds streaming limit")
		}
		rd, e := gc.Open()
		if e != nil {
			return nil, e
		}
		defer rd.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, e = io.Copy(w, io.LimitReader(rd, 1<<30))
		return nil, e
	}
}
