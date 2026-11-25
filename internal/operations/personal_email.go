package operations

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"strings"
)

// Queue only mail the account had enabled when the physical run changed state.
func (s *Service) queuePersonal(ctx context.Context, tx *sql.Tx, kind string, run int64, title, body string) error {
	switch kind {
	case "print_start", "print_complete", "print_failed", "print_stopped":
	default:
		return nil
	}
	if run <= 0 || s.AccountMail == nil {
		return nil
	}
	_, e := tx.ExecContext(ctx, `INSERT OR IGNORE INTO bambastic_account_mail(run_id,event_type,title,message,state)
	SELECT l.id,?,?,?,'pending' FROM print_log_entries l JOIN users u ON l.created_by_id=u.id
	LEFT JOIN user_email_preferences p ON p.user_id=u.id WHERE l.id=? AND u.is_active AND length(u.email)>0 AND COALESCE(p.notify_`+kind+`,1)
	AND (SELECT value FROM settings WHERE key='advanced_auth_enabled')='true'
	AND COALESCE((SELECT value FROM settings WHERE key='user_notifications_enabled'),'true')!='false'
	AND length((SELECT value FROM settings WHERE key='smtp_host'))>0`, kind, title, body, run)
	return e
}
func (s *Service) personalEmail(ctx context.Context) error {
	if s.AccountMail == nil {
		return nil
	}
	jobs, e := s.list(ctx, "SELECT * FROM bambastic_account_mail WHERE state='pending' ORDER BY rowid LIMIT 32")
	if e != nil {
		return e
	}
	for _, job := range jobs {
		event := str(job["event_type"])
		switch event {
		case "print_start", "print_complete", "print_failed", "print_stopped":
		default:
			continue
		}
		var to, token string
		body := str(job["message"])
		e = s.db.QueryRowContext(ctx, `SELECT u.email,COALESCE(l.confirm_token,'') FROM print_log_entries l JOIN users u ON l.created_by_id=u.id LEFT JOIN user_email_preferences p ON p.user_id=u.id WHERE l.id=? AND u.is_active AND length(u.email)>0 AND COALESCE(p.notify_`+event+`,1) AND (SELECT value FROM settings WHERE key='advanced_auth_enabled')='true' AND COALESCE((SELECT value FROM settings WHERE key='user_notifications_enabled'),'true')!='false'`, job["run_id"]).Scan(&to, &token)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		state := "sending"
		if to == "" {
			state = "skipped"
		}
		res, e := s.db.ExecContext(ctx, "UPDATE bambastic_account_mail SET state=? WHERE run_id=? AND event_type=? AND state='pending'", state, job["run_id"], event)
		if e != nil {
			return e
		}
		changed, _ := res.RowsAffected()
		if changed == 0 || state == "skipped" {
			continue
		}
		if event == "print_complete" && token != "" {
			base := strings.TrimRight(setting(ctx, s.db, "external_url"), "/")
			if u, e := url.Parse(base); e == nil && u.Host != "" && u.User == nil && (u.Scheme == "https" || u.Scheme == "http") {
				body += "\n\nConfirm quality\n" + base + "/confirm/" + url.PathEscape(token) + "/good\nReject print\n" + base + "/confirm/" + url.PathEscape(token) + "/reject"
			}
		}
		state = "sent"
		if e = s.AccountMail(ctx, to, str(job["title"]), body); e != nil {
			state = "failed"
		}
		if _, e = s.db.ExecContext(ctx, "UPDATE bambastic_account_mail SET state=? WHERE run_id=? AND event_type=?", state, job["run_id"], event); e != nil {
			return e
		}
	}
	return nil
}
