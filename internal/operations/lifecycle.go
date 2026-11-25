package operations

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"time"
)

type reading struct {
	online               bool
	filamentProblem, hms bool
	state                string
	nozzle               *float64
	at                   time.Time
}

func (s *Service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	sensors := make(chan struct{})
	lights := make(chan struct{})
	stock := make(chan struct{})
	go func() { defer close(stock); s.stockLoop(ctx) }()
	go func() { defer close(lights); s.lightPoll(ctx) }()
	go func() { defer close(sensors); s.sensorPoll(ctx) }()
	go func() {
		defer func() { <-sensors; <-lights; <-stock; s.plugDiscovery.close(); close(done) }()
		t := time.NewTicker(10 * time.Second)
		defer t.Stop()
		for {
			if e := s.process(ctx); e != nil && ctx.Err() == nil {
				log.Printf("operations worker: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-s.wake:
			}
		}
	}()
	return done
}

// Publish receives committed domain events. Device I/O is performed by the worker after persistence.
func (s *Service) Publish(event map[string]any) {
	kind := str(event["type"])
	data := sub(event["data"])
	pid := int64(num(event["printer_id"]))
	if pid == 0 {
		pid = int64(num(data["printer_id"]))
	}
	if kind == "printer_status" {
		if pid <= 0 {
			return
		}
		v := reading{online: flag(data["connected"]), state: str(data["state"]), at: time.Now()}
		v.filamentProblem = num(data["stg_cur"]) == 6
		v.hms = wledFaults(data["hms_errors"])
		if nozzle := sub(data["temperatures"])["nozzle"]; nozzle != nil {
			n := num(nozzle)
			v.nozzle = &n
		}
		s.mu.Lock()
		s.telemetry[pid] = v
		s.mu.Unlock()
		return
	}
	switch kind {
	case "print_complete":
		switch str(data["status"]) {
		case "failed":
			kind = "print_failed"
		case "cancelled", "aborted", "stopped":
			kind = "print_stopped"
		}
	case "queue_item_failed":
		kind = "queue_job_failed"
	case "queue_item_acked":
		kind = "queue_job_started"
	}
	if _, supported := schemas["NotificationProviderCreate"].Properties["on_"+kind]; !supported {
		return
	}
	if e := s.onEvent(context.Background(), kind, pid, event, data); e != nil {
		log.Printf("operations event %s: %v", kind, e)
		return
	}
	s.signal()
}
func localClock(ctx context.Context, db *sql.DB) time.Time {
	z := setting(ctx, db, "timezone")
	if tz, e := time.LoadLocation(z); z != "" && e == nil {
		return time.Now().In(tz)
	}
	return time.Now()
}
func quiet(p obj, when time.Time) bool {
	if !flag(p["quiet_hours_enabled"]) {
		return false
	}
	s, e := str(p["quiet_hours_start"]), str(p["quiet_hours_end"])
	if s == "" || e == "" || s == e {
		return false
	}
	v := when.Format("15:04")
	if s < e {
		return v >= s && v < e
	}
	return v >= s || v < e
}
func (s *Service) onEvent(ctx context.Context, kind string, pid int64, evt, data obj) error {
	vars := objectCopy(data)
	for k, v := range evt {
		if k != "data" && k != "type" {
			vars[k] = v
		}
	}
	rid := int64(num(vars["run_id"]))
	var run any
	if rid > 0 {
		r, e := s.get(ctx, "print_log_entries", rid)
		if e != nil {
			return e
		}
		for _, k := range []string{"printer_name", "print_name", "filament_type", "duration_seconds", "filament_used_grams", "failure_reason", "cost", "quantity"} {
			vars[k] = r[k]
		}
		if r["failure_reason"] != nil {
			vars["reason"] = r["failure_reason"]
		}
		vars["duration"] = time.Duration(int64(num(r["duration_seconds"])) * int64(time.Second)).String()
		vars["filament_used"] = r["filament_used_grams"]
		run = rid
	}
	var printer any
	if pid > 0 {
		printer = pid
		if vars["printer_name"] == nil {
			var name string
			if e := s.db.QueryRowContext(ctx, "SELECT name FROM printers WHERE id=?", pid).Scan(&name); e != nil {
				return e
			}
			vars["printer_name"] = name
		}
	}
	vars["filename"] = vars["print_name"]
	vars["printer"] = vars["printer_name"]
	vars["filament_grams"] = vars["filament_used_grams"]
	var title, body string
	e := s.db.QueryRowContext(ctx, "SELECT title_template,body_template FROM notification_templates WHERE event_type=?", kind).Scan(&title, &body)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if e != nil {
		title = strings.ReplaceAll(kind, "_", " ")
		body = title
	}
	title = render(title, vars)
	body = render(body, vars)
	providers, e := s.list(ctx, "SELECT * FROM notification_providers WHERE enabled AND on_"+kind+" AND (printer_id IS NULL OR printer_id=?)", printer)
	if e != nil {
		return e
	}
	photo := ""
	if b, ok := data["image_data"].([]byte); ok && len(b) > 0 {
		for _, provider := range providers {
			if flag(provider["attach_photo"]) {
				photo, _ = s.savePhoto(b, kind)
				break
			}
		}
	}
	when := localClock(ctx, s.db)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	notice := queuedNotice{kind: kind, title: title, body: body, photo: photo, printerName: str(vars["printer_name"]), printer: printer, run: run, when: when}
	for _, p := range providers {
		if e = notice.enqueue(ctx, tx, p); e != nil {
			return e
		}
	}

	if run != nil && kind == "print_start" {
		if _, e = tx.Exec("UPDATE smart_plugs SET auto_off_pending=0,auto_off_pending_since=NULL,auto_off_executed=0 WHERE printer_id=?", pid); e != nil {
			return e
		}
		_, e = tx.Exec("INSERT OR IGNORE INTO bambastic_power_actions(plug_id,run_id,action) SELECT id,?,'on' FROM smart_plugs WHERE enabled AND auto_on AND printer_id=?", run, pid)
		if e != nil {
			return e
		}
	}
	if run != nil && kind == "print_complete" {
		if _, e = tx.Exec(`UPDATE smart_plugs SET auto_off_pending=1,auto_off_pending_since=CURRENT_TIMESTAMP WHERE id IN
  (SELECT p.id FROM smart_plugs p WHERE p.printer_id=? AND p.enabled AND (p.auto_off OR EXISTS(SELECT 1 FROM print_log_entries l JOIN print_queue q ON q.id=l.queue_item_id WHERE l.id=? AND q.auto_off_after))
  AND NOT EXISTS(SELECT 1 FROM bambastic_power_actions a WHERE a.plug_id=p.id AND a.run_id=? AND a.action='arm'))`, pid, run, run); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT OR IGNORE INTO bambastic_power_actions(plug_id,run_id,action,state) SELECT id,?,'arm','done' FROM smart_plugs WHERE printer_id=? AND auto_off_pending", run, pid); e != nil {
			return e
		}
	}
	if run != nil && pid > 0 {
		bound := ""
		if kind == "print_start" {
			bound = "start"
		} else if kind == "print_complete" || kind == "print_failed" || kind == "print_stopped" {
			bound = "end"
		}
		if bound != "" {
			if _, e = tx.Exec("INSERT OR IGNORE INTO bambastic_energy_samples(run_id,boundary) VALUES(?,?)", run, bound); e != nil {
				return e
			}
		}
	}
	if e := s.queuePersonal(ctx, tx, kind, rid, title, body); e != nil {
		return e
	}
	return tx.Commit()
}

type queuedNotice struct {
	kind, title, body, photo, printerName string
	printer, run                          any
	when                                  time.Time
}

func (v queuedNotice) enqueue(ctx context.Context, tx *sql.Tx, p obj) error {
	state := "pending"
	if quiet(p, v.when) {
		state = "quiet"
	}
	res, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bambastic_notification_delivery(provider_id,event_type,printer_id,run_id,title,message,state) VALUES(?,?,?,?,?,?,?)", p["id"], v.kind, v.printer, v.run, v.title, v.body, state)
	if e != nil {
		return e
	}
	count, e := res.RowsAffected()
	if e != nil || count == 0 {
		return e
	}
	if v.photo != "" && flag(p["attach_photo"]) {
		id, e := res.LastInsertId()
		if e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "INSERT INTO bambastic_notification_photos(delivery_id,name) VALUES(?,?)", id, v.photo); e != nil {
			return e
		}
	}
	if flag(p["daily_digest_enabled"]) && str(p["daily_digest_time"]) != "" {
		_, e = tx.ExecContext(ctx, "INSERT INTO notification_digest_queue(provider_id,event_type,title,message,printer_id,printer_name,created_at) VALUES(?,?,?,?,?,?,CURRENT_TIMESTAMP)", p["id"], v.kind, v.title, v.body, v.printer, v.printerName)
	}
	return e
}

func (s *Service) process(ctx context.Context) error {
	if e := s.personalEmail(ctx); e != nil {
		return e
	}
	if e := s.energySamples(ctx); e != nil {
		return e
	}
	pending, e := s.list(ctx, "SELECT * FROM bambastic_notification_delivery WHERE state='pending' ORDER BY id LIMIT 64")
	if e != nil {
		return e
	}
	for _, v := range pending {
		p, e := s.get(ctx, providerTable, v["provider_id"])
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return e
		}
		state := "sending"
		if !flag(p["enabled"]) || p["printer_id"] != nil && !fleetStock(str(v["event_type"])) && str(v["printer_id"]) != str(p["printer_id"]) {
			state = "skipped"
		}
		res, e := s.db.ExecContext(ctx, "UPDATE bambastic_notification_delivery SET state=? WHERE id=? AND state='pending'", state, v["id"])
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n == 0 || state == "skipped" {
			continue
		}
		e = s.deliver(ctx, str(p["provider_type"]), sub(p["config"]), str(v["title"]), str(v["message"]), str(v["event_type"]), v["printer_id"], s.deliveryImage(ctx, v, p))
		if err := s.logDelivery(ctx, p, str(v["event_type"]), str(v["title"]), str(v["message"]), v["printer_id"], e); err != nil {
			return err
		}
		state = "sent"
		if e != nil {
			state = "failed"
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE bambastic_notification_delivery SET state=? WHERE id=?", state, v["id"]); e != nil {
			return e
		}
	}
	if e = s.digest(ctx); e != nil {
		return e
	}
	actions, e := s.list(ctx, "SELECT * FROM bambastic_power_actions WHERE state='pending' ORDER BY id LIMIT 32")
	if e != nil {
		return e
	}
	for _, a := range actions {
		p, e := s.get(ctx, plugTable, a["plug_id"])
		if e != nil {
			continue
		}
		state := "cancelled"
		if flag(p["enabled"]) {
			var active int
			if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM print_log_entries WHERE id=? AND completed_at IS NULL", a["run_id"]).Scan(&active); e != nil {
				return e
			}
			if active > 0 {
				e = s.control(ctx, p, "on")
				if e != nil {
					state = "failed"
				} else {
					state = "done"
				}
			}
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE bambastic_power_actions SET state=? WHERE id=?", state, a["id"]); e != nil {
			return e
		}
	}
	return s.pendingPower(ctx)
}
func (s *Service) pendingPower(ctx context.Context) error {
	plugs, e := s.list(ctx, `SELECT *, (julianday('now')-julianday(auto_off_pending_since))*1440 age FROM smart_plugs WHERE enabled AND auto_off_pending`)
	if e != nil {
		return e
	}
	for _, p := range plugs {
		age := num(p["age"])
		if age >= 1440 {
			if _, e = s.db.ExecContext(ctx, "UPDATE smart_plugs SET auto_off_pending=0 WHERE id=?", p["id"]); e != nil {
				return e
			}
			continue
		}
		pid := int64(num(p["printer_id"]))
		s.mu.RLock()
		last := s.telemetry[pid]
		s.mu.RUnlock()
		if !last.online || time.Since(last.at) > 3*time.Minute || (last.state != "IDLE" && last.state != "FINISH") {
			continue
		}
		var jobs int
		if e = s.db.QueryRowContext(ctx, "SELECT count(*) FROM print_log_entries WHERE printer_id=? AND completed_at IS NULL AND status IN ('printing','paused','running')", pid).Scan(&jobs); e != nil {
			return e
		}
		if jobs > 0 {
			continue
		}
		if p["off_delay_mode"] == "temperature" {
			if last.nozzle == nil || math.IsNaN(*last.nozzle) || *last.nozzle <= 0 || *last.nozzle > num(p["off_temp_threshold"]) {
				continue
			}
		} else if age < num(p["off_delay_minutes"]) {
			continue
		}
		if e = s.control(ctx, p, "off"); e != nil {
			continue
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE smart_plugs SET auto_off_executed=1,auto_off=CASE WHEN auto_off_persistent THEN auto_off ELSE FALSE END WHERE id=?", p["id"]); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) digest(ctx context.Context) error {
	when := localClock(ctx, s.db)
	day := when.Format("2006-01-02")
	ps, e := s.list(ctx, `SELECT p.* FROM notification_providers p WHERE enabled AND daily_digest_enabled AND daily_digest_time=?
 AND NOT EXISTS(SELECT 1 FROM bambastic_digest_days d WHERE d.provider_id=p.id AND d.day=?)`, when.Format("15:04"), day)
	if e != nil {
		return e
	}
	for _, p := range ps {
		v, e := s.list(ctx, `SELECT * FROM notification_digest_queue WHERE provider_id=? AND julianday(created_at)>julianday('now','-7 days') ORDER BY id LIMIT 256`, p["id"])
		if e != nil {
			return e
		}
		if len(v) == 0 {
			continue
		}
		lines := []string{}
		for _, row := range v {
			lines = append(lines, str(row["title"])+": "+str(row["message"]))
		}
		msg := strings.Join(lines, "\n")
		title := fmt.Sprintf("Daily digest · %d events", len(v))
		sendErr := s.deliver(ctx, str(p["provider_type"]), sub(p["config"]), title, msg, "daily_digest", p["printer_id"])
		if e = s.logDelivery(ctx, p, "daily_digest", title, msg, p["printer_id"], sendErr); e != nil {
			return e
		}
		if sendErr != nil {
			continue
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		_, e = tx.Exec("DELETE FROM notification_digest_queue WHERE provider_id=? AND id<=?", p["id"], v[len(v)-1]["id"])
		if e == nil {
			_, e = tx.Exec("INSERT INTO bambastic_digest_days(provider_id,day) VALUES(?,?) ON CONFLICT(provider_id) DO UPDATE SET day=excluded.day", p["id"], day)
		}
		if e != nil {
			tx.Rollback()
			return e
		}
		if e = tx.Commit(); e != nil {
			return e
		}
	}
	_, e = s.db.ExecContext(ctx, "DELETE FROM notification_digest_queue WHERE julianday(created_at)<julianday('now','-7 days')")
	return e
}
