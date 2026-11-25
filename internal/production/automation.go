package production

import (
	"bambastic/internal/access"
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
)

func (s *Service) automationRoutes(rt register) {
	rt("POST", "/webhook/queue/add", s.webhook("add"))
	rt("GET", "/webhook/queue", s.webhook("queue"))
	for _, a := range []string{"start", "stop", "cancel"} {
		rt("POST", "/webhook/printer/{id}/"+a, s.webhook(a))
	}
	rt("GET", "/webhook/printer/{id}/status", s.webhook("status"))
	for _, a := range []string{"slot-presets", "ams-labels", "firmware-updates", "printer-queues", "archive-folders"} {
		rt("GET", "/bulk/"+a, s.bulk(a))
	}
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		path := "/printers/{id}/ams-labels"
		if method != "GET" {
			path += "/{ams}"
		}
		rt(method, path, s.amsLabels)
	}
}
func (s *Service) webhook(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		p := access.From(r.Context())
		if p.KeyID == 0 {
			return nil, fail(401, "An API key is required")
		}
		ctx := r.Context()
		if action == "add" {
			m, e := input(w, r)
			if e != nil {
				return nil, e
			}
			if num(m["printer_id"]) < 1 || num(m["archive_id"]) < 1 {
				return nil, fail(422, "An archive and printer are required")
			}
			return s.enqueueInput(ctx, p, r.Header.Get("Idempotency-Key"), m)
		}
		if action == "queue" {
			if !p.Has("printers:read") || !p.Has("queue:read_own") && !p.Has("queue:read_all") {
				return nil, fail(403, "Status access required")
			}
			printers, e := records(ctx, s.db, "SELECT id,name FROM printers WHERE is_active ORDER BY id")
			if e != nil {
				return nil, e
			}
			out := []row{}
			for _, v := range printers {
				k := num(v["id"])
				if !p.AllowsPrinter(k) || r.URL.Query().Has("printer_id") && k != num(r.URL.Query().Get("printer_id")) {
					continue
				}
				rs, e := records(ctx, s.db, queueProjection+" WHERE q.printer_id=? AND q.status IN ('pending','printing','dispatching') ORDER BY position,q.id", k)
				if e != nil {
					return nil, e
				}
				items := []row{}
				pending, printing := 0, 0
				for _, j := range rs {
					if queueVisible(p, j, "read") {
						if j["status"] == "printing" {
							printing++
						} else {
							pending++
						}
						items = append(items, row{"id": j["id"], "archive_id": j["archive_id"], "position": j["position"], "status": j["status"], "scheduled_time": j["scheduled_time"]})
					}
				}
				out = append(out, row{"printer_id": k, "printer_name": v["name"], "pending": pending, "printing": printing, "items": items})
			}
			return out, nil
		}
		perm := "printers:control"
		if action == "status" {
			perm = "printers:read"
		}
		pr, e := s.printerRow(r, perm)
		if e != nil {
			return nil, e
		}
		k := num(pr["id"])
		status := s.state(k, pr)
		switch action {
		case "status":
			status["serial_number"] = pr["serial_number"]
			status["remaining_seconds"] = num(status["remaining_time"]) * 60
			return status, nil
		case "stop", "cancel":
			if !flag(status["connected"]) {
				return nil, fail(503, "Printer is offline")
			}
			if !active(val(status["state"])) {
				return nil, fail(409, "No print in progress")
			}
			if _, e = s.deviceCommand(ctx, k, "stop"); e != nil {
				return nil, e
			}
			return row{"message": "Stop sent to printer"}, nil
		case "start":
			jobs, e := records(ctx, s.db, queueProjection+" WHERE q.printer_id=? AND q.status='pending' ORDER BY q.position,q.id", k)
			if e != nil {
				return nil, e
			}
			for _, j := range jobs {
				if !queueVisible(p, j, "update") && !(p.Has("queue:update_own") && num(j["created_by_id"]) == 0) {
					continue
				}
				r.SetPathValue("id", val(j["id"]))
				if _, e = s.queueAction("start")(w, r); e != nil {
					return nil, e
				}
				return row{"message": "Print released", "queue_item_id": j["id"]}, nil
			}
			return nil, fail(404, "No accessible pending item")
		}
		return nil, sql.ErrNoRows
	}
}
func batchIDs(r *http.Request) ([]int64, error) {
	qs := r.URL.Query().Get("ids")
	if qs == "" {
		return []int64{}, nil
	}
	tokens := strings.Split(qs, ",")
	if len(tokens) > 500 {
		return nil, fail(422, "At most 500 IDs are supported")
	}
	out := []int64{}
	seen := map[int64]bool{}
	for _, v := range tokens {
		n, e := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if e != nil || n < 1 {
			return nil, fail(422, "Invalid identifier")
		}
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out, nil
}
func slotKey(ams, tray int64) string {
	switch {
	case ams == 255:
		return strconv.FormatInt(255*4+tray, 10)
	case ams >= 128:
		return strconv.FormatInt(ams, 10)
	case ams == 16:
		return strconv.FormatInt(24+tray, 10)
	case ams == 32:
		return strconv.FormatInt(28+tray, 10)
	default:
		return strconv.FormatInt(ams*4+tray, 10)
	}
}
func (s *Service) presetsFor(ctx context.Context, k int64) (map[string]row, error) {
	rs, e := records(ctx, s.db, "SELECT ams_id,tray_id,preset_id,preset_name,preset_source,tray_info_idx FROM slot_preset_mappings WHERE printer_id=?", k)
	out := map[string]row{}
	for _, v := range rs {
		out[slotKey(num(v["ams_id"]), num(v["tray_id"]))] = v
	}
	return out, e
}
func (s *Service) unitSerials(k int64) map[int64]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[int64]string{}
	if d := s.devices[k]; d != nil {
		ams, _ := d.report["ams"].(map[string]any)
		units, _ := ams["ams"].([]any)
		for _, raw := range units {
			u, _ := raw.(map[string]any)
			serial := val(u["sn"])
			if serial != "" {
				out[num(u["id"])] = serial
			}
		}
	}
	return out
}
func (s *Service) labelsFor(ctx context.Context, k int64) (map[int64]string, error) {
	out := map[int64]string{}
	for a, sn := range s.unitSerials(k) {
		label, e := one(ctx, s.db, "SELECT label FROM ams_labels WHERE ams_serial_number=?", sn)
		if e == sql.ErrNoRows {
			continue
		}
		if e != nil {
			return nil, e
		}
		out[a] = val(label["label"])
	}
	return out, nil
}
func (s *Service) amsLabels(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := "printers:read"
	if r.Method != "GET" {
		perm = "printers:control"
	}
	if _, e := s.printerRow(r, perm); e != nil {
		return nil, e
	}
	k := pathID(r)
	ctx := r.Context()
	if r.Method != "GET" {
		a := num(r.PathValue("ams"))
		sn := s.unitSerials(k)[a]
		if sn == "" {
			return nil, fail(409, "AMS identity is unavailable")
		}
		m := row{}
		var e error
		if r.Method == "PUT" {
			m, e = input(w, r)
			if e != nil {
				return nil, e
			}
		}
		label := strings.TrimSpace(val(m["label"]))
		if len(label) > 100 {
			return nil, fail(422, "Label too long")
		}
		if label == "" {
			_, e = s.db.ExecContext(ctx, "DELETE FROM ams_labels WHERE ams_serial_number=?", sn)
		} else {
			_, e = s.db.ExecContext(ctx, "INSERT INTO ams_labels(ams_serial_number,ams_id,label) VALUES(?,?,?) ON CONFLICT(ams_serial_number) DO UPDATE SET ams_id=excluded.ams_id,label=excluded.label,updated_at=CURRENT_TIMESTAMP", sn, a, label)
		}
		if e != nil {
			return nil, e
		}
	}
	return s.labelsFor(ctx, k)
}
func (s *Service) bulk(action string) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		ids, e := batchIDs(r)
		if e != nil {
			return nil, e
		}
		perm := "printers:read"
		switch action {
		case "firmware-updates":
			perm = "firmware:read"
		case "archive-folders":
			perm = "library:read_own"
			if p.Has("library:read_all") {
				perm = "library:read_all"
			}
		case "printer-queues":
			perm = "queue:read_own"
			if p.Has("queue:read_all") {
				perm = "queue:read_all"
			}
		}
		if !p.Has(perm) {
			return nil, fail(403, "Permission denied")
		}
		result := map[int64]any{}
		firmware := []row{}
		for _, k := range ids {
			if action == "archive-folders" {
				if _, e := s.getAsset(ctx, s.db, p, "archives", k, "read"); e != nil {
					continue
				}
				r.SetPathValue("id", strconv.FormatInt(k, 10))
				result[k], e = s.linkedFolders("archive")(w, r)
				if e != nil {
					return nil, e
				}
				continue
			}
			if !p.AllowsPrinter(k) {
				continue
			}
			pr, e := one(ctx, s.db, "SELECT * FROM printers WHERE id=?", k)
			if e == sql.ErrNoRows {
				continue
			}
			if e != nil {
				return nil, e
			}
			switch action {
			case "slot-presets":
				result[k], e = s.presetsFor(ctx, k)
			case "ams-labels":
				result[k], e = s.labelsFor(ctx, k)
			case "firmware-updates":
				firmware = append(firmware, s.firmwareInfo(ctx, pr))
			case "printer-queues":
				rs, err := records(ctx, s.db, queueProjection+" WHERE q.printer_id=? ORDER BY q.position,q.id", k)
				if err != nil {
					return nil, err
				}
				list := []row{}
				states := r.URL.Query().Get("status")
				for _, v := range rs {
					if !queueVisible(p, v, "read") || states != "" && !strings.Contains(","+states+",", ","+val(v["status"])+",") {
						continue
					}
					list = append(list, queueView(v))
				}
				result[k] = list
			}
			if e != nil {
				return nil, e
			}
		}
		if action == "firmware-updates" {
			return firmware, nil
		}
		return result, nil
	}
}
