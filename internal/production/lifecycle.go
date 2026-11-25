package production

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/management"
	"bambastic/internal/operations"
)

func (s *Service) InitialStatus(ctx context.Context, p access.Principal) ([]map[string]any, error) {
	rs, e := records(ctx, s.db, "SELECT id,name,model,nozzle_count,awaiting_plate_clear FROM printers")
	out := []map[string]any{}
	if !p.Has("printers:read") {
		return out, nil
	}
	for _, v := range rs {
		if p.AllowsPrinter(num(v["id"])) {
			out = append(out, s.state(num(v["id"]), v))
		}
	}
	return out, e
}
func (s *Service) Run(ctx context.Context) {
	// "uploading" means that no start command has been issued. "sent" is ambiguous and
	// is retained across restarts until a report from the printer identifies the job.
	if tx, e := s.db.BeginTx(ctx, nil); e == nil {
		_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='pending',dispatching_at=NULL WHERE status='dispatching' AND id IN (SELECT queue_id FROM bb_attempts WHERE phase='uploading')")
		if e == nil {
			_, e = tx.ExecContext(ctx, "DELETE FROM bb_attempts WHERE phase='uploading'")
		}
		if e == nil {
			e = tx.Commit()
		} else {
			tx.Rollback()
		}
		if e != nil {
			log.Printf("dispatch recovery: %v", e)
		}
	}
	var workers sync.WaitGroup
	workers.Add(1)
	go func() { defer workers.Done(); s.librarySweepLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.captureLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.downloadLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.dryingLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.eventLoop(ctx) }()
	workers.Add(1)
	go func() { defer workers.Done(); s.firmwareLoop(ctx) }()
	running := map[int64]context.CancelFunc{}
	identities := map[int64]string{}
	defer func() {
		for _, stop := range running {
			stop()
		}
		workers.Wait()
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		s.releaseHeaters(ctx)
		s.keepWarm(ctx)
		rs, e := records(ctx, s.db, "SELECT * FROM printers WHERE is_active ORDER BY dispatch_priority IS NULL,dispatch_priority,id")
		if e == nil {
			seen := map[int64]bool{}
			for _, v := range rs {
				k := num(v["id"])
				seen[k] = true
				ident := jstr([]any{v["ip_address"], v["access_code"], v["name"], v["model"], v["auto_archive"]})
				if running[k] != nil && ident != identities[k] {
					running[k]()
					delete(running, k)
				}
				identities[k] = ident
				if running[k] == nil {
					child, stop := context.WithCancel(ctx)
					running[k] = stop
					workers.Add(1)
					go func(pr row) { defer workers.Done(); s.watch(child, pr) }(v)
				}
			}
			for k, stop := range running {
				if !seen[k] {
					stop()
					delete(running, k)
					delete(identities, k)
				}
			}
			for _, v := range rs {
				job, e := s.claim(ctx, v)
				if e != nil && !errors.Is(e, sql.ErrNoRows) && ctx.Err() == nil {
					log.Printf("claim: %v", e)
				}
				if job != nil {
					workers.Add(1)
					go func(j row) { defer workers.Done(); s.send(ctx, j) }(job)
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-s.wake:
		case <-tick.C:
		}
	}
}
func (s *Service) watch(ctx context.Context, pr row) {
	k := num(pr["id"])
	d := &liveDevice{printer: pr, report: row{}}
	s.mu.Lock()
	s.devices[k] = d
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if s.devices[k] == d {
			delete(s.devices, k)
		}
		s.mu.Unlock()
	}()
	for ctx.Err() == nil {
		d.drift.mu.Lock()
		d.drift.watches = nil
		d.calibration.mu.Lock()
		d.calibration.tables = nil
		d.calibration.mu.Unlock()
		s.mu.Lock()
		d.kEpoch++
		epoch := d.kEpoch
		d.kReport = row{}
		// A partial first report cannot confirm the previous session's print state.
		d.report = row{}
		s.mu.Unlock()
		d.drift.mu.Unlock()
		kChanged := make(chan struct{}, 1)
		trust, _ := one(ctx, s.db, "SELECT fingerprint FROM bb_printer_trust WHERE printer_id=?", k)
		s.db.ExecContext(ctx, "DELETE FROM bb_idle_watch WHERE queue_id IN(SELECT queue_id FROM bb_attempts WHERE printer_id=?)", k)
		t, cancel := context.WithTimeout(ctx, 12*time.Second)
		h, e := s.open(t, pr, val(trust["fingerprint"]), func(delta row) {
			if ctx.Err() != nil {
				return
			}
			d.logs.add("device/"+val(pr["serial_number"])+"/report", "in", row{"print": delta})
			d.calibration.receive(delta)
			s.mu.Lock()
			// Command acknowledgements may carry the parameters of another slicer.
			// Only status deltas contribute to the shared telemetry snapshot.
			if d.kEpoch != epoch || s.devices[k] != d {
				s.mu.Unlock()
				return
			}
			statusReport := isPrinterStatus(delta)
			if statusReport {
				merge(d.report, delta)
				for _, field := range []string{"ams", "device", "gcode_state", "vir_slot", "vt_tray", "vt_tray_list", "nozzle_diameter", "nozzle_type", "nozzle_diameter_2", "right_nozzle_diameter", "left_nozzle_diameter", "nozzle_type_2", "right_nozzle_type", "left_nozzle_type", "power_on_flag"} {
					if v, ok := delta[field]; ok {
						merge(d.kReport, row{field: v})
					}
				}
			}
			snap := row{}
			for key, value := range d.report {
				snap[key] = value
			}
			storeSample := time.Since(d.sampleAt) >= time.Minute
			if storeSample {
				d.sampleAt = time.Now()
			}
			connected := d.h != nil
			s.mu.Unlock()
			if statusReport {
				select {
				case kChanged <- struct{}{}:
				default:
				}
			}
			if storeSample {
				s.sample(ctx, pr, snap)
			}
			s.observeRefusal(ctx, k, delta)
			if materialReportChanged(delta, snap) {
				if e := s.observe(ctx, pr, snap); e != nil && ctx.Err() == nil {
					log.Printf("reconcile printer %d: %v", k, e)
				}
			}
			if connected {
				s.emit(row{"type": "printer_status", "printer_id": k, "data": s.state(k, pr)})
			}
		})
		cancel()
		if e == nil {
			if trust == nil {
				_, _ = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO bb_printer_trust VALUES (?,?)", k, h.Fingerprint())
			}
			s.mu.Lock()
			if real, ok := h.(*lan); ok {
				real.history = &d.logs
			}
			d.h = h
			s.mu.Unlock()
			s.emit(row{"type": "printer_status", "printer_id": k, "data": s.state(k, pr)})
			s.kick()
			lastPoll := time.Time{}
			for h.Online() && ctx.Err() == nil {
				if time.Since(lastPoll) >= 15*time.Second {
					var pending int
					if s.db.QueryRowContext(ctx, "SELECT count(*) FROM bb_attempts WHERE printer_id=? AND phase='sent'", k).Scan(&pending) == nil && pending > 0 {
						poll, done := context.WithTimeout(ctx, 3*time.Second)
						_ = h.Publish(poll, packet("pushing", "pushall", nil))
						done()
					}
					lastPoll = time.Now()
				}
				select {
				case <-ctx.Done():
				case <-kChanged:
					s.replayAssignments(ctx, k)
					s.reapplyK(ctx, k, d, nil, 0, time.Now())
				case <-time.After(time.Second):
				}
			}
			h.Close()
			s.mu.Lock()
			d.h = nil
			s.mu.Unlock()
			s.emit(row{"type": "printer_status", "printer_id": k, "data": s.state(k, pr)})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(8 * time.Second):
		}
	}
}
func merge(dst row, src row) {
	for k, v := range src {
		if nested, ok := v.(map[string]any); ok {
			if k == "ams" {
				prior, _ := dst[k].(map[string]any)
				dst[k] = mergeAMS(prior, nested)
				continue
			}
			merged := row{}
			if old, ok := dst[k].(map[string]any); ok {
				for k, v := range old {
					merged[k] = v
				}
			}
			merge(merged, nested)
			dst[k] = map[string]any(merged)
		} else {
			dst[k] = v
		}
	}
}
func active(state string) bool {
	switch strings.ToUpper(state) {
	case "RUNNING", "PAUSE", "PREPARE", "SLICING", "INIT":
		return true
	}
	return false
}
func idle(state string) bool {
	switch strings.ToUpper(state) {
	case "IDLE", "FINISH", "FAILED":
		return true
	}
	return false
}
func (s *Service) owner(ctx context.Context, uid int64) (access.Principal, error) {
	s.mu.RLock()
	f := s.resolve
	s.mu.RUnlock()
	if uid == 0 {
		return access.Principal{Admin: true}, nil
	}
	if f == nil {
		return access.Principal{}, errors.New("user scope unavailable")
	}
	return f(ctx, uid)
}
func (s *Service) hold(ctx context.Context, v row, reason string) {
	if v["waiting_reason"] != reason {
		_, _ = s.db.ExecContext(ctx, "UPDATE print_queue SET waiting_reason=? WHERE id=? AND status='pending'", reason, v["id"])
	}
}

var modelAliases = map[string]string{"BL-P001": "X1C", "BL-P002": "X1", "BL-P003": "X1E", "C13": "X1E", "C11": "P1P", "C12": "P1S", "N2S": "A1", "A11": "A1", "A12": "A1MINI", "A04": "A1MINI", "N1": "A1MINI", "N7": "P2S", "N6": "X2D", "O1D": "H2D", "O1E": "H2DPRO", "O2D": "H2DPRO", "O1C": "H2C", "O1C2": "H2C", "O1S": "H2S", "N9": "A2L", "N10": "H3C"}

func modelName(v any) string {
	name := strings.ToUpper(strings.ReplaceAll(strings.TrimPrefix(strings.ToUpper(val(v)), "BAMBU LAB "), " ", ""))
	if model := modelAliases[name]; model != "" {
		return model
	}
	return name
}
func compatible(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	family := map[string]bool{"X1": true, "X1C": true, "X1E": true, "P1P": true, "P1S": true}
	return family[a] && family[b]
}
func (s *Service) claim(ctx context.Context, pr row) (row, error) {
	k := num(pr["id"])
	s.mu.RLock()
	d := s.devices[k]
	if d == nil || d.h == nil || !d.h.Online() || !idle(val(d.report["gcode_state"])) {
		s.mu.RUnlock()
		return nil, nil
	}
	rp := row{}
	for k, v := range d.report {
		rp[k] = v
	}
	s.mu.RUnlock()
	var cooldown int64
	_ = s.db.QueryRowContext(ctx, "SELECT until FROM bb_device_cooldown WHERE printer_id=?", k).Scan(&cooldown)
	if cooldown > time.Now().Unix() {
		return nil, nil
	}
	if flag(pr["awaiting_plate_clear"]) {
		set, _ := access.Setting(ctx, s.db, "require_plate_clear", "false")
		if flag(set) {
			return nil, nil
		}
	}
	order := "position,id"
	if shortest, _ := access.Setting(ctx, s.db, "queue_shortest_first", "false"); flag(shortest) {
		order = "been_jumped DESC, print_time_seconds IS NULL OR print_time_seconds<=0, print_time_seconds,position,id"
	}
	rs, e := records(ctx, s.db, "SELECT print_queue.*,(SELECT key_id FROM bb_queue_actor WHERE queue_id=print_queue.id) origin_key FROM print_queue WHERE status='pending' AND NOT manual_start AND (printer_id=? OR printer_id IS NULL) ORDER BY "+order, k)
	if e != nil {
		return nil, e
	}
	for _, v := range rs {
		if gt := val(v["scheduled_time"]); gt != "" {
			when, e := time.Parse(time.RFC3339, gt)
			if e != nil || time.Now().Before(when) {
				s.hold(ctx, v, "Waiting for scheduled time")
				continue
			}
		}
		p, e := s.durableActor(ctx, num(v["created_by_id"]), num(v["origin_key"]))
		if e != nil || !p.AllowsPrinter(k) || !p.Has("queue:create") {
			s.hold(ctx, v, "Waiting for a printer in the submitter's scope")
			continue
		}
		candidate, has, e := s.alternativeFor(ctx, num(v["id"]), pr)
		if e != nil {
			return nil, e
		}
		if has && candidate == nil {
			continue
		}
		if candidate != nil {
			for _, key := range variantFields {
				v[key] = candidate[key]
			}
		}
		target := modelName(v["target_model"])
		if num(v["printer_id"]) == 0 && target != "" && target != "ANY" && target != "ANYMODEL" && target != modelName(pr["model"]) {
			continue
		}
		if loc := val(v["target_location"]); loc != "" && loc != val(pr["location"]) {
			continue
		}
		if flag(v["require_previous_success"]) && !flag(v["gate_acknowledged"]) {
			prev, _ := one(ctx, s.db, "SELECT status FROM print_queue WHERE printer_id=? AND status IN ('completed','failed') ORDER BY completed_at DESC,id DESC LIMIT 1", k)
			if prev["status"] == "failed" {
				if num(v["printer_id"]) != 0 {
					_, _ = s.db.ExecContext(ctx, "UPDATE print_queue SET status='skipped',completed_at=CURRENT_TIMESTAMP,waiting_reason='Previous print failed' WHERE id=? AND status='pending'", v["id"])
				} else {
					s.hold(ctx, v, "Previous print failed")
				}
				continue
			}
		}
		kind := "archives"
		aid := num(v["archive_id"])
		if aid == 0 {
			aid = num(v["library_file_id"])
			kind = "library"
		}
		a, e := s.getAsset(ctx, s.db, p, kind, aid, "read")
		if errors.Is(e, sql.ErrNoRows) && kind == "archives" && num(v["created_by_id"]) != 0 {
			// A claimed virtual upload remains unowned on the archive. The queue
			// grants use of its admitted source to its bounded physical dispatch.
			a, e = one(ctx, s.db, "SELECT a.* FROM print_archives a WHERE a.id=? AND a.deleted_at IS NULL AND a.created_by_id IS NULL AND EXISTS(SELECT 1 FROM bb_claimed_intake c WHERE c.queue_id=?)", aid, v["id"])
			if e == nil && !scope(p, num(a["printer_id"])) {
				e = sql.ErrNoRows
			}
		}
		if e != nil {
			s.hold(ctx, v, "Source is unavailable")
			continue
		}
		info, e := s.meta(kind, a)
		if e != nil || !compatible(modelName(info.Model), modelName(pr["model"])) {
			s.hold(ctx, v, "Waiting for compatible sliced file")
			continue
		}
		var selectedPlate plate
		for _, pl := range info.Plates {
			if pl.Printable && (num(v["plate_id"]) == 0 || pl.Index == num(v["plate_id"])) {
				selectedPlate = pl
				break
			}
		}
		if !selectedPlate.Printable {
			s.hold(ctx, v, "The selected plate has no printable G-code")
			continue
		}
		v["plate_id"] = selectedPlate.Index
		var mapping []int64
		var reason string
		if !flag(v["skip_filament_check"]) {
			mapping, reason = matchFilaments(v, rp, selectedPlate)
			if reason != "" {
				s.hold(ctx, v, reason)
				continue
			}
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return nil, e
		}
		if !flag(v["skip_filament_check"]) && len(mapping) > 0 && len(selectedPlate.Filaments) > 0 {
			stock, err := withStock(ctx, tx, p, pr, rp)
			if err == nil {
				ams, _ := rp["ams"].(map[string]any)
				if reason := deficit(stock, selectedPlate, mapping, flag(ams["auto_switch_filament"])); reason != "" {
					// A model-targeted job may still fit a different printer.
					fixed := num(v["printer_id"]) != 0
					_, err = tx.ExecContext(ctx, "UPDATE print_queue SET manual_start=?,filament_short=1,waiting_reason=? WHERE id=? AND status='pending'", fixed, reason, v["id"])
					if err != nil {
						tx.Rollback()
						return nil, err
					}
					if err = tx.Commit(); err != nil {
						return nil, err
					}
					continue
				}
			}
		}
		if blocked, _ := access.Setting(ctx, tx, "queue_drying_block", "false"); flag(blocked) {
			d := s.dryer(pr, 0)
			var running int
			err := tx.QueryRowContext(ctx, "SELECT count(*) FROM scheduled_dryings WHERE printer_id=? AND status='running'", k).Scan(&running)
			if err != nil {
				tx.Rollback()
				return nil, err
			}
			if d.anyDry || running > 0 {
				tx.Rollback()
				s.hold(ctx, v, "Waiting for AMS drying to finish")
				continue
			}
		}
		busy, e := one(ctx, tx, "SELECT (SELECT count(*) FROM bb_attempts WHERE printer_id=? AND phase IN ('uploading','sent','running'))+(SELECT count(*) FROM bb_external_run WHERE printer_id=?)+(SELECT count(*) FROM bb_firmware_jobs WHERE printer_id=? AND status IN ('preparing','downloading','uploading'))+(SELECT count(*) FROM bb_heater_owners WHERE printer_id=?)+(SELECT count(*) FROM bb_keep_warm WHERE printer_id=? AND released=0) n", k, k, k, k, k)
		if e != nil {
			tx.Rollback()
			return nil, e
		}
		if num(busy["n"]) > 0 {
			tx.Rollback()
			return nil, nil
		}
		blocked, e := operations.SensorHold(ctx, tx, k)
		if e != nil {
			tx.Rollback()
			return nil, e
		}
		if blocked {
			tx.Rollback()
			s.hold(ctx, v, "Waiting for Home Assistant sensor clearance")
			continue
		}
		if candidate != nil {
			if e = update(ctx, tx, "print_queue", num(v["id"]), candidate, variantFields); e != nil {
				tx.Rollback()
				return nil, e
			}
			if _, e = tx.ExecContext(ctx, "UPDATE print_queue_variants SET attempt_count=attempt_count+1 WHERE id=?", candidate["id"]); e != nil {
				tx.Rollback()
				return nil, e
			}
		}
		if e = management.RecheckQueue(ctx, tx, p, num(v["id"])); e != nil {
			tx.Rollback()
			s.hold(ctx, v, e.Error())
			continue
		}
		if len(mapping) > 0 {
			v["ams_mapping"] = jstr(mapping)
		}
		runID := uuid()
		name := fmt.Sprintf("bb-%d-%s%s", num(v["id"]), runID[:10], filepath.Ext(val(a["filename"])))
		if strings.HasSuffix(strings.ToLower(val(a["filename"])), ".gcode.3mf") {
			name = strings.TrimSuffix(name, ".3mf") + ".gcode.3mf"
		}
		res, e := tx.ExecContext(ctx, "UPDATE print_queue SET status='dispatching',printer_id=?,dispatching_at=CURRENT_TIMESTAMP,dispatch_attempts=dispatch_attempts+1,filament_short=0,plate_id=?,ams_mapping=?,billing_run_id=?,waiting_reason=NULL WHERE id=? AND status='pending' AND NOT manual_start", k, selectedPlate.Index, v["ams_mapping"], runID, v["id"])
		if e != nil {
			tx.Rollback()
			return nil, e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			tx.Rollback()
			continue
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO bb_attempts(queue_id,printer_id,name,phase) VALUES (?,?,?,'uploading') ON CONFLICT(queue_id) DO UPDATE SET printer_id=excluded.printer_id,name=excluded.name,phase='uploading',run_id=NULL,subtask=NULL,updated_at=CURRENT_TIMESTAMP", v["id"], k, name)
		if e != nil {
			tx.Rollback()
			return nil, e
		}
		if order != "position,id" {
			if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET been_jumped=1 WHERE status='pending' AND NOT manual_start AND position<? AND (printer_id=? OR (printer_id IS NULL AND target_model=?))", v["position"], k, pr["model"]); e != nil {
				tx.Rollback()
				return nil, e
			}
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		v["dispatch_plate"] = selectedPlate
		v["stored_bed"] = a["bed_temperature"]
		v["dispatch_model"] = pr["model"]
		v["remote"] = name
		v["file_path"] = a["file_path"]
		v["file_size"] = a["file_size"]
		v["printer_id"] = k
		return v, nil
	}
	return nil, nil
}
func (s *Service) send(ctx context.Context, v row) {
	k := num(v["printer_id"])
	qid := num(v["id"])
	failJob := func(msg string) {
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return
		}
		defer tx.Rollback()
		res, e := tx.ExecContext(ctx, "UPDATE print_queue SET status='failed',completed_at=CURRENT_TIMESTAMP,error_message=? WHERE id=? AND status='dispatching'", msg, qid)
		if e != nil {
			return
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return
		}
		if _, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='failed' WHERE queue_id=?", qid); e != nil {
			return
		}
		if tx.Commit() == nil {
			s.emit(row{"type": "queue_item_failed", "printer_id": k, "queue_item_id": qid, "reason": msg})
		}
	}
	s.mu.RLock()
	d := s.devices[k]
	var h hardware
	if d != nil {
		h = d.h
	}
	s.mu.RUnlock()
	if h == nil {
		failJob("Printer disconnected before upload")
		return
	}
	file, e := s.assetPath(v["file_path"])
	if e != nil {
		failJob("File unavailable")
		return
	}
	ok, err := s.preheat(ctx, v, file, h)
	if !ok {
		if ctx.Err() == nil {
			if err == nil {
				err = errors.New("Queue authorization or state changed")
			}
			failJob("Preheat did not complete: " + err.Error())
		}
		s.releaseHeaters(ctx)
		return
	}
	uploadFile, err := s.dispatchBytes(ctx, v, file)
	if err != nil {
		failJob("Could not prepare configured G-code snippets")
		return
	}
	if uploadFile != file {
		defer os.Remove(uploadFile)
		file = uploadFile
		if st, e := os.Stat(file); e == nil {
			v["file_size"] = st.Size()
		}
	}
	s.emit(row{"type": "queue_item_uploading", "printer_id": k, "queue_item_id": qid, "file_name": filepath.Base(file), "total_bytes": num(v["file_size"])})
	last := time.Time{}
	upload, cancel := context.WithTimeout(ctx, 45*time.Minute)
	e = h.Upload(upload, file, val(v["remote"]), func(n int64) {
		total := num(v["file_size"])
		if total <= 0 {
			return
		}
		if time.Since(last) > 250*time.Millisecond || n >= total {
			last = time.Now()
			s.emit(row{"type": "queue_item_upload_progress", "printer_id": k, "queue_item_id": qid, "bytes_transferred": n, "total_bytes": total, "pct": n * 100 / total})
		}
	})
	cancel()
	if e != nil {
		if s.coolDown(ctx, qid, k, e) {
			return
		}
		failJob("File upload failed")
		return
	}
	p, e := s.durableActor(ctx, num(v["created_by_id"]), num(v["origin_key"]))
	if e != nil || !p.AllowsPrinter(k) || !p.Has("queue:create") {
		failJob("Printer no longer permitted")
		return
	}
	s.db.ExecContext(ctx, "DELETE FROM bb_device_cooldown WHERE printer_id=?", k)
	// Recheck the latest physical state before committing the irreversible send.
	s.mu.RLock()
	current := s.devices[k]
	ready := current != nil && current.h == h && idle(val(current.report["gcode_state"]))
	s.mu.RUnlock()
	if !ready {
		failJob("Printer became unavailable during transfer")
		return
	}
	s.reapplyK(ctx, k, d, usedKTrays(v["ams_mapping"]), qid, time.Now())
	// Checking remote material identity can take time. Recheck the printer
	// before recording an irreversible send.
	s.mu.RLock()
	ready = s.devices[k] == d && d.h == h && idle(val(d.report["gcode_state"]))
	s.mu.RUnlock()
	if !ready {
		failJob("Printer became unavailable before dispatch")
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	blocked, e := operations.SensorHold(ctx, tx, k)
	if e != nil {
		return
	}
	reason := ""
	if blocked {
		reason = "Waiting for Home Assistant sensor clearance"
	} else if e = management.RecheckQueue(ctx, tx, p, qid); e != nil {
		reason = e.Error()
	}
	if reason != "" {
		if e = s.requeue(ctx, tx, qid, reason, "held"); e == nil {
			_ = tx.Commit()
		}
		return
	}
	res, e := tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='sent',updated_at=CURRENT_TIMESTAMP WHERE queue_id=? AND phase='uploading' AND EXISTS (SELECT 1 FROM print_queue WHERE id=? AND status='dispatching')", qid, qid)
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n != 1 || tx.Commit() != nil {
		return
	}
	cmd, cancel := context.WithTimeout(ctx, 8*time.Second)
	e = h.Publish(cmd, startMessage(val(v["remote"]), v))
	cancel()
	if e != nil {
		_, _ = s.db.ExecContext(ctx, "UPDATE print_queue SET waiting_reason='Start delivery uncertain; waiting for printer report' WHERE id=? AND status='dispatching'", qid)
	}
}
func cleanName(x any) string {
	name := strings.ToLower(filepath.Base(val(x)))
	return strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, ".3mf"), ".gcode"), ".bgcode")
}
func matches(name string, r row) bool {
	expected := cleanName(name)
	if expected == "" {
		return false
	}
	for _, key := range []string{"gcode_file", "subtask_name"} {
		if cleanName(r[key]) == expected {
			return true
		}
	}
	return false
}
func (s *Service) observe(ctx context.Context, pr, r row) error {
	inputs := s.materialCloseInputs(ctx, num(pr["id"]), r)
	s.dispatch.Lock()
	defer s.dispatch.Unlock()
	k := num(pr["id"])
	state := strings.ToUpper(val(r["gcode_state"]))
	subtask := val(r["subtask_id"])
	if subtask == "0" {
		subtask = ""
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	a, e := one(ctx, tx, "SELECT * FROM bb_attempts WHERE printer_id=? AND phase IN ('sent','running')", k)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	ext, e := one(ctx, tx, "SELECT * FROM bb_external_run WHERE printer_id=?", k)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	var event row
	if a != nil && a["phase"] == "sent" {
		if idle(state) {
			if e = s.idleEvidence(ctx, tx, a); e != nil {
				return e
			}
		} else if _, e = tx.ExecContext(ctx, "DELETE FROM bb_idle_watch WHERE queue_id=?", a["queue_id"]); e != nil {
			return e
		}
	}
	if active(state) {
		var current int64
		if a != nil && a["phase"] == "running" && sameRun(a, r, subtask) {
			current = num(a["run_id"])
		} else if ext != nil && sameRun(ext, r, subtask) {
			current = num(ext["run_id"])
		}
		if current > 0 {
			if e = management.ObserveMaterial(ctx, tx, current, k, r, false, false); e != nil {
				return e
			}
		}
		if a != nil && a["phase"] == "sent" && matches(val(a["name"]), r) {
			q, e := one(ctx, tx, "SELECT * FROM print_queue WHERE id=?", a["queue_id"])
			if e != nil {
				return e
			}
			rid, e := s.openRun(ctx, tx, pr, q, r)
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='running',run_id=?,subtask=?,updated_at=CURRENT_TIMESTAMP WHERE queue_id=?", rid, subtask, q["id"])
			if e != nil {
				return e
			}
			_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='printing',started_at=CURRENT_TIMESTAMP,waiting_reason=NULL WHERE id=?", q["id"])
			if e != nil {
				return e
			}
			event = row{"type": "print_start", "printer_id": k, "data": row{"run_id": rid, "queue_item_id": q["id"]}}
		} else if a == nil && ext == nil && flag(pr["auto_archive"]) {
			enabled, _ := access.Setting(ctx, tx, "auto_archive", "true")
			name := val(r["subtask_name"])
			if name == "" {
				name = val(r["gcode_file"])
			}
			if name != "" && flag(enabled) {
				aid, e := insert(ctx, tx, "print_archives", row{"printer_id": k, "filename": filepath.Base(name), "file_path": "", "file_size": 0, "print_name": name, "status": "printing", "is_favorite": false, "quantity": 1})
				if e != nil {
					return e
				}
				externalConfirm, e := access.Setting(ctx, tx, "confirm_outcome_external_prints", "false")
				if e != nil {
					return e
				}
				rid, e := s.openRun(ctx, tx, pr, row{"archive_id": aid, "billing_run_id": uuid(), "confirm_outcome": flag(externalConfirm), "plate_id": id(physicalPlate(r))}, r)
				if e != nil {
					return e
				}
				_, e = tx.ExecContext(ctx, "INSERT INTO bb_external_run VALUES(?,?,?,?)", k, rid, name, subtask)
				if e != nil {
					return e
				}
				remote := val(r["gcode_file"])
				if remote == "" {
					remote = name
				}
				if _, e = tx.ExecContext(ctx, "INSERT INTO bb_captures(run_id,archive_id,printer_id,remote_name) VALUES(?,?,?,?)", rid, aid, k, remote); e != nil {
					return e
				}
				event = row{"type": "print_start", "printer_id": k, "data": row{"run_id": rid, "archive_id": aid}}
			}
		}
	} else if idle(state) {
		var rid int64
		var queueID int64
		if a != nil && a["phase"] == "running" && sameRun(a, r, subtask) {
			rid = num(a["run_id"])
			queueID = num(a["queue_id"])
		} else if ext != nil && sameRun(ext, r, subtask) {
			rid = num(ext["run_id"])
		}
		if rid > 0 {
			status := "cancelled"
			if state == "FINISH" {
				status = "completed"
			} else if state == "FAILED" {
				status = "failed"
			}
			if e = s.closeRun(ctx, tx, pr, r, rid, queueID, status, inputs); e != nil {
				return e
			}
			if queueID > 0 {
				_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='ended' WHERE queue_id=?", queueID)
			} else {
				_, e = tx.ExecContext(ctx, "DELETE FROM bb_external_run WHERE printer_id=?", k)
			}
			if e != nil {
				return e
			}
			event = row{"type": "print_complete", "printer_id": k, "data": row{"run_id": rid, "status": status, "queue_item_id": id(queueID)}}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if event != nil {
		s.emit(event)
		if event["type"] == "print_start" && a != nil && matches(val(a["name"]), r) {
			s.emit(row{"type": "queue_item_acked", "queue_item_id": a["queue_id"], "printer_id": k})
		}
		s.kick()
	}
	return nil
}
func sameRun(a, r row, subtask string) bool {
	if orig := val(a["subtask"]); orig != "" && subtask != "" {
		return orig == subtask
	}
	if matches(val(a["name"]), r) {
		return true
	}
	return val(r["gcode_file"]) == "" && val(r["subtask_name"]) == ""
}
func (s *Service) openRun(ctx context.Context, tx *sql.Tx, pr, q, r row) (int64, error) {
	aid := num(q["archive_id"])
	if aid == 0 && num(q["library_file_id"]) != 0 {
		v, e := one(ctx, tx, "SELECT * FROM library_files WHERE id=?", q["library_file_id"])
		if e != nil {
			return 0, e
		}
		a, e := one(ctx, tx, "SELECT id FROM print_archives WHERE library_file_id=? AND deleted_at IS NULL ORDER BY id LIMIT 1", v["id"])
		if e == nil {
			aid = num(a["id"])
		} else if errors.Is(e, sql.ErrNoRows) {
			aid, e = insert(ctx, tx, "print_archives", row{"library_file_id": v["id"], "filename": v["filename"], "file_path": v["file_path"], "file_size": v["file_size"], "content_hash": v["file_hash"], "extra_data": v["file_metadata"], "status": "printing", "print_name": v["filename"], "is_favorite": false, "quantity": 1, "created_by_id": v["created_by_id"]})
		}
		if e != nil {
			return 0, e
		}
		_, e = tx.ExecContext(ctx, "UPDATE library_files SET print_count=print_count+1,last_printed_at=CURRENT_TIMESTAMP WHERE id=?", v["id"])
		if e != nil {
			return 0, e
		}
	}
	asset, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", aid)
	if e != nil {
		return 0, e
	}
	next, e := one(ctx, tx, "SELECT max(coalesce((SELECT max(id) FROM print_log_entries),0),coalesce((SELECT last_id FROM bb_run_sequence WHERE singleton=1),0))+1 id")
	if e != nil {
		return 0, e
	}
	run, e := insert(ctx, tx, "print_log_entries", row{"id": next["id"], "confirm_requested": flag(q["confirm_outcome"]), "archive_id": aid, "queue_item_id": q["id"], "printer_name": pr["name"], "printer_id": pr["id"], "plate_id": q["plate_id"], "print_name": asset["print_name"], "status": "printing", "started_at": time.Now().UTC().Format(time.RFC3339), "subtask_id": r["subtask_id"], "billing_run_id": q["billing_run_id"], "created_by_id": q["created_by_id"], "cost_center_id": q["cost_center_id"], "filament_type": asset["filament_type"], "filament_color": asset["filament_color"], "quantity": asset["quantity"]})
	if e != nil {
		return 0, e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET printer_id=?,status='printing',plate_id=?,started_at=CURRENT_TIMESTAMP,completed_at=NULL, user_verdict=NULL,user_verdict_source=NULL,user_verdict_at=NULL,confirm_token=NULL,confirm_token_used_at=NULL,confirm_requested=?,billing_run_id=? WHERE id=?", pr["id"], q["plate_id"], flag(q["confirm_outcome"]), q["billing_run_id"], aid); e != nil {
		return 0, e
	}
	if e = management.SnapshotRun(ctx, tx, run); e != nil {
		return 0, e
	}
	return run, management.ObserveMaterial(ctx, tx, run, num(pr["id"]), r, true, false)
}
func (s *Service) closeRun(ctx context.Context, tx *sql.Tx, pr, r row, rid, qid int64, status string, inputs management.MaterialCloseInputs) error {
	run, e := one(ctx, tx, "SELECT * FROM print_log_entries WHERE id=? AND completed_at IS NULL", rid)
	if e != nil {
		return e
	}
	var grams any
	var cost any
	a, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", run["archive_id"])
	if e != nil {
		return e
	}
	grams, e = s.measuredMaterial(ctx, tx, a, run, r, status, rid, inputs)
	if e != nil {
		return e
	}
	if qid != 0 {
		q, e := one(ctx, tx, "SELECT * FROM print_queue WHERE id=?", qid)
		if e != nil {
			return e
		}
		cost = q["estimated_cost"]
		if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET status=?,completed_at=CURRENT_TIMESTAMP WHERE id=?", status, qid); e != nil {
			return e
		}
	}
	var token any
	if status == "completed" && flag(run["confirm_requested"]) {
		token = uuid() + uuid()
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_log_entries SET confirm_token=?,status=?,completed_at=CURRENT_TIMESTAMP,duration_seconds=max(0,unixepoch()-unixepoch(started_at)),filament_used_grams=?,cost=? WHERE id=?", token, status, grams, cost, rid); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET confirm_token=?,status=?,completed_at=CURRENT_TIMESTAMP WHERE id=? AND billing_run_id=?", token, status, a["id"], run["billing_run_id"]); e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE printers SET awaiting_plate_clear=1 WHERE id=?", pr["id"]); e != nil {
		return e
	}
	return management.SettleRun(ctx, tx, rid)
}
