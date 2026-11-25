package production

import (
	"context"
	"errors"
	"strings"
	"time"
)

// OnDetection publishes evidence for the current running print. A requested
// pause additionally requires an exact device-reported subtask ID.
func (s *Service) OnDetection(ctx context.Context, id int64, task, name, mode, provider, verdict string) error {
	s.mu.RLock()
	d := s.devices[id]
	if d == nil || d.h == nil || !d.h.Online() || strings.ToUpper(val(d.report["gcode_state"])) != "RUNNING" || val(d.report["subtask_name"]) != name || val(d.report["subtask_id"]) != task {
		s.mu.RUnlock()
		return errors.New("The active print changed")
	}
	h := d.h
	s.mu.RUnlock()
	message := "An inspection reported " + verdict
	var err error
	if mode != "notify" {
		if task == "" || task == "0" {
			err = errors.New("A verified print ID is required to pause")
		} else if mode == "pause" || mode == "pause_and_off" {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			err = h.Publish(c, row{"print": row{"command": "pause", "sequence_id": "2000", "param": ""}})
			cancel()
			if err == nil {
				message += "; pause sent"
				if mode == "pause_and_off" {
					message += "; power-off requested for the assigned outlet"
				}
			}
		} else {
			err = errors.New("Unsupported detection action")
		}
	}
	if err != nil {
		message += ". " + err.Error()
	}
	s.emit(row{"type": "ai_failure_detection", "printer_id": id, "data": map[string]any{"printer_id": id, "task_name": name, "filename": name, "provider": provider, "action": mode, "class": verdict, "message": message}})
	return err
}

// OnPlateInspection rechecks current settings, run, subtask and layer after
// camera I/O. A stale recording must never pause a replacement print.
func (s *Service) OnPlateInspection(ctx context.Context, id, run int64, task, name string, occupied bool, reason string) error {
	var active int
	if e := s.db.QueryRowContext(ctx, "SELECT count(*) FROM print_log_entries r JOIN printers p ON p.id=r.printer_id WHERE r.id=? AND p.id=? AND coalesce(r.subtask_id,'')=? AND r.completed_at IS NULL AND p.plate_detection_enabled", run, id, task).Scan(&active); e != nil || active != 1 {
		return errors.New("Print or plate check settings changed")
	}
	s.mu.RLock()
	d := s.devices[id]
	if d == nil || d.h == nil || !d.h.Online() || !activePlatePhase(val(d.report["gcode_state"])) || num(d.report["layer_num"]) > 1 || val(d.report["subtask_name"]) != name || val(d.report["subtask_id"]) != task {
		s.mu.RUnlock()
		return errors.New("Printer task changed")
	}
	h := d.h
	s.mu.RUnlock()
	kind := "plate_check_failed"
	var err error
	if occupied {
		if task == "" || task == "0" {
			err = errors.New("A verified print ID is required to pause")
		} else {
			c, cancel := context.WithTimeout(ctx, 8*time.Second)
			err = h.Publish(c, row{"print": row{"command": "pause", "sequence_id": "2000", "param": ""}})
			cancel()
		}
		if err != nil {
			reason = "Could not deliver a verified pause"
		} else {
			kind = "plate_not_empty"
			reason = "Objects detected on build plate; pause sent"
		}
	}
	s.emit(row{"type": kind, "printer_id": id, "data": map[string]any{"run_id": run, "printer_id": id, "task_name": name, "reason": reason}})
	return err
}
func activePlatePhase(s string) bool {
	switch strings.ToUpper(s) {
	case "RUNNING", "PREPARE", "INIT":
		return true
	}
	return false
}
