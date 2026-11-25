package operations

import (
	"bambastic/internal/access"
	"bambastic/internal/mailer"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

func (s *Service) appMessages(m *http.ServeMux) {
	const path = "notifications/app-message"
	channels := func(r *http.Request) ([]obj, error) {
		v, e := s.list(r.Context(), "SELECT id,name,provider_type,printer_id FROM notification_providers WHERE enabled AND on_app_message ORDER BY name,id")
		if e != nil {
			return nil, e
		}
		out := []obj{}
		for _, p := range v {
			if providerVisible(r, p) {
				out = append(out, p)
			}
		}
		return out, nil
	}
	s.route(m, "GET", path+"/channels", "notifications:send", func(r *http.Request) (any, error) {
		v, e := channels(r)
		if e != nil {
			return nil, e
		}
		for _, p := range v {
			delete(p, "id")
			delete(p, "printer_id")
		}
		return v, nil
	})
	s.route(m, "POST", path, "notifications:send", func(r *http.Request) (any, error) {
		d, e := input(r, "AppMessage")
		if e != nil {
			return nil, e
		}
		title, msg := strings.TrimSpace(str(d["title"])), strings.TrimSpace(str(d["message"]))
		if title == "" || msg == "" {
			return nil, fail(422, "Title and message are required")
		}
		if raw := str(d["url"]); raw != "" {
			u, e := url.Parse(raw)
			if e != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
				return nil, fail(422, "Invalid message link")
			}
			msg += "\n" + raw
		}
		v, e := channels(r)
		if e != nil {
			return nil, e
		}
		a := access.From(r.Context())
		sender := fmt.Sprintf("%d/%d", a.ID, a.KeyID)
		tx, e := s.db.BeginTx(r.Context(), nil)
		if e != nil {
			return nil, e
		}
		defer tx.Rollback()
		if _, e = tx.Exec("DELETE FROM bambastic_app_message_rate WHERE julianday(created_at)<julianday('now','-60 seconds')"); e != nil {
			return nil, e
		}
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM bambastic_app_message_rate WHERE sender=?", sender).Scan(&n); e != nil {
			return nil, e
		}
		if n >= 20 {
			return nil, fail(429, "Too many application messages in the past minute")
		}
		if _, e = tx.Exec("INSERT INTO bambastic_app_message_rate(sender) VALUES(?)", sender); e != nil {
			return nil, e
		}
		for _, p := range v {
			if _, e = tx.Exec("INSERT INTO bambastic_notification_delivery(provider_id,event_type,title,message,printer_id) VALUES(?,'app_message',?,?,?)", p["id"], title, msg, p["printer_id"]); e != nil {
				return nil, e
			}
		}
		if e = tx.Commit(); e != nil {
			return nil, e
		}
		s.signal()
		return obj{"channels": len(v)}, nil
	})
}
func sendMail(ctx context.Context, c obj, title, message string) error {
	config := mailer.Config{Host: str(c["smtp_server"]), Port: int(num(c["smtp_port"])), From: str(c["from_email"]), Security: str(c["security"]), Auth: c["auth_enabled"] == nil || flag(c["auth_enabled"]), Username: str(c["username"]), Password: str(c["password"])}
	err := mailer.Send(ctx, config, str(c["to_email"]), title, message)
	if e, ok := err.(mailer.Error); ok {
		return fail(e.Status, e.Message)
	}
	return err
}
