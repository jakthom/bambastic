package production

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/operations"
)

func heatOptions(m row) error {
	if x, ok := m["preheat_override"]; ok && !in(val(x), "inherit", "on", "off") {
		return fail(422, "Invalid preheat mode")
	}
	if x := m["preheat_chamber_target_override"]; x != nil {
		_, e := bounded(val(x), 0, 65)
		return e
	}
	return nil
}
func (s *Service) warmSetting(ctx context.Context, name string, fallback, lo, hi int64) int64 {
	v, e := access.Setting(ctx, s.db, name, fmt.Sprint(fallback))
	if e != nil {
		return fallback
	}
	n, e := bounded(v, int(lo), int(hi))
	if e != nil {
		return fallback
	}
	return int64(n)
}
func bedForPlate(file string, pl plate) int64 {
	z, e := zip.OpenReader(file)
	if e != nil {
		return 0
	}
	defer z.Close()
	b, e := readMember(z, "Metadata/project_settings.config", 8<<20)
	if e != nil {
		return 0
	}
	var config row
	if json.Unmarshal(b, &config) != nil {
		return 0
	}
	prefix := map[string]string{"Cool Plate": "cool_plate_temp", "Engineering Plate": "eng_plate_temp", "High Temp Plate": "hot_plate_temp", "Textured PEI Plate": "textured_plate_temp", "Supertack Plate": "supertack_plate_temp", "Textured Cool Plate": "textured_cool_plate_temp"}[val(config["curr_bed_type"])]
	keys := []string{"bed_temperature_initial_layer", "bed_temperature"}
	if prefix != "" {
		keys = append([]string{prefix + "_initial_layer", prefix}, keys...)
	}
	used := map[int]bool{}
	for _, f := range pl.Filaments {
		used[int(num(f["id"]))-1] = true
	}
	for _, key := range keys {
		values, isArray := config[key].([]any)
		if !isArray {
			values = []any{config[key]}
		}
		var target int64
		for i, x := range values {
			if isArray && len(used) > 0 && !used[i] {
				continue
			}
			if n := num(x); n > target && n <= 120 {
				target = n
			}
		}
		if target > 0 {
			return target
		}
	}
	return 0
}
func (s *Service) warmTargets(ctx context.Context, j, pr, rp row, file string) (int64, int64, bool) {
	mode := val(j["preheat_override"])
	if mode == "off" {
		return 0, 0, false
	}
	enabled, _ := access.Setting(ctx, s.db, "preheat_enabled", "false")
	if mode != "on" && !flag(enabled) {
		return 0, 0, false
	}
	pl, _ := j["dispatch_plate"].(plate)
	target := num(j["preheat_chamber_target_override"])
	if j["preheat_chamber_target_override"] == nil {
		targets := map[string]int64{"PLA": 0, "PETG": 0, "PETG-CF": 40, "ABS": 45, "ASA": 45, "PA": 50, "PA-CF": 55, "PC": 50, "PC-FR": 50, "TPU": 0, "PVA": 0}
		if raw, _ := access.Setting(ctx, s.db, "preheat_filament_targets", ""); raw != "" {
			var custom map[string]int64
			if json.Unmarshal([]byte(raw), &custom) == nil {
				targets = map[string]int64{}
				for k, v := range custom {
					if v >= 0 && v <= 65 {
						targets[strings.ToUpper(k)] = v
					}
				}
			}
		}
		want := map[int64]bool{}
		if a, ok := unpack(j["ams_mapping"]).([]any); ok {
			for _, x := range a {
				want[num(x)] = true
			}
		}
		materials := []string{}
		for _, tray := range liveTrays(rp) {
			if len(want) == 0 || want[tray.address] {
				materials = append(materials, tray.material)
			}
		}
		if len(materials) == 0 {
			for _, f := range pl.Filaments {
				materials = append(materials, val(f["type"]))
			}
		}
		for _, mat := range materials {
			v, ok := targets[filamentType(mat)]
			if !ok {
				v = targets["DEFAULT"]
			}
			target = max(target, v)
		}
		if target == 0 && mode != "on" {
			return 0, 0, false
		}
	}
	bed := bedForPlate(file, pl)
	if bed == 0 {
		bed = num(j["stored_bed"])
	}
	if bed <= 0 && target > 0 {
		bed = s.warmSetting(ctx, "queue_keep_warm_bed_temp", 90, 0, 120)
	}
	if bed < 1 || bed > 120 || target < 0 || target > 65 {
		return 0, 0, false
	}
	if modelName(pr["model"]) == "X1E" {
		target = min(target, 60)
	}
	return bed, target, true
}
func (s *Service) heatSnapshot(k int64, h hardware) (row, row, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[k]
	if d == nil || d.h == nil || h != nil && h != d.h || !d.h.Online() {
		return nil, nil, false
	}
	rp := row{}
	for k, x := range d.report {
		rp[k] = x
	}
	return d.printer, rp, true
}

// heatGate runs before acquiring a heater and during the soak. Ownership and
// the sensor gate may change independently of the job's initial queue admission.
func (s *Service) heatGate(ctx context.Context, j row) bool {
	qid, k := num(j["id"]), num(j["printer_id"])
	p, e := s.durableActor(ctx, num(j["created_by_id"]), num(j["origin_key"]))
	ok := e == nil && p.Has("queue:create") && p.AllowsPrinter(k)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false
	}
	defer tx.Rollback()
	var active int
	e = tx.QueryRowContext(ctx, "SELECT count(*) FROM bb_attempts a JOIN print_queue q ON q.id=a.queue_id WHERE a.printer_id=? AND a.queue_id=? AND a.phase='uploading' AND q.status='dispatching'", k, qid).Scan(&active)
	if e != nil || active != 1 {
		return false
	}
	blocked, e := operations.SensorHold(ctx, tx, k)
	if e != nil {
		return false
	}
	if ok && !blocked {
		return true
	}
	reason := "Waiting for Home Assistant sensor clearance"
	if !ok {
		reason = "Waiting for submitter authorization"
	}
	if s.requeue(ctx, tx, qid, reason, "held") == nil {
		_ = tx.Commit()
	}
	return false
}

func (s *Service) preheat(ctx context.Context, j row, file string, h hardware) (bool, error) {
	k := num(j["printer_id"])
	pr, rp, ok := s.heatSnapshot(k, h)
	if !ok {
		return false, errors.New("Printer disconnected")
	}
	bed, chamber, enabled := s.warmTargets(ctx, j, pr, rp, file)
	if !enabled {
		return true, nil
	}
	if !s.heatGate(ctx, j) {
		return false, nil
	}
	actualChamber := int64(0)
	if heater(pr) {
		actualChamber = chamber
	}
	sensor := actualChamber > 0 || in(modelName(pr["model"]), "X1", "X1C", "P2S") && chamber > 0
	wait := time.Duration(s.warmSetting(ctx, "preheat_max_wait_seconds", 900, 60, 3600)) * time.Second
	soak := time.Duration(s.warmSetting(ctx, "preheat_soak_seconds", 300, 0, 1800)) * time.Second
	qid := num(j["id"])
	s.heatMu.Lock()
	_, e := s.db.ExecContext(ctx, "INSERT INTO bb_heater_owners(printer_id,queue_id,bed,chamber,expires) VALUES(?,?,?,?,?) ON CONFLICT(printer_id) DO UPDATE SET queue_id=excluded.queue_id,bed=excluded.bed,chamber=excluded.chamber,expires=excluded.expires", k, qid, bed, actualChamber, time.Now().Add(wait+soak+46*time.Minute).Unix())
	if e == nil {
		send, stop := context.WithTimeout(ctx, 5*time.Second)
		e = h.Publish(send, line("M140 S%d", bed))
		stop()
	}
	if e == nil && actualChamber > 0 {
		send, stop := context.WithTimeout(ctx, 5*time.Second)
		if modelName(pr["model"]) == "H3C" {
			e = h.Publish(send, packet("print", "set_airduct", row{"modeId": 1, "submode": -1}))
		}
		if e == nil {
			e = h.Publish(send, line("M141 S%d", actualChamber))
		}
		stop()
	}
	s.heatMu.Unlock()
	if e != nil {
		return false, e
	}
	deadline := time.Now().Add(wait)
	var readyAt time.Time
	lastStage := ""
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		_, r, ok := s.heatSnapshot(k, h)
		if !ok || !idle(val(r["gcode_state"])) {
			return false, errors.New("Printer changed state during preheat")
		}
		if !s.heatGate(ctx, j) {
			return false, nil
		}
		bt, bok := decimal(r["bed_temper"])
		ct, cok := decimal(r["chamber_temper"])
		ready := bok && bt >= float64(bed)-2 && (!sensor || cok && ct >= float64(chamber)-2)
		if ready || time.Now().After(deadline) {
			if readyAt.IsZero() {
				readyAt = time.Now()
			}
		} else {
			readyAt = time.Time{}
		}
		stage := "Heating"
		if !readyAt.IsZero() {
			stage = "Soaking"
		}
		if stage != lastStage {
			s.db.ExecContext(ctx, "UPDATE print_queue SET waiting_reason=? WHERE id=? AND status='dispatching'", stage+" before dispatch", qid)
			s.emit(row{"type": "queue_item_preheating", "queue_item_id": qid, "printer_id": k, "stage": stage, "bed_target": bed, "chamber_target": chamber})
			lastStage = stage
		}
		if !readyAt.IsZero() && time.Since(readyAt) >= soak {
			return true, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-tick.C:
		}
	}
}

// A publish acknowledgment can precede the printer's next setpoint report.
// Keep ownership through a short reporting window before treating a different
// setpoint as an operator override, including across a service restart.
func (s *Service) waitForSetpoint(ctx context.Context, k, qid, purpose int64) bool {
	now := time.Now().Unix()
	_, err := s.db.ExecContext(ctx, "INSERT INTO bb_heat_release_wait(printer_id,purpose,queue_id,until_unix) VALUES(?,?,?,?) ON CONFLICT(printer_id,purpose) DO UPDATE SET queue_id=excluded.queue_id,until_unix=excluded.until_unix WHERE bb_heat_release_wait.queue_id<>excluded.queue_id", k, purpose, qid, now+10)
	if err != nil {
		return true
	}
	var until int64
	return s.db.QueryRowContext(ctx, "SELECT until_unix FROM bb_heat_release_wait WHERE printer_id=? AND purpose=? AND queue_id=?", k, purpose, qid).Scan(&until) != nil || now < until
}
func (s *Service) clearSetpointWait(ctx context.Context, k, qid, purpose int64) {
	_, _ = s.db.ExecContext(ctx, "DELETE FROM bb_heat_release_wait WHERE printer_id=? AND purpose=? AND queue_id=?", k, purpose, qid)
}

// releaseHeaters leaves the setpoints with a running print or with an operator
// who changed them. A disconnected device is retried after it reconnects.
func (s *Service) releaseHeaters(ctx context.Context) {
	s.heatMu.Lock()
	defer s.heatMu.Unlock()
	jobs, e := records(ctx, s.db, "SELECT h.*,a.phase,q.status FROM bb_heater_owners h LEFT JOIN bb_attempts a ON a.queue_id=h.queue_id LEFT JOIN print_queue q ON q.id=h.queue_id")
	if e != nil {
		return
	}
	for _, j := range jobs {
		k := num(j["printer_id"])
		phase := val(j["phase"])
		state := val(j["status"])
		if j["queue_id"] != nil && state == "dispatching" && in(phase, "uploading", "sent") && num(j["expires"]) > time.Now().Unix() {
			continue
		}
		pr, rp, ok := s.heatSnapshot(k, nil)
		if !ok {
			continue
		}
		if idle(val(rp["gcode_state"])) && phase != "running" {
			s.mu.RLock()
			d := s.devices[k]
			var h hardware
			if d != nil {
				h = d.h
			}
			s.mu.RUnlock()
			if h == nil {
				continue
			}
			bed, chamber := num(j["bed"]), num(j["chamber"])
			bedMatches := bed > 0 && num(rp["bed_target_temper"]) == bed
			chamberMatches := chamber > 0 && heater(pr) && num(rp["chamber_target_temper"]) == chamber
			pending := bed > 0 && !bedMatches || chamber > 0 && !chamberMatches
			c, stop := context.WithTimeout(ctx, 4*time.Second)
			var sendErr error
			if bedMatches {
				sendErr = h.Publish(c, line("M140 S0"))
			}
			if sendErr == nil && chamberMatches {
				sendErr = h.Publish(c, line("M141 S0"))
			}
			stop()
			if sendErr != nil || pending && s.waitForSetpoint(ctx, k, num(j["queue_id"]), 0) {
				continue
			}
		} else if !idle(val(rp["gcode_state"])) && !in(val(rp["gcode_state"]), "RUNNING", "PAUSE", "PREPARE") {
			continue
		}
		if _, e := s.db.ExecContext(ctx, "DELETE FROM bb_heater_owners WHERE printer_id=? AND queue_id=?", k, j["queue_id"]); e == nil {
			s.clearSetpointWait(ctx, k, num(j["queue_id"]), 0)
		}
	}
}

func (s *Service) keepWarm(ctx context.Context) {
	s.heatMu.Lock()
	defer s.heatMu.Unlock()
	enabled := true
	for _, setting := range []string{"preheat_enabled", "require_plate_clear", "queue_keep_bed_warm"} {
		raw, _ := access.Setting(ctx, s.db, setting, "false")
		if !flag(raw) {
			enabled = false
			break
		}
	}
	existing, e := records(ctx, s.db, "SELECT * FROM bb_keep_warm")
	if e != nil {
		return
	}
	held := map[int64]row{}
	for _, r := range existing {
		held[num(r["printer_id"])] = r
	}
	candidates := map[int64]bool{}
	fleet := []row{}
	if enabled {
		fleet, e = records(ctx, s.db, "SELECT * FROM printers WHERE is_active AND awaiting_plate_clear")
		if e != nil {
			return
		}
	}
	for _, pr := range fleet {
		k := num(pr["id"])
		_, rp, online := s.heatSnapshot(k, nil)
		if !online || val(rp["gcode_state"]) != "FINISH" {
			continue
		}
		j, e := one(ctx, s.db, "SELECT q.*,(SELECT key_id FROM bb_queue_actor WHERE queue_id=q.id) origin_key FROM print_queue q WHERE printer_id=? AND status='pending' AND NOT manual_start ORDER BY position,id LIMIT 1", k)
		if e != nil || val(j["preheat_override"]) == "off" {
			continue
		}
		if when := val(j["scheduled_time"]); when != "" {
			t, e := time.Parse(time.RFC3339, when)
			if e != nil || t.After(time.Now()) {
				continue
			}
		}
		p, e := s.durableActor(ctx, num(j["created_by_id"]), num(j["origin_key"]))
		if e != nil || !p.Has("queue:create") || !p.AllowsPrinter(k) {
			continue
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return
		}
		blocked, e := operations.SensorHold(ctx, tx, k)
		tx.Rollback()
		if e != nil || blocked {
			continue
		}
		item := held[k]
		if item != nil && num(item["queue_id"]) == num(j["id"]) {
			candidates[k] = true
			continue
		}
		if item != nil {
			continue
		}
		kind, aid := "archives", num(j["archive_id"])
		if aid == 0 {
			kind, aid = "library", num(j["library_file_id"])
		}
		a, e := s.getAsset(ctx, s.db, p, kind, aid, "read")
		if e != nil {
			continue
		}
		info, e := s.meta(kind, a)
		if e != nil {
			continue
		}
		for _, pl := range info.Plates {
			if pl.Index == num(j["plate_id"]) {
				j["dispatch_plate"] = pl
				break
			}
		}
		file, e := s.assetPath(a["file_path"])
		if e != nil {
			continue
		}
		j["stored_bed"] = a["bed_temperature"]
		bed, chamber, on := s.warmTargets(ctx, j, pr, rp, file)
		if !on || chamber <= 0 {
			continue
		}
		target := max(bed, s.warmSetting(ctx, "queue_keep_warm_bed_temp", 90, 40, 110))
		until := time.Now().Unix() + 60*s.warmSetting(ctx, "queue_keep_warm_max_minutes", 120, 5, 480)
		res, e := s.db.ExecContext(ctx, "INSERT INTO bb_keep_warm(printer_id,queue_id,bed,expires) VALUES(?,?,?,?) ON CONFLICT(printer_id) DO NOTHING", k, j["id"], target, until)
		if e != nil {
			continue
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			continue
		}
		candidates[k] = true
		s.mu.RLock()
		d := s.devices[k]
		var h hardware
		if d != nil {
			h = d.h
		}
		s.mu.RUnlock()
		if h != nil && num(rp["bed_target_temper"]) != target {
			c, stop := context.WithTimeout(ctx, 4*time.Second)
			_ = h.Publish(c, line("M140 S%d", target))
			stop()
		}
	}
	for _, item := range existing {
		k := num(item["printer_id"])
		eligible := candidates[k]
		if eligible && (flag(item["released"]) || time.Now().Unix() < num(item["expires"])) {
			continue
		}
		_, rp, ok := s.heatSnapshot(k, nil)
		if !ok {
			continue
		}
		if !flag(item["released"]) && idle(val(rp["gcode_state"])) {
			if num(rp["bed_target_temper"]) != num(item["bed"]) {
				if s.waitForSetpoint(ctx, k, num(item["queue_id"]), 1) {
					continue
				}
			} else {
				s.mu.RLock()
				d := s.devices[k]
				var h hardware
				if d != nil {
					h = d.h
				}
				s.mu.RUnlock()
				if h == nil {
					continue
				}
				c, stop := context.WithTimeout(ctx, 4*time.Second)
				e = h.Publish(c, line("M140 S0"))
				stop()
				if e != nil {
					continue
				}
			}
		}
		if eligible {
			_, _ = s.db.ExecContext(ctx, "UPDATE bb_keep_warm SET released=1 WHERE printer_id=?", k)
		} else {
			_, _ = s.db.ExecContext(ctx, "DELETE FROM bb_keep_warm WHERE printer_id=?", k)
		}
		s.clearSetpointWait(ctx, k, num(item["queue_id"]), 1)
	}
}
