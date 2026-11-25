package operations

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"bambastic/internal/access"
)

const providerTable = "notification_providers"

var secrets = map[string]bool{"apikey": true, "auth_token": true, "user_key": true, "app_token": true, "bot_token": true, "password": true, "webhook_url": true, "auth_header": true, "device_key": true, "token": true}

func providerVisible(r *http.Request, v obj) bool {
	a := access.From(r.Context())
	return a.PrinterIDs == nil || v["printer_id"] != nil && a.AllowsPrinter(int64(num(v["printer_id"])))
}
func presentProvider(v obj) obj {
	out := objectCopy(v)
	c := objectCopy(sub(v["config"]))
	for k, x := range c {
		if secrets[k] && str(x) != "" {
			c[k] = mask
		}
	}
	out["config"] = c
	return out
}
func (s *Service) provider(r *http.Request, key any) (obj, error) {
	d, e := s.get(r.Context(), providerTable, key)
	if e == nil && !providerVisible(r, d) {
		return nil, sql.ErrNoRows
	}
	return d, e
}
func mergeConfig(old, c obj, kind, oldKind string) (obj, error) {
	out := objectCopy(c)
	for k, x := range c {
		if x != mask {
			continue
		}
		if !secrets[k] || str(old[k]) == "" || oldKind != kind {
			return nil, fail(422, "Enter credentials for the destination")
		}
		for _, dest := range []string{"server", "webhook_url", "smtp_server", "smtp_port", "security"} {
			newDest := str(c[dest])
			if newDest != mask && newDest != str(old[dest]) {
				return nil, fail(422, "Re-enter credentials for the new destination")
			}
		}
		out[k] = old[k]
	}
	return out, nil
}
func (s *Service) saveProvider(r *http.Request, create bool) (any, error) {
	var old obj
	var e error
	model := "NotificationProviderCreate"
	if !create {
		model = "NotificationProviderUpdate"
		old, e = s.provider(r, id(r))
		if e != nil {
			return nil, e
		}
	}
	d, e := input(r, model)
	if e != nil {
		return nil, e
	}
	v := objectCopy(old)
	for k, x := range d {
		v[k] = x
	}
	if !providerVisible(r, v) {
		return nil, fail(403, "Printer scope does not permit this destination")
	}
	if c, ok := d["config"]; ok {
		c, e := mergeConfig(sub(old["config"]), sub(c), str(v["provider_type"]), str(old["provider_type"]))
		if e != nil {
			return nil, e
		}
		d["config"] = c
		v["config"] = c
	} else if old != nil && v["provider_type"] != old["provider_type"] {
		return nil, fail(422, "New provider type requires its own configuration")
	}
	if e = validateProvider(str(v["provider_type"]), sub(v["config"])); e != nil {
		return nil, e
	}
	if !create {
		d["updated_at"] = now()
	}
	n, e := save(r.Context(), s.db, providerTable, id(r), d)
	if e != nil {
		return nil, e
	}
	saved, e := s.get(r.Context(), providerTable, n)
	return presentProvider(saved), e
}
func validateProvider(kind string, c obj) error {
	required := map[string][]string{"webhook": {"webhook_url"}, "ntfy": {"topic"}, "gotify": {"server", "app_token"}, "discord": {"webhook_url"}, "telegram": {"bot_token", "chat_id"}, "pushover": {"app_token", "user_key"}, "bark": {"device_key"}, "callmebot": {"phone", "apikey"}, "homeassistant": {}, "email": {"from_email", "to_email", "smtp_server"}}
	keys, ok := required[kind]
	if !ok {
		return fail(422, "Notification provider adapter unavailable")
	}
	for _, k := range keys {
		if strings.TrimSpace(str(c[k])) == "" {
			return fail(422, "Missing provider field "+k)
		}
	}
	for _, k := range []string{"webhook_url", "server"} {
		if raw := str(c[k]); raw != "" {
			if _, e := splitURL(raw); e != nil {
				return e
			}
		}
	}
	if kind == "discord" && !strings.HasPrefix(str(c["webhook_url"]), "https://discord.com/api/webhooks/") && !strings.HasPrefix(str(c["webhook_url"]), "https://discordapp.com/api/webhooks/") {
		return fail(422, "Invalid Discord webhook URL")
	}
	return nil
}
func (s *Service) deliver(ctx context.Context, kind string, c obj, title, message, event string, printer any, images ...*deliveryPhoto) error {
	if e := validateProvider(kind, c); e != nil {
		return e
	}
	endpoint := ""
	headers := map[string]string{"Content-Type": "application/json"}
	var payload any
	var body []byte
	switch kind {
	case "email":
		return sendMail(ctx, c, title, message)
	case "callmebot":
		q := url.Values{"phone": {str(c["phone"])}, "apikey": {str(c["apikey"])}, "text": {title + "\n" + message}}
		return send(ctx, "GET", "https://api.callmebot.com/whatsapp.php?"+q.Encode(), nil, nil, nil)
	case "homeassistant":
		u, t := s.ha(ctx)
		if u == "" || t == "" {
			return fail(422, "Home Assistant is not configured")
		}
		service := strings.TrimPrefix(strings.TrimLeft(str(c["service"]), "/"), "api/services/")
		if service == "" {
			service = "persistent_notification.create"
		}
		service = strings.ReplaceAll(service, "/", ".")
		if !entityID.MatchString(service) || !(strings.HasPrefix(service, "notify.") || service == "persistent_notification.create") {
			return fail(422, "Invalid Home Assistant notification service")
		}
		endpoint = u + "/api/services/" + strings.ReplaceAll(service, ".", "/")
		headers = bearer(t)
		payload = obj{"title": title, "message": message}
	case "webhook":
		endpoint = str(c["webhook_url"])
		if c["payload_format"] == "slack" {
			payload = obj{"text": title + "\n" + message}
		} else {
			kt, km := str(c["field_title"]), str(c["field_message"])
			if kt == "" {
				kt = "title"
			}
			if km == "" {
				km = "message"
			}
			payload = obj{kt: title, km: message, "source": "Bambastic", "event": event, "timestamp": now(), "printer_id": printer}
		}
		if h := str(c["auth_header"]); h != "" {
			headers["Authorization"] = h
		}
	case "ntfy":
		base := strings.TrimRight(str(c["server"]), "/")
		if base == "" {
			base = "https://ntfy.sh"
		}
		endpoint = base + "/" + url.PathEscape(str(c["topic"]))
		headers["Title"] = strings.ReplaceAll(strings.ReplaceAll(title, "\r", " "), "\n", " ")
		headers["Content-Type"] = "text/plain; charset=utf-8"
		if t := str(c["auth_token"]); t != "" {
			headers["Authorization"] = "Bearer " + t
		}
		body = []byte(message)
	case "gotify":
		endpoint = strings.TrimSuffix(strings.TrimRight(str(c["server"]), "/"), "/message") + "/message"
		headers["X-Gotify-Key"] = str(c["app_token"])
		payload = obj{"title": title, "message": message, "priority": int(num(c["priority"]))}
	case "discord":
		endpoint = str(c["webhook_url"])
		payload = obj{"embeds": []obj{{"title": title, "description": message}}}
	case "telegram":
		endpoint = "https://api.telegram.org/bot" + url.PathEscape(str(c["bot_token"])) + "/sendMessage"
		p := obj{"chat_id": c["chat_id"], "text": title + "\n" + message}
		if val := c["message_thread_id"]; val != nil {
			p["message_thread_id"] = val
		}
		payload = p
	case "pushover":
		endpoint = "https://api.pushover.net/1/messages.json"
		payload = obj{"token": c["app_token"], "user": c["user_key"], "title": title, "message": message}
	case "bark":
		endpoint = strings.TrimRight(str(c["server"]), "/")
		if endpoint == "" {
			endpoint = "https://api.day.app"
		}
		endpoint += "/push"
		payload = obj{"device_key": c["device_key"], "title": title, "body": message}
	}
	if len(images) > 0 && images[0] != nil {
		pic := images[0]
		switch kind {
		case "ntfy":
			headers["Message"] = strings.NewReplacer("\r", " ", "\n", " ").Replace(message)
			headers["Filename"] = "photo.jpg"
			headers["Content-Type"] = "image/jpeg"
			body = pic.jpeg
		case "telegram", "pushover", "discord":
			fields := obj{}
			key := "attachment"
			switch kind {
			case "telegram":
				fields = obj{"chat_id": c["chat_id"], "caption": title + "\n" + message, "message_thread_id": c["message_thread_id"]}
				key = "photo"
				endpoint = strings.TrimSuffix(endpoint, "sendMessage") + "sendPhoto"
			case "pushover":
				if len(pic.jpeg) > 2500<<10 {
					break
				}
				fields = payload.(obj)
			case "discord":
				data, _ := json.Marshal(obj{"embeds": []obj{{"title": title, "description": message, "image": obj{"url": "attachment://photo.jpg"}}}})
				fields = obj{"payload_json": string(data)}
				key = "files[0]"
			}
			if len(fields) > 0 {
				raw, typ, e := photoMultipart(fields, key, pic.jpeg)
				if e != nil {
					return e
				}
				body = raw
				headers["Content-Type"] = typ
				payload = nil
			}
		case "homeassistant":
			if pic.url != "" && !strings.HasSuffix(endpoint, "persistent_notification/create") {
				payload.(obj)["data"] = obj{"image": pic.url}
			}
		case "gotify":
			if pic.url != "" {
				payload.(obj)["extras"] = obj{"client::notification": obj{"bigImageUrl": pic.url}}
			}
		case "bark":
			if pic.url != "" {
				payload.(obj)["image"] = pic.url
			}
		case "webhook":
			if pic.url != "" {
				if c["payload_format"] == "slack" {
					payload.(obj)["blocks"] = []obj{{"type": "section", "text": obj{"type": "plain_text", "text": title + "\n" + message}}, {"type": "image", "image_url": pic.url, "alt_text": "Printer snapshot"}}
				} else {
					payload.(obj)["image_url"] = pic.url
				}
			}
		}
	}
	if payload != nil {
		body, _ = json.Marshal(payload)
	}
	return send(ctx, "POST", endpoint, headers, body, nil)
}
func (s *Service) logDelivery(ctx context.Context, p obj, event, title, msg string, printer any, err error) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var problem any
	if err != nil {
		problem = err.Error()
	}
	if _, e = tx.Exec("INSERT INTO notification_logs(provider_id,event_type,title,message,success,error_message,printer_id) VALUES(?,?,?,?,?,?,?)", p["id"], event, title, msg, err == nil, problem, printer); e != nil {
		return e
	}
	if err == nil {
		_, e = tx.Exec("UPDATE notification_providers SET last_success=CURRENT_TIMESTAMP,last_error=NULL,last_error_at=NULL WHERE id=?", p["id"])
	} else {
		_, e = tx.Exec("UPDATE notification_providers SET last_error=?,last_error_at=CURRENT_TIMESTAMP WHERE id=?", problem, p["id"])
	}
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Service) sendTest(r *http.Request, p obj) (obj, error) {
	title, body := "Bambastic notification test", "This destination can receive notifications from Bambastic."
	e := s.deliver(r.Context(), str(p["provider_type"]), sub(p["config"]), title, body, "test", p["printer_id"])
	if p["id"] != nil {
		if logErr := s.logDelivery(r.Context(), p, "test", title, body, p["printer_id"], e); logErr != nil {
			return nil, logErr
		}
	}
	if e != nil {
		return obj{"success": false, "message": e.Error()}, nil
	}
	return obj{"success": true, "message": "Test notification sent"}, nil
}
func (s *Service) notifications(m *http.ServeMux) {
	const base = "notifications"
	for _, path := range []string{base, base + "/{$}"} {
		s.route(m, "GET", path, "notifications:read", func(r *http.Request) (any, error) {
			v, e := s.list(r.Context(), "SELECT * FROM notification_providers ORDER BY name,id")
			res := []obj{}
			for _, d := range v {
				if providerVisible(r, d) {
					res = append(res, presentProvider(d))
				}
			}
			return res, e
		})
		s.route(m, "POST", path, "notifications:create", func(r *http.Request) (any, error) { return s.saveProvider(r, true) })
	}
	s.route(m, "GET", base+"/{id}", "notifications:read", func(r *http.Request) (any, error) { d, e := s.provider(r, id(r)); return presentProvider(d), e })
	s.route(m, "PATCH", base+"/{id}", "notifications:update", func(r *http.Request) (any, error) { return s.saveProvider(r, false) })
	s.route(m, "DELETE", base+"/{id}", "notifications:delete", func(r *http.Request) (any, error) {
		if _, e := s.provider(r, id(r)); e != nil {
			return nil, e
		}
		_, e := s.db.ExecContext(r.Context(), "DELETE FROM notification_providers WHERE id=?", id(r))
		return obj{"message": "Notification provider deleted"}, e
	})
	s.route(m, "POST", base+"/{id}/test", "notifications:update", func(r *http.Request) (any, error) {
		d, e := s.provider(r, id(r))
		if e != nil {
			return nil, e
		}
		return s.sendTest(r, d)
	})
	s.route(m, "POST", base+"/test-config", "notifications:create", func(r *http.Request) (any, error) {
		d, e := input(r, "NotificationTestRequest")
		if e != nil {
			return nil, e
		}
		var old obj
		if d["provider_id"] != nil {
			if !access.From(r.Context()).Has("notifications:update") {
				return nil, fail(403, "Permission denied")
			}
			old, e = s.provider(r, d["provider_id"])
			if e != nil {
				return nil, e
			}
		}
		d["config"], e = mergeConfig(sub(old["config"]), sub(d["config"]), str(d["provider_type"]), str(old["provider_type"]))
		if e != nil {
			return nil, e
		}
		return s.sendTest(r, d)
	})
	s.route(m, "POST", base+"/test-all", "notifications:update", func(r *http.Request) (any, error) {
		v, e := s.list(r.Context(), "SELECT * FROM notification_providers WHERE enabled")
		if e != nil {
			return nil, e
		}
		results := []obj{}
		n := 0
		for _, p := range v {
			if !providerVisible(r, p) {
				continue
			}
			d, e := s.sendTest(r, p)
			if e != nil {
				return nil, e
			}
			d["name"] = p["name"]
			d["id"] = p["id"]
			if flag(d["success"]) {
				n++
			}
			results = append(results, d)
		}
		return obj{"tested": len(results), "failed": len(results) - n, "success": n, "results": results}, nil
	})
	for _, suffix := range []string{"logs", "logs/stats"} {
		s.route(m, "GET", base+"/"+suffix, "notifications:read", func(r *http.Request) (any, error) { return s.logs(r, suffix == "logs/stats") })
	}
	s.route(m, "DELETE", base+"/logs", "notifications:delete", func(r *http.Request) (any, error) {
		v, e := s.list(r.Context(), "SELECT id,printer_id FROM notification_providers")
		if e != nil {
			return nil, e
		}
		var n int64
		for _, p := range v {
			if providerVisible(r, p) {
				res, e := s.db.ExecContext(r.Context(), "DELETE FROM notification_logs WHERE provider_id=?", p["id"])
				if e != nil {
					return nil, e
				}
				nn, _ := res.RowsAffected()
				n += nn
			}
		}
		return obj{"deleted": n}, nil
	})
	s.appMessages(m)
	s.templateRoutes(m)
}
func (s *Service) logs(r *http.Request, stats bool) (any, error) {
	q := r.URL.Query()
	where := " WHERE 1=1"
	var args []any
	for _, k := range []string{"event_type", "provider_id", "success"} {
		if x := q.Get(k); x != "" {
			where += " AND l." + k + "=?"
			if k == "success" {
				args = append(args, x == "true")
			} else {
				args = append(args, x)
			}
		}
	}
	if days := int(num(q.Get("days"))); days > 0 {
		where += " AND julianday(l.created_at)>=julianday('now')-?"
		args = append(args, days)
	}
	a := access.From(r.Context())
	if a.PrinterIDs != nil {
		allowed := []string{}
		for k, ok := range a.PrinterIDs {
			if ok {
				args = append(args, k)
				allowed = append(allowed, "?")
			}
		}
		if len(allowed) == 0 {
			where += " AND 0"
		} else {
			where += " AND p.printer_id IN (" + strings.Join(allowed, ",") + ")"
		}
	}
	query := "SELECT l.*,p.name provider_name,p.provider_type FROM notification_logs l JOIN notification_providers p ON p.id=l.provider_id" + where + " ORDER BY l.id DESC"
	if !stats {
		limit := int(num(q.Get("limit")))
		if limit <= 0 {
			limit = 100
		}
		query += " LIMIT ? OFFSET ?"
		args = append(args, min(limit, 1000), max(0, int(num(q.Get("offset")))))
	}
	rows, e := s.list(r.Context(), query, args...)
	if !stats || e != nil {
		return rows, e
	}
	n := 0
	byType, byProvider := map[string]int{}, map[string]int{}
	for _, v := range rows {
		if flag(v["success"]) {
			n++
		}
		byType[str(v["event_type"])]++
		byProvider[str(v["provider_name"])]++
	}
	return obj{"total": len(rows), "success_count": n, "failure_count": len(rows) - n, "by_event_type": byType, "by_provider": byProvider}, nil
}

//go:embed templates.json
var templateBytes []byte

type templateData struct {
	Defaults  []obj               `json:"DEFAULT_TEMPLATES"`
	Variables map[string][]string `json:"EVENT_VARIABLES"`
	Samples   map[string]obj      `json:"SAMPLE_DATA"`
	Photos    []string            `json:"PHOTO_CAPABLE_EVENTS"`
}

var texts = func() templateData {
	var d templateData
	if e := json.Unmarshal(templateBytes, &d); e != nil {
		panic(e)
	}
	return d
}()

func initTemplates(db *sql.DB) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, v := range texts.Defaults {
		if _, e = tx.Exec("INSERT OR IGNORE INTO notification_templates(event_type,name,title_template,body_template,is_default) VALUES(?,?,?,?,1)", v["event_type"], v["name"], v["title_template"], v["body_template"]); e != nil {
			return e
		}
	}
	return tx.Commit()
}

var variable = regexp.MustCompile(`\{([a-zA-Z0-9_]+)\}`)

func render(t string, data obj) string {
	return variable.ReplaceAllStringFunc(t, func(k string) string {
		v := data[k[1:len(k)-1]]
		if v == nil {
			return ""
		}
		return str(v)
	})
}
func (s *Service) templateRoutes(m *http.ServeMux) {
	const path = "notification-templates"
	for _, p := range []string{path, path + "/{$}"} {
		s.route(m, "GET", p, "notification_templates:read", func(r *http.Request) (any, error) {
			return s.list(r.Context(), "SELECT * FROM notification_templates ORDER BY id")
		})
	}
	s.route(m, "GET", path+"/variables", "notification_templates:read", func(r *http.Request) (any, error) {
		out := []obj{}
		for k, v := range texts.Variables {
			photo := false
			for _, event := range texts.Photos {
				if k == event {
					photo = true
				}
			}
			name := k
			for _, d := range texts.Defaults {
				if d["event_type"] == k {
					name = str(d["name"])
					break
				}
			}
			out = append(out, obj{"event_type": k, "event_name": name, "variables": v, "supports_photo": photo})
		}
		sort.Slice(out, func(i, j int) bool { return str(out[i]["event_type"]) < str(out[j]["event_type"]) })
		return out, nil
	})
	s.route(m, "GET", path+"/{id}", "notification_templates:read", func(r *http.Request) (any, error) { return s.get(r.Context(), "notification_templates", id(r)) })
	s.route(m, "PUT", path+"/{id}", "notification_templates:update", func(r *http.Request) (any, error) {
		if _, e := s.get(r.Context(), "notification_templates", id(r)); e != nil {
			return nil, e
		}
		d, e := input(r, "NotificationTemplateUpdate")
		if e != nil {
			return nil, e
		}
		d["is_default"] = false
		d["updated_at"] = now()
		if _, e = save(r.Context(), s.db, "notification_templates", id(r), d); e != nil {
			return nil, e
		}
		return s.get(r.Context(), "notification_templates", id(r))
	})
	s.route(m, "POST", path+"/{id}/reset", "notification_templates:update", func(r *http.Request) (any, error) {
		d, e := s.get(r.Context(), "notification_templates", id(r))
		if e != nil {
			return nil, e
		}
		for _, v := range texts.Defaults {
			if v["event_type"] == d["event_type"] {
				_, e = save(r.Context(), s.db, "notification_templates", id(r), obj{"title_template": v["title_template"], "body_template": v["body_template"], "is_default": true, "updated_at": now()})
				break
			}
		}
		if e != nil {
			return nil, e
		}
		return s.get(r.Context(), "notification_templates", id(r))
	})
	s.route(m, "POST", path+"/preview", "notification_templates:read", func(r *http.Request) (any, error) {
		d, e := input(r, "TemplatePreviewRequest")
		if e != nil {
			return nil, e
		}
		sample := texts.Samples[str(d["event_type"])]
		return obj{"title": render(str(d["title_template"]), sample), "body": render(str(d["body_template"]), sample)}, nil
	})
}
