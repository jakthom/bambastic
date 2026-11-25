package production

import (
	"context"
	"database/sql"
	"strconv"
	"strings"

	"bambastic/internal/management"
)

// Load expensive layer or Spoolman evidence before opening the lifecycle write transaction.
// The run ID is verified again inside AttributeMaterial after acquiring the dispatch lock.
func (s *Service) materialCloseInputs(ctx context.Context, printer int64, r row) management.MaterialCloseInputs {
	if !idle(val(r["gcode_state"])) {
		return management.MaterialCloseInputs{}
	}
	v, e := one(ctx, s.db, `SELECT l.id AS run_id,l.plate_id,a.file_path FROM print_log_entries l LEFT JOIN print_archives a ON a.id=l.archive_id
		WHERE l.printer_id=? AND l.status='printing' AND l.completed_at IS NULL ORDER BY l.id DESC LIMIT 1`, printer)
	if e != nil {
		return management.MaterialCloseInputs{}
	}
	path, _ := s.assetPath(v["file_path"])
	return management.PrepareMaterialClose(ctx, s.db, num(v["run_id"]), printer, num(v["plate_id"]), path, strings.EqualFold(val(r["gcode_state"]), "FINISH"), r)
}
func materialReportChanged(delta, snapshot row) bool {
	if delta["gcode_state"] != nil || delta["subtask_id"] != nil || delta["gcode_file"] != nil {
		return true
	}
	if !active(val(snapshot["gcode_state"])) {
		return false
	}
	for _, name := range []string{"layer_num", "total_layer_num", "mc_percent", "ams", "ams_mapping", "mapping", "vt_tray", "vt_tray_list"} {
		if delta[name] != nil {
			return true
		}
	}
	return false
}
func physicalPlate(r row) int64 {
	for _, key := range []string{"plate_id", "plate_index"} {
		if i := num(r[key]); i > 0 {
			return i
		}
	}
	raw := strings.ReplaceAll(val(r["gcode_file"]), "\\", "/")
	parts := strings.Split(raw, "/")
	base := strings.ToLower(parts[len(parts)-1])
	if strings.HasPrefix(base, "plate_") && strings.HasSuffix(base, ".gcode") {
		id, _ := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(base, "plate_"), ".gcode"), 10, 64)
		if id > 0 {
			return id
		}
	}
	return 0
}
func (s *Service) measuredMaterial(ctx context.Context, tx *sql.Tx, archive, run, report row, status string, rid int64, inputs management.MaterialCloseInputs) (any, error) {
	if e := management.ObserveMaterial(ctx, tx, rid, num(run["printer_id"]), report, false, true); e != nil {
		return nil, e
	}
	tools := map[int]float64{}
	full := 0.
	meta, e := s.meta("archives", archive)
	if e == nil {
		for _, p := range meta.Plates {
			if p.Index != num(run["plate_id"]) && !(len(meta.Plates) == 1 && num(run["plate_id"]) == 0) {
				continue
			}
			full = p.Grams
			for _, f := range p.Filaments {
				g, _ := decimal(f["used_g"])
				if id := num(f["id"]); id > 0 && g > 0 {
					tools[int(id)-1] += g
				}
			}
			break
		}
	}
	g, e := management.AttributeMaterial(ctx, tx, rid, status == "completed", full, tools, inputs)
	if e != nil {
		return nil, e
	}
	if g != nil {
		return *g, nil
	}
	// Runs from earlier Go builds can predate durable telemetry. Their closed
	// single-plate measurement remains available when the slice supplies one.
	if status == "completed" && full > 0 {
		return full, nil
	}
	return nil, nil
}
