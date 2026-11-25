package cloudbackup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

var categories = []string{"settings", "spools", "archives", "kprofiles"}
var writePerm = map[string]string{"settings": "settings:update", "spools": "inventory:update", "archives": "archives:update_all", "kprofiles": "kprofiles:update"}

type snapshot struct {
	calibrations []calibrationFile
	ref          string
	meta         obj
	settings     obj
	records      map[string][]obj
	included     map[string]bool
	commit       obj
}

func (s *Service) load(ctx context.Context, c obj, ref string, selected map[string]bool) (snapshot, error) {
	ss := snapshot{records: map[string][]obj{}, included: map[string]bool{}}
	repo, e := s.repo(ctx, c)
	if e != nil {
		return ss, e
	}
	if _, e = repo.inspect(ctx); e != nil {
		return ss, e
	}
	if ss.ref, e = repo.resolve(ctx, ref); e != nil {
		return ss, e
	}
	get := func(name string) (obj, error) {
		b, e := repo.file(ctx, ss.ref, name)
		if e != nil {
			return nil, e
		}
		var x obj
		if e = json.Unmarshal(b, &x); e != nil || x == nil {
			return nil, errors.New("invalid snapshot file " + name)
		}
		return x, nil
	}
	if ss.meta, e = get(metadataPath); e != nil {
		return ss, e
	}
	if ss.meta["backup_type"] != "bambuddy_profiles" || ss.meta["version"] != "1.0" {
		return ss, errors.New("unsupported configuration backup")
	}
	if commits, e := repo.commits(ctx, ss.ref, 1); e == nil && len(commits) > 0 && str(commits[0]["sha"]) == ss.ref {
		ss.commit = commits[0]
	}
	marked := sub(ss.meta["contents"])
	manifest := map[string]bool{}
	paths, listed := ss.meta["files"].([]any)
	for _, p := range paths {
		manifest[str(p)] = true
	}
	probe := func(category, name string) (obj, error) {
		if !selected[category] || !truth(marked[category]) || listed && !manifest[name] {
			return nil, nil
		}
		x, e := get(name)
		if !listed && errors.Is(e, remoteError(404)) {
			return nil, nil
		}
		if e != nil {
			return nil, e
		}
		if x["version"] != "1.0" {
			return nil, errors.New("unsupported snapshot file version")
		}
		ss.included[category] = true
		return x, nil
	}
	payload, e := probe("settings", settingsPath)
	if e != nil {
		return ss, e
	}
	if payload != nil {
		var ok bool
		if ss.settings, ok = payload["settings"].(obj); !ok {
			return ss, errors.New("invalid settings snapshot")
		}
	}
	for _, set := range datasets {
		x, e := probe(set.category, set.path)
		if e != nil {
			return ss, e
		}
		if x == nil {
			continue
		}
		list, ok := x[set.key].([]any)
		if !ok {
			return ss, errors.New("invalid record list in " + set.path)
		}
		for _, v := range list {
			m, ok := v.(obj)
			if !ok {
				return ss, errors.New("invalid record in " + set.path)
			}
			ss.records[set.table] = append(ss.records[set.table], m)
		}
	}
	if selected["kprofiles"] && truth(marked["kprofiles"]) {
		names := []string{}
		if listed {
			for p := range manifest {
				names = append(names, p)
			}
		} else {
			names, e = repo.paths(ctx, ss.ref)
			if e != nil {
				return ss, e
			}
		}
		slices.Sort(names)
		for _, p := range names {
			m := calibrationPath.FindStringSubmatch(p)
			if len(m) != 3 {
				continue
			}
			payload, err := get(p)
			if err != nil {
				return ss, err
			}
			if payload["version"] != "1.0" || str(payload["printer_serial"]) != m[1] || str(payload["nozzle_diameter"]) != m[2] {
				return ss, errors.New("calibration identity differs from its snapshot path")
			}
			v, ok := payload["profiles"].([]any)
			if !ok {
				return ss, errors.New("invalid calibration list")
			}
			cf := calibrationFile{serial: m[1], nozzle: m[2]}
			for _, x := range v {
				row, ok := x.(obj)
				if !ok {
					return ss, errors.New("invalid calibration profile")
				}
				cf.profiles = append(cf.profiles, row)
			}
			ss.calibrations = append(ss.calibrations, cf)
			ss.included["kprofiles"] = true
		}
	}
	return ss, nil
}
func allowedSettings(p access.Principal, key string, value any) (string, bool) {
	schema, exists := release.Settings[key]
	if !exists || secretKey(key) || strings.HasPrefix(key, "ldap_") {
		return "", false
	}
	// Authentication, addresses used with existing credentials, exposure switches,
	// and filesystem destinations must pass their dedicated configuration flow.
	switch key {
	case "auth_enabled", "advanced_auth_enabled", "local_login_enabled", "setup_completed", "local_backup_path", "mqtt_broker", "mqtt_port", "mqtt_use_tls", "mqtt_enabled", "ha_url", "homeassistant_url", "spoolman_url", "obico_ml_url", "prometheus_enabled", "external_url", "manyfold_url", "manyfold_enabled":
		return "", false
	}
	if strings.HasPrefix(key, "smtp_") || strings.HasSuffix(key, "_path") || strings.HasPrefix(key, "two_factor_") || !p.Admin && key == "session_max_hours" {
		return "", false
	}
	val := str(value)
	if secretURL(val) || len(val) > 1<<18 || val == mask {
		return "", false
	}
	choices, _ := schema["anyOf"].([]any)
	if len(choices) == 0 {
		choices = []any{schema}
	}
	for _, choice := range choices {
		r := sub(choice)
		switch r["type"] {
		case "string":
			if _, ok := value.(string); !ok {
				continue
			}
			if opts, ok := r["enum"].([]any); ok && !slices.Contains(opts, any(val)) {
				continue
			}
			if n := ident(r["maxLength"]); n > 0 && len([]rune(val)) > int(n) {
				continue
			}
			if strings.HasSuffix(key, "_url") && val != "" {
				u, e := url.Parse(val)
				if e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
					continue
				}
			}
			return val, true
		case "boolean":
			if b, e := strconv.ParseBool(val); e == nil {
				return strconv.FormatBool(b), true
			}
		case "integer", "number":
			n, e := strconv.ParseFloat(val, 64)
			if e != nil || math.IsInf(n, 0) || math.IsNaN(n) || r["type"] == "integer" && math.Trunc(n) != n {
				continue
			}
			if v, ok := r["minimum"].(float64); ok && n < v {
				continue
			}
			if v, ok := r["maximum"].(float64); ok && n > v {
				continue
			}
			return val, true
		case "object", "array":
			var v any
			if len(val) <= 1<<18 && json.Unmarshal([]byte(val), &v) == nil {
				_, isObj := v.(obj)
				_, isArr := v.([]any)
				if r["type"] == "array" && isArr || r["type"] == "object" && isObj {
					return val, true
				}
			}
		}
	}
	return "", false
}
func selection(p access.Principal) map[string]bool {
	a := map[string]bool{}
	for k, perm := range writePerm {
		a[k] = p.Has(perm)
	}
	return a
}
func (s *Service) preview(r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	c, e := s.config(ctx)
	if e != nil {
		return nil, e
	}
	permitted := selection(p)
	ss, e := s.load(ctx, c, r.URL.Query().Get("ref"), permitted)
	if e != nil {
		return nil, e
	}
	report := []obj{}
	for _, k := range categories {
		n := 0
		var detail, code any
		if k == "settings" {
			for key, v := range ss.settings {
				if _, ok := allowedSettings(p, key, v); ok {
					n++
				}
			}
			if n < len(ss.settings) {
				code = "settingsNotRestorableWillSkip"
				detail = "Protected credentials and unsupported settings will be skipped"
			}
		}
		for _, d := range datasets {
			if d.category == k {
				n += len(ss.records[d.table])
			}
		}
		if k == "archives" && ss.included[k] {
			detail = "Print files and photos are stored outside the configuration repository"
			code = "archivesMetadataOnly"
		}
		if k == "kprofiles" && ss.included[k] {
			for _, f := range ss.calibrations {
				n += len(f.profiles)
			}
			code = "kprofilesAlwaysOverwrite"
			detail = "Calibration records are matched to current slots on connected printers"
		}
		report = append(report, obj{"category": k, "available": ss.included[k], "item_count": n, "detail": detail, "detail_code": code, "detail_params": obj{}})
	}
	return obj{"success": true, "message": "OK", "ref": ss.ref, "commit": ss.commit, "metadata_version": ss.meta["version"], "categories": report}, nil
}

type tally struct {
	Restored int   `json:"restored"`
	Skipped  int   `json:"skipped"`
	Failed   int   `json:"failed"`
	Notes    []obj `json:"notes"`
}

func (t *tally) note(code, msg string) {
	for _, n := range t.Notes {
		if n["code"] == code {
			return
		}
	}
	t.Notes = append(t.Notes, obj{"code": code, "message": msg, "params": obj{}})
}

type importer struct {
	ctx       context.Context
	tx        *sql.Tx
	repo      string
	p         access.Principal
	overwrite bool
	ids       map[string]map[int64]int64
	results   map[string]*tally
}

func (i *importer) local(q string, args ...any) (int64, error) {
	var id int64
	e := i.tx.QueryRowContext(i.ctx, q, args...).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return 0, nil
	}
	return id, e
}
func (i *importer) match(t string, row obj) (id int64, err error) {
	old := ident(row["id"])
	at := str(row["created_at"])
	if old <= 0 {
		return 0, nil
	}
	if at != "" {
		// Verify creation identity even for a previous import: SQLite tables
		// without AUTOINCREMENT can reuse the id of a deleted record.
		id, err = i.local("SELECT a.id FROM bambastic_git_restore_ids m JOIN "+t+" a ON a.id=m.target_id WHERE m.repo=? AND m.kind=? AND m.source_id=? AND m.identity=? AND julianday(a.created_at)=julianday(?)", i.repo, t, old, at, at)
		if err != nil || id > 0 {
			return
		}
		q := "SELECT id FROM " + t + " WHERE id=? AND julianday(created_at)=julianday(?)"
		args := []any{old, at}
		switch t {
		case "spool":
			q += " AND material=?"
			args = append(args, row["material"])
		case "print_archives":
			q += " AND filename=?"
			args = append(args, row["filename"])
		case "print_log_entries", "spool_usage_history":
			q += " AND print_name IS ?"
			args = append(args, row["print_name"])
		}
		id, err = i.local(q, args...)
		if err != nil || id > 0 {
			return
		}
	}
	if t == "spool" {
		return i.local("SELECT CASE WHEN count(*)=1 THEN min(id) ELSE 0 END FROM spool WHERE (tag_uid=? AND tag_uid!='') OR (tray_uuid=? AND tray_uuid!='')", row["tag_uid"], row["tray_uuid"])
	}
	return 0, nil
}
func (i *importer) remember(t string, row obj, id int64) error {
	old := ident(row["id"])
	if old <= 0 {
		return nil
	}
	i.ids[t][old] = id
	at := str(row["created_at"])
	if at == "" {
		return nil
	}
	_, e := i.tx.ExecContext(i.ctx, "INSERT INTO bambastic_git_restore_ids(repo,kind,source_id,identity,target_id) VALUES(?,?,?,?,?) ON CONFLICT(repo,kind,source_id,identity) DO UPDATE SET target_id=excluded.target_id", i.repo, t, old, at, id)
	return e
}
func (i *importer) location(name string) (int64, error) {
	n := strings.TrimSpace(name)
	if n == "" {
		return 0, nil
	}
	k := strings.ToLower(n)
	if _, e := i.tx.ExecContext(i.ctx, "INSERT INTO locations(name,name_key) VALUES(?,?) ON CONFLICT(name_key) DO NOTHING", n, k); e != nil {
		return 0, e
	}
	return i.local("SELECT id FROM locations WHERE name_key=?", k)
}
func stampValid(s string) bool {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if _, e := time.Parse(layout, s); e == nil {
			return true
		}
	}
	return false
}
func (i *importer) record(ds dataset, row obj) error {
	t := ds.table
	out := i.results[ds.category]
	if t == "archive_aliases" {
		return i.alias(row, out)
	}
	if ident(row["id"]) <= 0 || !stampValid(str(row["created_at"])) {
		out.Failed++
		out.note("missingIdentity", "A record lacked a stable identifier and creation date")
		return nil
	}
	existing, e := i.match(t, row)
	if e != nil {
		return e
	}
	if existing > 0 && !i.overwrite {
		out.Skipped++
		return i.remember(t, row, existing)
	}
	d := obj{}
	for _, k := range release.Fields[t] {
		v, known := row[k]
		if !known || k == "id" || k == "owner_username" || k == "created_by_id" || k == "created_by_username" && t == "print_archives" {
			continue
		}
		switch v.(type) {
		case nil, string, bool, float64:
		default:
			out.Failed++
			out.note("invalidRecord", "A record contained invalid field values")
			return nil
		}
		d[k] = v
	}
	if existing > 0 {
		delete(d, "created_at")
	}
	if t == "print_archives" || t == "print_log_entries" {
		if str(row["status"]) == "printing" || str(row["status"]) == "running" || str(row["status"]) == "paused" {
			if existing > 0 {
				delete(d, "status")
				delete(d, "failure_reason")
				delete(d, "completed_at")
			} else {
				d["status"] = "cancelled"
				d["failure_reason"] = "noStatusUpdate"
			}
		}
		if existing > 0 {
			live, e := i.local("SELECT id FROM "+t+" WHERE id=? AND status IN ('printing','running','paused')", existing)
			if e != nil {
				return e
			}
			if live != 0 {
				out.Skipped++
				out.note("liveRecordsKept", "A local print is still active")
				return i.remember(t, row, existing)
			}
		}
		k := "owner_username"
		if t == "print_archives" {
			k = "created_by_username"
		}
		if owner := str(row[k]); owner != "" {
			id, e := i.local("SELECT id FROM users WHERE username=? AND is_active", owner)
			if e != nil {
				return e
			}
			if id != 0 {
				d["created_by_id"] = id
			} else {
				out.note("archivesOwnerUnknown", "A snapshot owner has no matching local user")
			}
		}
	}
	for _, col := range []string{"project_id", "printer_id"} {
		if _, ok := d[col]; !ok {
			continue
		}
		table := "printers"
		if col == "project_id" {
			table = "projects"
		}
		id, e := i.local("SELECT id FROM "+table+" WHERE id=?", ident(row[col]))
		if e != nil {
			return e
		}
		if id > 0 {
			d[col] = id
		} else if existing == 0 {
			d[col] = nil
		} else {
			delete(d, col)
		}
	}
	switch t {
	case "spool":
		if str(row["material"]) == "" {
			out.Failed++
			return nil
		}
		if existing == 0 {
			for k, v := range (obj{"label_weight": 1000, "core_weight": 0, "weight_used": 0, "weight_used_baseline": 0, "weight_locked": false}) {
				if d[k] == nil {
					d[k] = v
				}
			}
		}
		// An ambiguous tag on this instance must not create another duplicate.
		for _, k := range []string{"tag_uid", "tray_uuid"} {
			if tag := str(d[k]); tag != "" {
				dup, e := i.local("SELECT id FROM spool WHERE "+k+"=? AND id!=? LIMIT 1", tag, existing)
				if e != nil {
					return e
				}
				if dup > 0 {
					out.Skipped++
					out.note("spoolTagConflict", "A tag already belongs to a different local spool")
					return nil
				}
			}
		}
		if loc := str(d["storage_location"]); loc != "" {
			id, e := i.location(loc)
			if e != nil {
				return e
			}
			d["location_id"] = id
		}
	case "print_archives":
		if existing == 0 {
			d["file_path"] = ""
			if str(d["filename"]) == "" {
				d["filename"] = "restored-from-backup"
			}
			d["filename"] = path.Base(strings.ReplaceAll(str(d["filename"]), "\\", "/"))
			for k, v := range (obj{"file_size": 0, "quantity": 1, "is_favorite": false, "status": "completed"}) {
				if d[k] == nil {
					d[k] = v
				}
			}
			out.note("archivesMetadataOnly", "Restored archive records contain metadata only")
		}
		if existing > 0 {
			for _, k := range []string{"filename", "file_size", "content_hash", "gcode_hash"} {
				delete(d, k)
			}
		}
		for _, k := range []string{"external_url", "makerworld_url"} {
			if secretURL(str(d[k])) {
				delete(d, k)
			}
		}
	case "print_log_entries", "spool_usage_history":
		delete(d, "archive_id")
		if id := i.ids["print_archives"][ident(row["archive_id"])]; id > 0 {
			d["archive_id"] = id
		}
		if t == "spool_usage_history" {
			delete(d, "spool_id")
			if id := i.ids["spool"][ident(row["spool_id"])]; id > 0 {
				d["spool_id"] = id
			} else if ident(row["spool_id"]) > 0 {
				out.Skipped++
				out.note("usageMissingSpool", "Skipped a usage record without a matching spool")
				return nil
			}
			if d["weight_used"] == nil || d["percent_used"] == nil {
				out.Failed++
				return nil
			}
		}
		if d["status"] == nil && existing == 0 {
			d["status"] = "completed"
		}
	}
	id, e := mutate(i.ctx, i.tx, t, existing, d)
	if e != nil {
		return e
	}
	out.Restored++
	return i.remember(t, row, id)
}
func (i *importer) alias(row obj, out *tally) error {
	old := ident(row["merged_archive_id"])
	dest := i.ids["print_archives"][ident(row["archive_id"])]
	if known := i.ids["print_archives"][old]; known > 0 {
		old = known
	}
	if old <= 0 || dest <= 0 || old == dest {
		out.Skipped++
		return nil
	}
	existing, e := i.local("SELECT archive_id FROM archive_aliases WHERE merged_archive_id=?", old)
	if e != nil {
		return e
	}
	if existing > 0 {
		out.Skipped++
		return nil
	}
	if active, e := i.local("SELECT id FROM print_archives WHERE id=? AND deleted_at IS NULL", old); e != nil {
		return e
	} else if active > 0 {
		out.Skipped++
		return nil
	}
	_, e = i.tx.ExecContext(i.ctx, "INSERT INTO archive_aliases(merged_archive_id,archive_id) VALUES(?,?)", old, dest)
	if e == nil {
		out.Restored++
	}
	return e
}
func (s *Service) apply(r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	d, e := body(r)
	if e != nil {
		return nil, e
	}
	list, ok := d["categories"].([]any)
	if !ok || len(list) == 0 {
		return nil, invalid("Choose at least one restore category")
	}
	selected := map[string]bool{}
	results := map[string]*tally{}
	for _, v := range list {
		k := str(v)
		perm := writePerm[k]
		if perm == "" {
			return nil, invalid("Unknown restore category")
		}
		if !p.Has(perm) {
			return nil, fault{403, "Missing " + perm}
		}
		selected[k] = true
		results[k] = &tally{Notes: []obj{}}
	}
	if _, exists := d["overwrite_existing"]; exists {
		if _, ok := d["overwrite_existing"].(bool); !ok {
			return nil, invalid("overwrite_existing must be boolean")
		}
	}
	if !s.lock(true) {
		return nil, fault{409, "Backup operation already running"}
	}
	defer s.unlock()
	c, e := s.config(ctx)
	if e != nil {
		return nil, e
	}
	id, e := mutate(ctx, s.db, "github_backup_logs", 0, obj{"config_id": c["id"], "trigger": "restore", "status": "running", "files_changed": 0})
	if e != nil {
		return nil, e
	}
	ss, e := s.load(ctx, c, str(d["ref"]), selected)
	if e == nil {
		e = s.importSnapshot(ctx, c, ss, p, selected, truth(d["overwrite_existing"]), results)
	}
	if e == nil && selected["kprofiles"] && ss.included["kprofiles"] {
		s.restoreCalibrations(ctx, ss.calibrations, results["kprofiles"])
	}
	success := e == nil
	message := "Restored selected configuration"
	state := "success"
	n := 0
	failed := 0
	for _, t := range results {
		n += t.Restored
		failed += t.Failed
	}
	if e != nil {
		state = "failed"
		message = e.Error()
		n = 0
		for _, t := range results {
			t.Restored = 0
		}
	} else if failed > 0 {
		message = fmt.Sprintf("Restored %d records, with %d unavailable or invalid", n, failed)
		if n == 0 {
			state = "failed"
			success = false
		}
	}
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	log := obj{"status": state, "commit_sha": ss.ref, "completed_at": time.Now().UTC().Format(time.RFC3339), "files_changed": n}
	if !success || failed > 0 {
		log["error_message"] = message
	}
	if _, err := mutate(recordCtx, s.db, "github_backup_logs", id, log); err != nil {
		return nil, err
	}
	return obj{"success": success, "message": message, "ref": ss.ref, "log_id": id, "results": results}, nil
}
func (s *Service) importSnapshot(ctx context.Context, c obj, ss snapshot, p access.Principal, sel map[string]bool, overwrite bool, results map[string]*tally) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	i := importer{ctx: ctx, tx: tx, p: p, overwrite: overwrite, results: results, repo: str(c["repository_url"]), ids: map[string]map[int64]int64{}}
	for cat := range sel {
		if !ss.included[cat] {
			results[cat].Failed++
			results[cat].note("categoryUnavailable", "This category is unavailable at the selected commit")
		}
	}
	if sel["settings"] && ss.included["settings"] {
		out := results["settings"]
		for key, v := range ss.settings {
			val, ok := allowedSettings(p, key, v)
			if !ok {
				out.Skipped++
				out.note("settingsProtected", "Protected credentials and unsupported settings were skipped")
				continue
			}
			if !overwrite {
				found, e := i.local("SELECT id FROM settings WHERE key=?", key)
				if e != nil {
					return e
				}
				if found > 0 {
					out.Skipped++
					continue
				}
			}
			if e = access.Put(tx, key, val); e != nil {
				return e
			}
			out.Restored++
		}
	}
	for _, set := range datasets {
		i.ids[set.table] = map[int64]int64{}
		for _, v := range ss.records[set.table] {
			if e = i.record(set, v); e != nil {
				return e
			}
		}
	}
	return tx.Commit()
}
