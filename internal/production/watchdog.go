package production

import (
	"context"
	"database/sql"
	"errors"
	"hash/crc32"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

func startSequence(remote string) string {
	return strconv.FormatUint(uint64(crc32.ChecksumIEEE([]byte(remote))), 10)
}
func refusalReason(e error) bool {
	var code *textproto.Error
	if errors.As(e, &code) {
		switch code.Code {
		case 421, 425, 426, 450, 451, 452, 530, 532, 552:
			return true
		}
		return false
	}
	var problem net.Error
	return errors.As(e, &problem)
}
func (s *Service) requeue(ctx context.Context, tx *sql.Tx, qid int64, reason string, phase string) error {
	_, e := tx.ExecContext(ctx, `UPDATE print_queue SET status='pending',printer_id=CASE WHEN EXISTS(SELECT 1 FROM bb_submission_target WHERE queue_id=?) THEN (SELECT printer_id FROM bb_submission_target WHERE queue_id=?) ELSE printer_id END,dispatching_at=NULL,billing_run_id=NULL,waiting_reason=? WHERE id=? AND status='dispatching'`, qid, qid, reason, qid)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase=?,updated_at=CURRENT_TIMESTAMP WHERE queue_id=?", phase, qid)
	return e
}
func (s *Service) coolDown(ctx context.Context, qid, k int64, e error) bool {
	if !refusalReason(e) {
		return false
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false
	}
	defer tx.Rollback()
	a, err := one(ctx, tx, "SELECT phase FROM bb_attempts WHERE queue_id=?", qid)
	if err != nil || a["phase"] != "uploading" {
		return false
	}
	reason := "Printer file service unavailable; retrying after cooldown"
	if err = s.requeue(ctx, tx, qid, reason, "refused"); err != nil {
		return false
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO bb_device_cooldown(printer_id,failures,until) VALUES(?,1,unixepoch()+300) ON CONFLICT(printer_id) DO UPDATE SET failures=min(5,bb_device_cooldown.failures+1),until=unixepoch()+min(3600,300*(1<<CAST(bb_device_cooldown.failures AS INTEGER)))", k)
	if err != nil {
		return false
	}
	return tx.Commit() == nil
}
func (s *Service) observeRefusal(ctx context.Context, k int64, delta row) {
	cmd := val(delta["command"])
	result := strings.ToLower(val(delta["result"]))
	if (cmd != "project_file" && cmd != "gcode_file") || (result != "fail" && result != "failed" && result != "error") {
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	a, e := one(ctx, tx, "SELECT * FROM bb_attempts WHERE printer_id=? AND phase='sent'", k)
	if e != nil || val(delta["sequence_id"]) != startSequence(val(a["name"])) {
		return
	}
	reason := "Printer rejected the start command"
	if why := val(delta["reason"]); why != "" && len(why) <= 180 {
		reason += ": " + why
	}
	_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='failed',completed_at=CURRENT_TIMESTAMP,error_message=?,waiting_reason=NULL WHERE id=? AND status='dispatching'", reason, a["queue_id"])
	if e != nil {
		return
	}
	_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='rejected' WHERE queue_id=? AND phase='sent'", a["queue_id"])
	if e == nil && tx.Commit() == nil {
		s.emit(row{"type": "queue_item_failed", "printer_id": k, "queue_item_id": a["queue_id"], "reason": reason})
		s.kick()
	}
}

// Only an uninterrupted series of fresh idle reports can reject a sent start.
// A reconnect resets this clock. A stale terminal report cannot by itself reprint a job.
func (s *Service) idleEvidence(ctx context.Context, tx *sql.Tx, a row) error {
	qid := num(a["queue_id"])
	w, e := one(ctx, tx, "SELECT first,last FROM bb_idle_watch WHERE queue_id=?", qid)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	now := time.Now().Unix()
	if w == nil || now-num(w["last"]) > 40 {
		_, e = tx.ExecContext(ctx, "INSERT INTO bb_idle_watch VALUES(?,?,?) ON CONFLICT(queue_id) DO UPDATE SET first=excluded.first,last=excluded.last", qid, now, now)
		return e
	}
	if now-num(w["first"]) < 90 {
		_, e = tx.ExecContext(ctx, "UPDATE bb_idle_watch SET last=? WHERE queue_id=?", now, qid)
		return e
	}
	q, e := one(ctx, tx, "SELECT dispatch_attempts FROM print_queue WHERE id=?", qid)
	if e != nil {
		return e
	}
	if num(q["dispatch_attempts"]) >= 3 {
		_, e = tx.ExecContext(ctx, "UPDATE print_queue SET status='failed',error_message='Printer did not start after three attempts',completed_at=CURRENT_TIMESTAMP WHERE id=? AND status='dispatching'", qid)
		if e == nil {
			_, e = tx.ExecContext(ctx, "UPDATE bb_attempts SET phase='timeout' WHERE queue_id=?", qid)
		}
	} else {
		e = s.requeue(ctx, tx, qid, "Printer remained idle after start", "timeout")
	}
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "DELETE FROM bb_idle_watch WHERE queue_id=?", qid)
	return e
}
