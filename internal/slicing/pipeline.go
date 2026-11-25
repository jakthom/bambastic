package slicing

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

// Farm provides live device state and transaction-scoped queue admission.
type Farm interface {
	InitialStatus(context.Context, access.Principal) ([]map[string]any, error)
	CreateCopies(context.Context, *sql.Tx, access.Principal, map[string]any) ([]int64, error)
}

func (s *Service) AttachFarm(f Farm) {
	s.farm = f
	if previews, ok := f.(interface {
		SetFilamentPreview(func(context.Context, access.Principal, string, int64, int, string, bool) ([]map[string]any, error))
	}); ok {
		previews.SetFilamentPreview(s.previewFilaments)
	}
}
func recoverRuns(db *sql.DB) error {
	// Terminal state must survive trimming a queue entry. The foreign key clears its ID.
	_, e := db.Exec(`CREATE TRIGGER IF NOT EXISTS bb_pipeline_queue_status AFTER UPDATE OF status ON print_queue
 BEGIN UPDATE pipeline_jobs SET status=CASE NEW.status WHEN 'completed' THEN 'completed' WHEN 'failed' THEN 'failed' WHEN 'cancelled' THEN 'cancelled' WHEN 'printing' THEN 'printing' ELSE status END,completed_at=CASE WHEN NEW.status IN ('completed','failed','cancelled') THEN CURRENT_TIMESTAMP ELSE completed_at END WHERE queue_entry_id=NEW.id AND status NOT IN ('completed','cancelled','failed'); END;
 CREATE TRIGGER IF NOT EXISTS bb_pipeline_queue_delete BEFORE DELETE ON print_queue BEGIN UPDATE pipeline_jobs SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE queue_entry_id=OLD.id AND status NOT IN ('completed','cancelled','failed'); END;
 UPDATE pipeline_jobs SET status='failed',completed_at=CURRENT_TIMESTAMP,error_message='Service restarted during slicing' WHERE pipeline_run_id IN(SELECT id FROM pipeline_runs WHERE status IN ('queued','slicing','dispatching')) AND queue_entry_id IS NULL AND status='pending';
 UPDATE pipeline_runs SET status='failed',completed_at=CURRENT_TIMESTAMP,error_message='Service restarted during slicing' WHERE status IN ('queued','slicing','dispatching') AND NOT EXISTS(SELECT 1 FROM pipeline_jobs j WHERE j.pipeline_run_id=pipeline_runs.id AND j.queue_entry_id IS NOT NULL)`)
	return e
}
func (s *Service) pipelineRoutes(m *http.ServeMux) {
	const pipes = "/api/v1/slicer-pipelines"
	const runs = "/api/v1/pipeline-runs"
	m.HandleFunc("GET "+pipes+"/{$}", s.pipelineList)
	m.HandleFunc("POST "+pipes+"/{$}", s.writePipeline)
	m.HandleFunc("GET "+pipes+"/{id}", s.pipelineRead)
	m.HandleFunc("PUT "+pipes+"/{id}", s.writePipeline)
	m.HandleFunc("DELETE "+pipes+"/{id}", s.removePipeline)
	m.HandleFunc("POST "+pipes+"/{id}/check-eligibility", s.launch)
	m.HandleFunc("POST "+pipes+"/{id}/run", s.launch)
	m.HandleFunc("GET "+pipes+"/{id}/runs", s.listRuns)
	m.HandleFunc("GET "+runs, s.listRuns)
	m.HandleFunc("POST "+runs+"/clear", s.clearRuns)
	m.HandleFunc("GET "+runs+"/{id}", s.readRun)
	m.HandleFunc("POST "+runs+"/{id}/cancel", s.cancelRun)
	m.HandleFunc("POST "+runs+"/{id}/retry-failed", s.retryRun)
	m.HandleFunc("GET /api/v1/slicer/loaded-spools", s.loadedSpools)
}
func number(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	case float64:
		return int64(n)
	case string:
		a, _ := strconv.ParseInt(n, 10, 64)
		return a
	}
	return 0
}
func nullable(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func routeID(r *http.Request) int64 { return number(r.PathValue("id")) }
func scoped(p access.Principal, v object) bool {
	k := number(v["target_printer_id"])
	return k == 0 || p.AllowsPrinter(k)
}
func formatPipeline(v object) object {
	v["printer_preset"] = presetRef{httpx.Str(v["printer_preset_source"]), httpx.Str(v["printer_preset_id"])}
	v["process_preset"] = presetRef{httpx.Str(v["process_preset_source"]), httpx.Str(v["process_preset_id"])}
	var fil []presetRef
	_ = json.Unmarshal([]byte(httpx.Str(v["filament_presets_json"])), &fil)
	v["filament_presets"] = fil
	for _, k := range []string{"printer_preset_source", "printer_preset_id", "process_preset_source", "process_preset_id", "filament_presets_json", "is_deleted"} {
		delete(v, k)
	}
	return v
}
func (s *Service) pipeline(ctx context.Context, p access.Principal, id int64) (object, error) {
	a, e := rows(s.db, ctx, "SELECT * FROM slicer_pipelines WHERE id=? AND NOT is_deleted", id)
	if e != nil {
		return nil, e
	}
	if len(a) == 0 || !scoped(p, a[0]) {
		return nil, sql.ErrNoRows
	}
	return formatPipeline(a[0]), nil
}
func pipelineErr(w http.ResponseWriter, e error) {
	if errors.Is(e, sql.ErrNoRows) {
		httpx.Error(w, 404, "Pipeline or run not found")
		return
	}
	httpx.Error(w, 500, "Could not access pipeline storage")
}
func (s *Service) pipelineList(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:read") {
		return
	}
	a, e := rows(s.db, r.Context(), "SELECT * FROM slicer_pipelines WHERE NOT is_deleted ORDER BY lower(name),id")
	if e != nil {
		pipelineErr(w, e)
		return
	}
	out := []object{}
	for _, v := range a {
		if scoped(access.From(r.Context()), v) {
			out = append(out, formatPipeline(v))
		}
	}
	httpx.JSON(w, 200, object{"pipelines": out})
}
func (s *Service) pipelineRead(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:read") {
		return
	}
	o, e := s.pipeline(r.Context(), access.From(r.Context()), routeID(r))
	if e != nil {
		pipelineErr(w, e)
		return
	}
	httpx.JSON(w, 200, o)
}
func (s *Service) removePipeline(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:write") {
		return
	}
	id := routeID(r)
	if _, e := s.pipeline(r.Context(), access.From(r.Context()), id); e != nil {
		pipelineErr(w, e)
		return
	}
	_, e := s.db.ExecContext(r.Context(), "UPDATE slicer_pipelines SET is_deleted=1,updated_at=CURRENT_TIMESTAMP WHERE id=?", id)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	w.WriteHeader(204)
}
func validRef(v presetRef) bool {
	return v.ID != "" && len(v.ID) <= 200 && (v.Source == "local" || v.Source == "standard" || v.Source == "cloud" || v.Source == "orca_cloud")
}
func (s *Service) writePipeline(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:write") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	id := routeID(r)
	var err error
	obj := object{"target_kind": "printer_class", "fanout_strategy": "max_parallel"}
	if r.Method == "PUT" {
		obj, err = s.pipeline(ctx, p, id)
		if err != nil {
			pipelineErr(w, err)
			return
		}
	}
	var changes object
	if !httpx.Decode(w, r, &changes) {
		return
	}
	for k, v := range changes {
		obj[k] = v
	}
	name := strings.TrimSpace(httpx.Str(obj["name"]))
	var req sliceRequest
	b, _ := json.Marshal(obj)
	_ = json.Unmarshal(b, &req)
	kind := httpx.Str(obj["target_kind"])
	strategy := httpx.Str(obj["fanout_strategy"])
	if name == "" || len(name) > 200 || !validRef(req.Printer) || !validRef(req.Process) || len(req.Filaments) < 1 || len(req.Filaments) > 64 || req.Bed != "" && bedNames[req.Bed] == "" ||
		(kind != "specific_printer" && kind != "printer_class") || (strategy != "max_parallel" && strategy != "round_robin" && strategy != "fill_one_first") {
		httpx.Error(w, 422, "Invalid pipeline recipe, target, or fanout strategy")
		return
	}
	for _, ref := range req.Filaments {
		if !validRef(ref) {
			httpx.Error(w, 422, "Invalid filament preset")
			return
		}
	}
	target := number(obj["target_printer_id"])
	if target != 0 {
		var k int
		if !p.AllowsPrinter(target) || s.db.QueryRowContext(ctx, "SELECT 1 FROM printers WHERE id=?", target).Scan(&k) != nil {
			httpx.Error(w, 404, "Printer not found")
			return
		}
	}
	fil, _ := json.Marshal(req.Filaments)
	params := []any{name, blankNull(httpx.Str(obj["description"])), req.Printer.Source, req.Printer.ID, req.Process.Source, req.Process.ID, string(fil), blankNull(bedNames[req.Bed]), kind, nullable(target), blankNull(httpx.Str(obj["target_model_class"])), strategy}
	if r.Method == "POST" {
		res, e := s.db.ExecContext(ctx, `INSERT INTO slicer_pipelines(name,description,printer_preset_source,printer_preset_id,process_preset_source,process_preset_id,filament_presets_json,bed_type,target_kind,target_printer_id,target_model_class,fanout_strategy,created_by) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, append(params, nullable(p.ID))...)
		if e != nil {
			pipelineErr(w, e)
			return
		}
		id, _ = res.LastInsertId()
	} else {
		_, e := s.db.ExecContext(ctx, `UPDATE slicer_pipelines SET name=?,description=?,printer_preset_source=?,printer_preset_id=?,process_preset_source=?,process_preset_id=?,filament_presets_json=?,bed_type=?,target_kind=?,target_printer_id=?,target_model_class=?,fanout_strategy=?,updated_at=CURRENT_TIMESTAMP WHERE id=?`, append(params, id)...)
		if e != nil {
			pipelineErr(w, e)
			return
		}
	}
	o, e := s.pipeline(ctx, p, id)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	status := 200
	if r.Method == "POST" {
		status = 201
	}
	httpx.JSON(w, status, o)
}

type runRequest struct {
	Library int64 `json:"source_library_file_id"`
	Archive int64 `json:"source_archive_id"`
	Copies  int   `json:"copies"`
	Force   bool  `json:"force"`
}

func (s *Service) status(ctx context.Context, p access.Principal) (map[int64]object, error) {
	m := map[int64]object{}
	if s.farm == nil {
		return m, errors.New("Printer service is unavailable")
	}
	a, e := s.farm.InitialStatus(ctx, p)
	for _, v := range a {
		m[number(v["id"])] = v
	}
	return m, e
}
func objects(v any) []object {
	switch a := v.(type) {
	case []object:
		return a
	case []any:
		out := []object{}
		for _, item := range a {
			if o, ok := item.(object); ok {
				out = append(out, o)
			}
		}
		return out
	}
	return []object{}
}
func trays(state object) map[int]object {
	out := map[int]object{}
	for _, a := range objects(state["ams"]) {
		for _, tray := range objects(a["tray"]) {
			idx := int(number(a["id"])*4 + number(tray["id"]))
			out[idx] = tray
		}
	}
	return out
}
func (s *Service) eligibility(ctx context.Context, p access.Principal, pipe object) (object, []int64, error) {
	states, e := s.status(ctx, p)
	if e != nil {
		return nil, nil, e
	}
	printers, e := rows(s.db, ctx, "SELECT id,name,model,is_active FROM printers ORDER BY id")
	if e != nil {
		return nil, nil, e
	}
	kind := httpx.Str(pipe["target_kind"])
	target := number(pipe["target_printer_id"])
	model := httpx.Str(pipe["target_model_class"])
	report := object{"ok": false, "target_kind": kind, "target_printer_id": nullable(target), "target_model_class": blankNull(model), "printer_reports": []object{}, "issues": []object{}}
	specific := kind == "specific_printer"
	if specific && target == 0 || !specific && model == "" {
		issue := "class_not_set"
		if specific {
			issue = "printer_not_set"
		}
		report["issues"] = []object{{"kind": issue}}
		return report, nil, nil
	}
	refs, _ := pipe["filament_presets"].([]presetRef)
	var expected []object
	for _, f := range refs {
		o, _ := s.resolve(ctx, "filament", f)
		expected = append(expected, o)
	}
	matches := []object{}
	eligible := []int64{}
	for _, v := range printers {
		id := number(v["id"])
		if !p.AllowsPrinter(id) || specific && id != target || !specific && !strings.EqualFold(model, httpx.Str(v["model"])) {
			continue
		}
		issues := []object{}
		online := httpx.Bool(states[id]["connected"])
		ok := true
		if !httpx.Bool(v["is_active"]) {
			issues = append(issues, object{"kind": "printer_disabled"})
			ok = false
		}
		if !online {
			issues = append(issues, object{"kind": "printer_offline"})
			ok = false
		}
		if online {
			loaded := trays(states[id])
			for i, prof := range expected {
				material := asText(prof["filament_type"])
				if material == "" {
					issues = append(issues, object{"kind": "filament_unverified", "slot_index": i})
					continue
				}
				tray := loaded[i]
				got := httpx.Str(tray["tray_type"])
				if got == "" {
					ok = false
					issues = append(issues, object{"kind": "ams_slot_missing", "slot_index": i, "expected": material})
					continue
				}
				if !strings.EqualFold(got, material) {
					ok = false
					issues = append(issues, object{"kind": "filament_type_mismatch", "slot_index": i, "expected": material, "actual": got})
				}
				wantCol := strings.TrimPrefix(asText(prof["default_filament_colour"]), "#")
				gotCol := strings.TrimPrefix(httpx.Str(tray["tray_color"]), "#")
				if len(wantCol) >= 6 && len(gotCol) >= 6 && !strings.EqualFold(wantCol[:6], gotCol[:6]) {
					ok = false
					issues = append(issues, object{"kind": "filament_color_mismatch", "slot_index": i, "expected": wantCol, "actual": gotCol})
				}
			}
		}
		if ok {
			eligible = append(eligible, id)
		}
		matches = append(matches, object{"printer_id": id, "printer_name": v["name"], "ok": ok, "issues": issues})
		if specific {
			report["target_printer_name"] = v["name"]
			report["issues"] = issues
		}
	}
	if len(matches) == 0 {
		issue := "no_class_matches"
		if specific {
			issue = "printer_not_found"
		}
		report["issues"] = []object{{"kind": issue}}
	}
	if !specific {
		report["printer_reports"] = matches
	}
	report["ok"] = len(eligible) > 0
	return report, eligible, nil
}
func (s *Service) loadedSpools(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "library:upload") || !access.Require(w, r, "printers:read") {
		return
	}
	ctx := r.Context()
	m, e := s.status(ctx, access.From(ctx))
	if e != nil {
		httpx.Error(w, 503, e.Error())
		return
	}
	out := []object{}
	for id, st := range m {
		if !httpx.Bool(st["connected"]) {
			continue
		}
		var model string
		_ = s.db.QueryRowContext(ctx, "SELECT COALESCE(model,'') FROM printers WHERE id=?", id).Scan(&model)
		mappings, e := rows(s.db, ctx, "SELECT ams_id,tray_id,preset_id,preset_name,preset_source,tray_info_idx FROM slot_preset_mappings WHERE printer_id=?", id)
		if e != nil {
			pipelineErr(w, e)
			return
		}
		saved := map[[2]int64]object{}
		for _, v := range mappings {
			saved[[2]int64{number(v["ams_id"]), number(v["tray_id"])}] = object{"preset_id": v["preset_id"], "preset_name": v["preset_name"], "preset_source": v["preset_source"], "tray_info_idx": v["tray_info_idx"]}
		}
		trayView := func(a, n int64, tray object) object {
			return object{"ams_id": a, "tray_id": n, "tray_type": tray["tray_type"], "tray_color": tray["tray_color"], "tray_sub_brands": tray["tray_sub_brands"], "tray_info_idx": tray["tray_info_idx"], "exists": tray["exists"], "state": tray["state"], "saved_preset": saved[[2]int64{a, n}]}
		}
		units := []object{}
		for _, a := range objects(st["ams"]) {
			loaded := []object{}
			ams := number(a["id"])
			for _, tray := range objects(a["tray"]) {
				loaded = append(loaded, trayView(ams, number(tray["id"]), tray))
			}
			units = append(units, object{"id": ams, "is_ams_ht": ams >= 128 && ams <= 135, "trays": loaded})
		}
		holders := objects(st["vt_tray"])
		if len(holders) == 0 {
			if single, ok := st["vir_slot"].(object); ok {
				holders = []object{single}
			}
		}
		external := []object{}
		for _, holder := range holders {
			if httpx.Str(holder["tray_type"]) == "" {
				continue
			}
			idx := number(holder["id"])
			switch {
			case idx == 0 || idx == 254:
				idx = 0
			case idx == 255:
				idx = 1
			case idx >= 247 && idx <= 253:
				idx -= 245
			default:
				continue
			}
			external = append(external, trayView(255, idx, holder))
		}
		out = append(out, object{"id": id, "name": st["name"], "model": model, "ams": units, "external": external, "external_holders": len(holders)})
	}
	sort.Slice(out, func(i, j int) bool { return number(out[i]["id"]) < number(out[j]["id"]) })
	httpx.JSON(w, 200, object{"printers": out})
}
func (s *Service) launch(w http.ResponseWriter, r *http.Request) {
	check := strings.HasSuffix(r.URL.Path, "/check-eligibility")
	perm := "pipelines:run"
	if check {
		perm = "pipelines:read"
	}
	if !access.Require(w, r, perm) {
		return
	}
	var body = runRequest{Copies: 1}
	if !httpx.Decode(w, r, &body) {
		return
	}
	s.startRun(w, r, routeID(r), body, 0, check)
}
func (s *Service) startRun(w http.ResponseWriter, r *http.Request, pipelineID int64, body runRequest, parent int64, check bool) {
	ctx := r.Context()
	p := access.From(ctx)
	if (body.Library > 0) == (body.Archive > 0) {
		httpx.Error(w, 422, "Choose exactly one source")
		return
	}
	pipe, e := s.pipeline(ctx, p, pipelineID)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	kind := "library_file"
	sid := body.Library
	if body.Archive > 0 {
		kind = "archive"
		sid = body.Archive
	}
	src, code, e := s.sourceFor(ctx, p, kind, strconv.FormatInt(sid, 10))
	if e != nil {
		httpx.Error(w, code, e.Error())
		return
	}
	report, _, e := s.eligibility(ctx, p, pipe)
	if e != nil {
		httpx.Error(w, 503, e.Error())
		return
	}
	if check {
		httpx.JSON(w, 200, report)
		return
	}
	if !httpx.Bool(report["ok"]) && !body.Force {
		httpx.JSON(w, 409, object{"detail": report})
		return
	}
	if pipe["target_kind"] == "specific_printer" && number(pipe["target_printer_id"]) == 0 || pipe["target_kind"] == "printer_class" && httpx.Str(pipe["target_model_class"]) == "" {
		httpx.Error(w, 400, "Choose a target printer or printer class")
		return
	}
	limit, _ := access.Setting(ctx, s.db, "pipeline_max_copies", "50")
	cap, e := strconv.Atoi(limit)
	if e != nil || cap < 1 {
		cap = 50
	}
	if body.Copies < 1 || body.Copies > cap || body.Copies > 1000 {
		httpx.Error(w, 422, fmt.Sprintf("Choose between 1 and %d copies", min(cap, 1000)))
		return
	}
	if !p.Has("queue:create") {
		httpx.Error(w, 403, "Queue creation permission required")
		return
	}
	engine, base, e := s.endpoint(ctx)
	if e != nil {
		httpx.Error(w, 400, e.Error())
		return
	}
	ext := strings.ToLower(filepath.Ext(src.name))
	if ext != ".stl" && ext != ".3mf" {
		httpx.Error(w, 400, "Slice source must be STL or 3MF")
		return
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 500, "Cannot open storage")
		return
	}
	defer root.Close()
	file, e := root.Open(src.path)
	if e != nil {
		httpx.Error(w, 404, "Source file missing")
		return
	}
	req := sliceRequest{}
	b, _ := json.Marshal(pipe)
	_ = json.Unmarshal(b, &req)
	recipe, e := s.profiles(ctx, req, engine, ext, file)
	if e != nil {
		file.Close()
		httpx.Error(w, 422, e.Error())
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		file.Close()
		pipelineErr(w, e)
		return
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, `INSERT INTO pipeline_runs(pipeline_id,source_library_file_id,source_archive_id,copies,created_by,status,started_at,eligibility_overridden,parent_run_id) VALUES(?,?,?,?,?,'slicing',CURRENT_TIMESTAMP,?,?)`, pipelineID, nullable(body.Library), nullable(body.Archive), body.Copies, nullable(p.ID), !httpx.Bool(report["ok"]) && body.Force, nullable(parent))
	if e != nil {
		file.Close()
		pipelineErr(w, e)
		return
	}
	runID, _ := res.LastInsertId()
	for i := 0; i < body.Copies; i++ {
		_, e = tx.ExecContext(ctx, "INSERT INTO pipeline_jobs(pipeline_run_id,copy_index,status) VALUES(?,?,'pending')", runID, i)
		if e != nil {
			file.Close()
			pipelineErr(w, e)
			return
		}
	}
	if e = tx.Commit(); e != nil {
		file.Close()
		pipelineErr(w, e)
		return
	}
	token := access.Credential(r)
	j, e := s.startJob(p, src, kind, true, req, recipe, base, file, func(result object, err error) {
		if err == nil {
			err = s.dispatchRun(runID, pipe, body, number(result["library_file_id"]), p, token)
		}
		if err != nil {
			s.failRun(runID, err)
		}
	})
	if e != nil {
		s.failRun(runID, e)
	} else {
		_, _ = s.db.Exec("UPDATE pipeline_runs SET slice_job_id=? WHERE id=?", j.id, runID)
	}
	out, e := s.run(ctx, p, runID)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	httpx.JSON(w, 202, out)
}
func (s *Service) dispatchRun(run int64, pipe object, b runRequest, file int64, original access.Principal, token string) error {
	ctx := s.ctx
	p, e := (&access.Service{DB: s.db}).Resolve(ctx, token, "session")
	if e != nil || original.ID != p.ID || !p.Has("pipelines:run") || !p.Has("queue:create") {
		return errors.New("Pipeline authority changed before dispatch")
	}
	report, candidates, e := s.eligibility(ctx, p, pipe)
	if e != nil {
		return e
	}
	if !httpx.Bool(report["ok"]) && !b.Force {
		return errors.New("Target eligibility changed before dispatch")
	}
	if len(candidates) == 0 && b.Force {
		if id := number(pipe["target_printer_id"]); pipe["target_kind"] == "specific_printer" && id > 0 && p.AllowsPrinter(id) {
			candidates = append(candidates, id)
		} else {
			for _, v := range objects(report["printer_reports"]) {
				candidates = append(candidates, number(v["printer_id"]))
			}
		}
	}
	if len(candidates) == 0 {
		return errors.New("No permitted target printer available")
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE pipeline_runs SET status='dispatching',sliced_library_file_id=? WHERE id=? AND status='slicing'", file, run)
	if e != nil {
		return e
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return nil
	}
	load := map[int64]int{}
	for _, id := range candidates {
		var count int
		if e = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM print_queue WHERE printer_id=? AND status IN ('pending','dispatching','printing')", id).Scan(&count); e != nil {
			return e
		}
		load[id] = count
	}
	sort.SliceStable(candidates, func(i, j int) bool { return load[candidates[i]] < load[candidates[j]] })
	for i := 0; i < b.Copies; i++ {
		id := candidates[0]
		switch pipe["fanout_strategy"] {
		case "round_robin":
			id = candidates[i%len(candidates)]
		case "max_parallel":
			for _, c := range candidates {
				if load[c] < load[id] {
					id = c
				}
			}
		}
		load[id]++
		ids, e := s.farm.CreateCopies(ctx, tx, p, object{"printer_id": id, "library_file_id": file, "quantity": 1})
		if e != nil {
			return e
		}
		if len(ids) != 1 {
			return errors.New("Queue returned an invalid copy count")
		}
		if _, e = tx.ExecContext(ctx, "UPDATE pipeline_jobs SET status='queued',queue_entry_id=?,assigned_printer_id=?,dispatched_at=CURRENT_TIMESTAMP WHERE pipeline_run_id=? AND copy_index=?", ids[0], id, run, i); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "UPDATE pipeline_runs SET status='in_progress' WHERE id=?", run); e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) failRun(id int64, err error) {
	tx, e := s.db.Begin()
	if e != nil {
		return
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE pipeline_runs SET status='failed',error_message=?,completed_at=CURRENT_TIMESTAMP WHERE id=? AND status IN ('slicing','dispatching','queued')", err.Error(), id); e != nil {
		return
	}
	if _, e = tx.Exec("UPDATE pipeline_jobs SET status='failed',error_message=?,completed_at=CURRENT_TIMESTAMP WHERE pipeline_run_id=? AND status='pending'", err.Error(), id); e == nil {
		_ = tx.Commit()
	}
}

const runSelect = `SELECT r.*,p.name pipeline_name,p.target_kind,p.target_printer_id,p.target_model_class,p.fanout_strategy,COALESCE(f.filename,a.filename) source_filename,COALESCE(f.created_by_id,a.created_by_id) source_owner,a.printer_id archive_printer FROM pipeline_runs r LEFT JOIN slicer_pipelines p ON p.id=r.pipeline_id LEFT JOIN library_files f ON f.id=r.source_library_file_id LEFT JOIN print_archives a ON a.id=r.source_archive_id`

func terminal(v string) bool {
	switch v {
	case "completed", "cancelled", "partial_failure", "failed":
		return true
	}
	return false
}
func (s *Service) run(ctx context.Context, p access.Principal, id int64) (object, error) {
	a, e := rows(s.db, ctx, runSelect+" WHERE r.id=?", id)
	if e != nil {
		return nil, e
	}
	if len(a) == 0 || !scoped(p, a[0]) {
		return nil, sql.ErrNoRows
	}
	v := a[0]
	jobs, e := rows(s.db, ctx, `SELECT j.*,p.name assigned_printer_name FROM pipeline_jobs j LEFT JOIN printers p ON p.id=j.assigned_printer_id WHERE pipeline_run_id=? ORDER BY copy_index`, id)
	if e != nil {
		return nil, e
	}
	if !p.Admin && p.PrinterIDs != nil {
		for _, j := range jobs {
			if pid := number(j["assigned_printer_id"]); pid > 0 && !p.AllowsPrinter(pid) {
				return nil, sql.ErrNoRows
			}
		}
	}
	family := "library"
	if v["source_archive_id"] != nil {
		family = "archives"
	}
	if (!p.Has(family+":read_all") && !(number(v["source_owner"]) != 0 && number(v["source_owner"]) == p.ID && p.Has(family+":read_own"))) || (number(v["archive_printer"]) > 0 && !p.AllowsPrinter(number(v["archive_printer"]))) {
		v["source_filename"] = nil
		v["source_archive_id"] = nil
		v["source_library_file_id"] = nil
	}
	delete(v, "source_owner")
	delete(v, "archive_printer")
	owner := number(v["created_by"])
	if !p.Has("library:read_all") && !(owner != 0 && owner == p.ID && p.Has("library:read_own")) {
		v["sliced_library_file_id"] = nil
	}
	completed, failed, cancelled, active := 0, 0, 0, 0
	for _, j := range jobs {
		if !p.Has("queue:read_all") && !(owner != 0 && owner == p.ID && p.Has("queue:read_own")) {
			j["queue_entry_id"] = nil
		}
		switch j["status"] {
		case "completed":
			completed++
		case "failed":
			failed++
		case "cancelled":
			cancelled++
		case "printing", "queued", "awaiting_printer":
			active++
		}
	}
	v["jobs"] = jobs
	v["copies_completed"] = completed
	v["copies_failed"] = failed
	v["copies_cancelled"] = cancelled
	v["copies_in_progress"] = active
	v["eligibility_overridden"] = httpx.Bool(v["eligibility_overridden"])
	if v["status"] == "in_progress" && len(jobs) > 0 && completed+failed+cancelled == len(jobs) {
		status := "partial_failure"
		switch {
		case completed == len(jobs):
			status = "completed"
		case failed == len(jobs):
			status = "failed"
		case cancelled == len(jobs):
			status = "cancelled"
		}
		_, e = s.db.ExecContext(ctx, "UPDATE pipeline_runs SET status=?,completed_at=COALESCE(completed_at,CURRENT_TIMESTAMP) WHERE id=? AND status='in_progress'", status, id)
		if e != nil {
			return nil, e
		}
		var done any
		var savedStatus string
		if e = s.db.QueryRowContext(ctx, "SELECT status,completed_at FROM pipeline_runs WHERE id=?", id).Scan(&savedStatus, &done); e != nil {
			return nil, e
		}
		v["status"] = savedStatus
		v["completed_at"] = done
	}
	return v, nil
}
func (s *Service) readRun(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:read") {
		return
	}
	v, e := s.run(r.Context(), access.From(r.Context()), routeID(r))
	if e != nil {
		pipelineErr(w, e)
		return
	}
	httpx.JSON(w, 200, v)
}
func (s *Service) listRuns(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:read") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	q := r.URL.Query()
	limit := number(q.Get("limit"))
	if limit == 0 {
		limit = 25
	}
	limit = max(int64(1), min(int64(500), limit))
	offset := max(int64(0), number(q.Get("offset")))
	args := []any{}
	query := "SELECT r.id FROM pipeline_runs r LEFT JOIN slicer_pipelines p ON p.id=r.pipeline_id WHERE 1"
	pipelineID := routeID(r)
	if pipelineID == 0 {
		pipelineID = number(q.Get("pipeline_id"))
	}
	if pipelineID > 0 {
		query += " AND r.pipeline_id=?"
		args = append(args, pipelineID)
	}
	if id := number(q.Get("target_printer_id")); id > 0 {
		query += " AND p.target_printer_id=?"
		args = append(args, id)
	}
	if model := q.Get("target_model_class"); model != "" {
		query += " AND p.target_model_class=?"
		args = append(args, model)
	}
	ids, e := rows(s.db, ctx, query+" ORDER BY r.id DESC", args...)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	out := []object{}
	var total int64
	for _, id := range ids {
		v, e := s.run(ctx, p, number(id["id"]))
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			pipelineErr(w, e)
			return
		}
		if status := q.Get("status"); status != "" && status != v["status"] {
			continue
		}
		if total >= offset && int64(len(out)) < limit {
			out = append(out, v)
		}
		total++
	}
	httpx.JSON(w, 200, object{"runs": out, "total": total})
}
func (s *Service) clearRuns(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:write") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	ids, e := rows(s.db, ctx, "SELECT id FROM pipeline_runs")
	if e != nil {
		pipelineErr(w, e)
		return
	}
	var deleted int64
	for _, item := range ids {
		id := number(item["id"])
		v, e := s.run(ctx, p, id)
		if e != nil {
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			pipelineErr(w, e)
			return
		}
		if !terminal(httpx.Str(v["status"])) || !p.Has("queue:update_all") && number(v["created_by"]) != p.ID {
			continue
		}
		res, e := s.db.ExecContext(ctx, "DELETE FROM pipeline_runs WHERE id=? AND status IN ('completed','partial_failure','cancelled','failed')", id)
		if e != nil {
			pipelineErr(w, e)
			return
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	httpx.JSON(w, 200, object{"deleted": deleted})
}
func (s *Service) cancelRun(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:run") {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	id := routeID(r)
	v, e := s.run(ctx, p, id)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	if !p.Has("queue:update_all") && !(p.ID > 0 && p.ID == number(v["created_by"]) && p.Has("queue:update_own")) {
		httpx.Error(w, 403, "Cancellation requires permission to change these queue entries")
		return
	}
	if terminal(httpx.Str(v["status"])) {
		httpx.JSON(w, 200, v)
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	defer tx.Rollback()
	// A physical command may have already left the server. Await its printer report.
	_, e = tx.ExecContext(ctx, `UPDATE print_queue SET status='cancelled',completed_at=CURRENT_TIMESTAMP,waiting_reason=NULL WHERE id IN (SELECT queue_entry_id FROM pipeline_jobs WHERE pipeline_run_id=?) AND status='pending'`, id)
	if e == nil {
		_, e = tx.ExecContext(ctx, "UPDATE pipeline_jobs SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE pipeline_run_id=? AND status IN ('pending','awaiting_printer') AND queue_entry_id IS NULL", id)
	}
	if e == nil {
		_, e = tx.ExecContext(ctx, "UPDATE pipeline_runs SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE id=? AND status NOT IN ('completed','partial_failure','cancelled','failed')", id)
	}
	if e == nil {
		e = tx.Commit()
	}
	if e != nil {
		pipelineErr(w, e)
		return
	}
	v, e = s.run(ctx, p, id)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	httpx.JSON(w, 200, v)
}
func (s *Service) retryRun(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "pipelines:run") {
		return
	}
	id := routeID(r)
	v, e := s.run(r.Context(), access.From(r.Context()), id)
	if e != nil {
		pipelineErr(w, e)
		return
	}
	count := int(number(v["copies_failed"]) + number(v["copies_cancelled"]))
	if count < 1 {
		httpx.Error(w, 400, "No failed copies to retry")
		return
	}
	opts := struct {
		Force bool `json:"force"`
	}{Force: true}
	if r.ContentLength > 0 && !httpx.Decode(w, r, &opts) {
		return
	}
	s.startRun(w, r, number(v["pipeline_id"]), runRequest{Library: number(v["source_library_file_id"]), Archive: number(v["source_archive_id"]), Copies: count, Force: opts.Force}, id, false)
}
