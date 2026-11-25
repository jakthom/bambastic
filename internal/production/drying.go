package production

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"bambastic/internal/access"
)

var dryingFirmware = map[string]string{"X1": "01.09.00.00", "X1C": "01.09.00.00", "H2D": "01.02.30.00", "H2C": "01.02.00.00", "H2S": "01.02.00.00", "P2S": "01.02.00.00"}
var concurrentFirmware = map[string]string{"X1C": "01.11.02.00", "H2D": "01.03.00.00", "H2DPRO": "01.02.00.00", "H2C": "01.02.00.00", "H2S": "01.02.00.00", "P2S": "01.02.00.00", "X2D": "01.01.00.00", "A2L": "01.01.00.00"}

func dryerSupport(model, firmware string, live bool) string {
	if in(model, "P1P", "P1S") {
		return "screen_only"
	}
	if in(model, "", "A1", "A1MINI") || live && firmware < dryingFirmware[model] {
		return "unsupported"
	}
	return ""
}
func dryerUnit(src map[string]any) row {
	u := row{}
	for k, v := range src {
		u[k] = v
	}
	zones, _ := u["zone_info"].([]any)
	for _, raw := range zones {
		z, _ := raw.(map[string]any)
		slots, _ := z["zone_slots"].([]any)
		if len(slots) != 2 {
			continue
		}
		for _, k := range []string{"dry_time", "dry_setting", "dry_sf_reason", "info"} {
			if z[k] != nil {
				u[k] = z[k]
			}
		}
		u["dry_zone_id"] = z["zone_id"]
		break
	}
	if u["info"] != nil {
		u["dry_status"] = int((faultValue(u["info"]) >> 4) & 15)
	}
	return u
}
func activeDry(u row) bool { return num(u["dry_time"]) > 0 || in(val(u["dry_status"]), "1", "2", "3") }

type dryerState struct {
	h                hardware
	unit             row
	anyDry           bool
	state, fw, model string
}

func (s *Service) dryer(pr row, unit int64) dryerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[num(pr["id"])]
	o := dryerState{model: modelName(pr["model"])}
	if d == nil || d.h == nil || !d.h.Online() {
		return o
	}
	o.h = d.h
	o.state = val(d.report["gcode_state"])
	o.fw = versionText.FindString(val(d.report["firmware_version"]))
	a, _ := d.report["ams"].(map[string]any)
	us, _ := a["ams"].([]any)
	for _, raw := range us {
		src, _ := raw.(map[string]any)
		u := dryerUnit(src)
		if num(u["id"]) == unit {
			o.unit = u
		}
		o.anyDry = o.anyDry || activeDry(u)
	}
	return o
}
func dryParams(m row) (row, error) {
	ams := num(m["ams_id"])
	if ams < 0 || ams > 3 && ams != 32 && (ams < 128 || ams > 135) {
		return nil, fail(422, "Unsupported AMS dryer unit")
	}
	temp, e := bounded(val(m["temp"]), 45, 85)
	if e != nil {
		return nil, e
	}
	hours, e := bounded(val(m["duration_hours"]), 1, 24)
	if e != nil {
		return nil, e
	}
	fil := strings.TrimSpace(val(m["filament"]))
	if len(fil) > 50 {
		return nil, fail(422, "Invalid filament name")
	}
	return row{"ams_id": ams, "temp": temp, "duration_hours": hours, "rotate_tray": flag(m["rotate_tray"]), "filament": fil}, nil
}
func dryersBlocked(u row) string {
	codes, _ := u["dry_sf_reason"].([]any)
	reason := ""
	for _, v := range codes {
		n := num(v)
		switch n {
		case 1, 8:
			return "ams_power_required"
		case 3:
			reason = "ams_retract_filament"
		case 0, 2, 4, 5, 6, 7:
			if reason == "" {
				reason = "ams_blocked"
			}
		}
	}
	return reason
}
func dryReady(d dryerState, scheduled bool) string {
	if d.h == nil {
		return "printer_offline"
	}
	if v := dryerSupport(d.model, d.fw, true); v != "" {
		return v
	}
	if d.unit == nil {
		return "ams_not_found"
	}
	if scheduled && d.anyDry || activeDry(d.unit) {
		return "already_drying"
	}
	if !idle(d.state) && (scheduled || concurrentFirmware[d.model] == "" || d.fw < concurrentFirmware[d.model]) {
		return "printer_busy"
	}
	return dryersBlocked(d.unit)
}
func sendDry(ctx context.Context, d dryerState, v row, on bool) error {
	if d.h == nil {
		return fail(409, "Printer is disconnected")
	}
	fields := row{"ams_id": v["ams_id"], "temp": 0, "duration": 0, "cooling_temp": 0, "mode": 0, "humidity": 0, "rotate_tray": false, "filament": "", "close_power_conflict": false}
	if on {
		material := val(v["filament"])
		if material == "" {
			trays, _ := d.unit["tray"].([]any)
			for _, raw := range trays {
				t, _ := raw.(map[string]any)
				if val(t["tray_type"]) != "" {
					material = val(t["tray_type"])
					break
				}
			}
			if material == "" {
				material = "PLA"
			}
		}
		fields["mode"] = 1
		fields["temp"] = v["temp"]
		fields["duration"] = v["duration_hours"]
		fields["filament"] = material
		fields["cooling_temp"] = 20
		fields["rotate_tray"] = flag(v["rotate_tray"])
		if num(v["ams_id"]) == 32 {
			fields["rotate_tray"] = false
			fields["zone_id"] = num(d.unit["dry_zone_id"])
			fields["cooling_temp"] = 50
			typ := strings.FieldsFunc(strings.ToUpper(material), func(r rune) bool { return r == ' ' || r == '-' })
			if len(typ) > 0 {
				if n := map[string]int{"PLA": 65, "PETG": 75, "ABS": 95, "ASA": 95, "TPU": 70, "PA": 150, "PC": 120}[typ[0]]; n > 0 {
					fields["cooling_temp"] = n
				}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if e := d.h.Publish(ctx, packet("print", "ams_filament_drying", fields)); e != nil {
		return fail(502, "Printer drying command delivery failed")
	}
	return nil
}
func (s *Service) dryRoutes(rt register) {
	rt("POST", "/printers/{id}/drying/start", s.manualDry)
	rt("POST", "/printers/{id}/drying/stop", s.manualDry)
	rt("GET", "/scheduled-dryings", s.drySchedules)
	rt("POST", "/scheduled-dryings", s.drySchedules)
	rt("DELETE", "/scheduled-dryings/{id}", s.drySchedules)
}
func (s *Service) manualDry(w http.ResponseWriter, r *http.Request) (any, error) {
	pr, e := s.printerRow(r, "printers:control")
	if e != nil {
		return nil, e
	}
	s.dryMu.Lock()
	defer s.dryMu.Unlock()
	q := r.URL.Query()
	unit, e := bounded(q.Get("ams_id"), 0, 135)
	if e != nil {
		return nil, e
	}
	d := s.dryer(pr, int64(unit))
	if in(d.model, "P1P", "P1S") {
		return nil, fail(400, "Use the printer display for AMS drying")
	}
	on := strings.HasSuffix(r.URL.Path, "/start")
	v := row{"ams_id": unit, "temp": 45, "duration_hours": 4, "filament": q.Get("filament"), "rotate_tray": flag(q.Get("rotate_tray"))}
	if q.Has("temp") {
		v["temp"] = q.Get("temp")
	}
	if q.Has("duration") {
		v["duration_hours"] = q.Get("duration")
	}
	v, e = dryParams(v)
	if e != nil {
		return nil, e
	}
	if on {
		if reason := dryReady(d, false); reason != "" {
			return nil, fail(409, reason)
		}
	}
	if e = sendDry(r.Context(), d, v, on); e != nil {
		return nil, e
	}
	if !on {
		_, e = s.db.ExecContext(r.Context(), "UPDATE scheduled_dryings SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE printer_id=? AND ams_id=? AND status='running'", pr["id"], unit)
		return row{"status": "drying_stopped", "ams_id": unit}, e
	}
	return row{"status": "drying_started", "ams_id": unit, "temp": v["temp"], "duration": v["duration_hours"]}, nil
}
func (s *Service) drySchedules(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if r.Method == "GET" {
		if !p.Has("printers:read") {
			return nil, fail(403, "Printer read permission required")
		}
		rs, e := records(ctx, s.db, "SELECT * FROM scheduled_dryings WHERE status IN('pending','running','failed') ORDER BY start_after,id")
		if e != nil {
			return nil, e
		}
		out := []row{}
		for _, v := range rs {
			k := num(v["printer_id"])
			if p.AllowsPrinter(k) && (r.URL.Query().Get("printer_id") == "" || num(r.URL.Query().Get("printer_id")) == k) {
				out = append(out, v)
			}
		}
		return out, nil
	}
	if !p.Has("printers:control") {
		return nil, fail(403, "Printer control permission required")
	}
	s.dryMu.Lock()
	defer s.dryMu.Unlock()
	if r.Method == "DELETE" {
		j, e := one(ctx, s.db, "SELECT * FROM scheduled_dryings WHERE id=?", pathID(r))
		if e != nil {
			return nil, e
		}
		if !p.AllowsPrinter(num(j["printer_id"])) {
			return nil, sql.ErrNoRows
		}
		if j["status"] == "failed" {
			_, e = s.db.ExecContext(ctx, "DELETE FROM scheduled_dryings WHERE id=?", j["id"])
			return row{"status": "dismissed", "id": j["id"]}, e
		}
		if !in(val(j["status"]), "pending", "running") {
			return nil, fail(409, "Drying is already finished")
		}
		if j["status"] == "running" {
			pr, e := one(ctx, s.db, "SELECT * FROM printers WHERE id=?", j["printer_id"])
			if e != nil {
				return nil, e
			}
			d := s.dryer(pr, num(j["ams_id"]))
			if e = sendDry(ctx, d, j, false); e != nil {
				return nil, e
			}
		}
		_, e = s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET status='cancelled',completed_at=CURRENT_TIMESTAMP WHERE id=?", j["id"])
		return row{"status": "cancelled", "id": j["id"]}, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	k := num(m["printer_id"])
	if !p.AllowsPrinter(k) {
		return nil, sql.ErrNoRows
	}
	pr, e := one(ctx, s.db, "SELECT * FROM printers WHERE id=?", k)
	if e != nil {
		return nil, e
	}
	v, e := dryParams(m)
	if e != nil {
		return nil, e
	}
	d := s.dryer(pr, num(v["ams_id"]))
	if reason := dryerSupport(d.model, d.fw, d.h != nil); reason != "" {
		return nil, fail(400, reason)
	}
	if m["start_after"] != nil {
		when, e := time.Parse(time.RFC3339, val(m["start_after"]))
		if e != nil || !time.Now().Before(when) {
			return nil, fail(422, "Choose a future start time")
		}
		v["start_after"] = when.UTC().Format(time.RFC3339)
	}
	v["printer_id"] = k
	v["created_by_id"] = id(p.ID)
	v["status"] = "pending"
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	n, e := insert(ctx, tx, "scheduled_dryings", v)
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO bb_dry_progress(dry_id,key_id) VALUES(?,?)", n, p.KeyID); e != nil {
		return nil, e
	}
	j, e := one(ctx, tx, "SELECT * FROM scheduled_dryings WHERE id=?", n)
	if e != nil {
		return nil, e
	}
	return j, tx.Commit()
}
func (s *Service) dryingLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.tickDry(ctx)
		}
	}
}
func (s *Service) tickDry(ctx context.Context) {
	s.dryMu.Lock()
	defer s.dryMu.Unlock()
	rs, e := records(ctx, s.db, `SELECT s.*,unixepoch()-unixepoch(started_at) elapsed,d.key_id,d.remaining,d.changed_at,d.observed FROM scheduled_dryings s LEFT JOIN bb_dry_progress d ON d.dry_id=s.id WHERE status IN('pending','running') AND(start_after IS NULL OR unixepoch(start_after)<=unixepoch()) ORDER BY start_after,s.id`)
	if e != nil {
		return
	}
	for _, j := range rs {
		pr, e := one(ctx, s.db, "SELECT * FROM printers WHERE id=?", j["printer_id"])
		if e != nil {
			continue
		}
		d := s.dryer(pr, num(j["ams_id"]))
		if j["status"] == "running" {
			s.trackDry(ctx, d, j)
			continue
		}
		p, e := s.durableActor(ctx, num(j["created_by_id"]), num(j["key_id"]))
		reason := dryReady(d, true)
		if e != nil || !p.Has("printers:control") || !p.AllowsPrinter(num(pr["id"])) {
			reason = "permission_revoked"
		}
		if in(reason, "unsupported", "screen_only", "permission_revoked") {
			s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET status='failed',error_code=?,error_message=?,completed_at=CURRENT_TIMESTAMP WHERE id=?", reason, reason, j["id"])
			continue
		}
		if reason == "" {
			var n int
			s.db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM scheduled_dryings WHERE printer_id=? AND status='running')+(SELECT count(*) FROM bb_attempts WHERE printer_id=? AND phase IN('uploading','sent','running'))`, pr["id"], pr["id"]).Scan(&n)
			if n > 0 {
				reason = "printer_busy"
			}
		}
		if reason != "" {
			if j["waiting_reason"] != reason {
				s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET waiting_reason=? WHERE id=?", reason, j["id"])
			}
			continue
		}
		res, e := s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET status='running',started_at=CURRENT_TIMESTAMP,waiting_reason=NULL WHERE id=? AND status='pending'", j["id"])
		if e != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue
		}
		if e = sendDry(ctx, d, j, true); e != nil {
			s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET error_message='Start delivery uncertain; waiting for AMS telemetry' WHERE id=?", j["id"])
		}
	}
}
func (s *Service) trackDry(ctx context.Context, d dryerState, j row) {
	if d.h == nil || d.unit == nil {
		return
	}
	seconds := num(j["elapsed"])
	n := num(d.unit["dry_time"])
	phase := num(d.unit["dry_status"])
	running := phase >= 1 && phase <= 3
	if running || n > 0 {
		if running || j["remaining"] == nil || n != num(j["remaining"]) {
			s.db.ExecContext(ctx, "INSERT INTO bb_dry_progress(dry_id,remaining,changed_at,observed) VALUES(?,?,unixepoch(),?) ON CONFLICT(dry_id) DO UPDATE SET remaining=excluded.remaining,changed_at=excluded.changed_at,observed=max(bb_dry_progress.observed,excluded.observed)", j["id"], n, running || j["remaining"] != nil && num(j["remaining"]) != n)
			return
		}
		if time.Now().Unix()-num(j["changed_at"]) < 150 {
			return
		}
	}
	if seconds < 120 {
		return
	}
	state := "failed"
	reason := "did_not_start"
	if seconds >= num(j["duration_hours"])*3240 {
		state = "completed"
		reason = ""
	} else if active(d.state) {
		state = "pending"
		reason = "interrupted"
	} else if flag(j["observed"]) {
		state = "cancelled"
		reason = ""
	}
	if state == "pending" {
		s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET status='pending',started_at=NULL,waiting_reason=? WHERE id=?", reason, j["id"])
		s.db.ExecContext(ctx, "UPDATE bb_dry_progress SET remaining=NULL,observed=0,changed_at=NULL WHERE dry_id=?", j["id"])
		return
	}
	s.db.ExecContext(ctx, "UPDATE scheduled_dryings SET status=?,error_code=NULLIF(?,''),error_message=NULLIF(?,''),completed_at=CURRENT_TIMESTAMP WHERE id=?", state, reason, reason, j["id"])
}
func (s *Service) durableActor(ctx context.Context, owner, key int64) (access.Principal, error) {
	if key == 0 {
		return s.owner(ctx, owner)
	}
	if s.auth == nil {
		return access.Principal{}, errors.New("API key authority unavailable")
	}
	p, e := s.auth.KeyByID(ctx, key)
	if e == nil && p.ID != owner {
		return access.Principal{}, errors.New("API key owner changed")
	}
	return p, e
}
