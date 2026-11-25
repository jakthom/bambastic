package operations

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

// SensorHold is evaluated from the same transaction as dispatch. Unreachable
// HA entities have no current alert, matching the release's interlock behavior.
func SensorHold(ctx context.Context, tx *sql.Tx, printerID int64) (bool, error) {
	rows, err := tx.QueryContext(ctx, "SELECT * FROM printer_ha_sensors WHERE printer_id=? AND block_print AND unixepoch(last_checked)>unixepoch()-90", printerID)
	if err != nil {
		return false, err
	}
	sensors, err := httpx.Rows(rows)
	if err != nil {
		return false, err
	}
	for _, s := range sensors {
		if sensorReading(s)["alerting"] == true {
			return true, nil
		}
	}
	return false, nil
}
func sensorReading(v obj) obj {
	state := str(v["last_state"])
	known := state != "" && state != "unknown" && state != "unavailable"
	alert := false
	var value any
	if known {
		if str(v["kind"]) == "binary" {
			target := str(v["alert_state"])
			if target == "" {
				target = "on"
			}
			alert = state == target
		} else if n, e := strconv.ParseFloat(state, 64); e == nil {
			value = n
			alert = v["alert_above"] != nil && n > num(v["alert_above"]) || v["alert_below"] != nil && n < num(v["alert_below"])
		}
	}
	var shown any
	if known {
		shown = state
	}
	return obj{"id": v["id"], "name": v["name"], "entity_id": v["entity_id"], "kind": v["kind"], "device_class": v["device_class"], "unit": v["unit"], "state": shown, "value": value, "alerting": alert, "block_print": flag(v["block_print"]), "reachable": known, "last_changed": v["last_changed"]}
}
func (s *Service) sensorPoll(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		s.pollSensors(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
func (s *Service) pollSensors(ctx context.Context) {
	base, token := s.ha(ctx)
	for _, table := range []string{"printer_ha_sensors", "location_ha_sensors"} {
		all, e := s.list(ctx, "SELECT * FROM "+table)
		if e != nil {
			return
		}
		for _, v := range all {
			if ctx.Err() != nil {
				return
			}
			state := "unavailable"
			var data obj
			if base != "" && token != "" {
				if e = send(ctx, "GET", base+"/api/states/"+str(v["entity_id"]), bearer(token), nil, &data); e == nil {
					if val := str(data["state"]); val != "" && len(val) <= 64 {
						state = val
					}
				}
			}
			old := sensorReading(v)
			ts := time.Now().UTC().Format("2006-01-02 15:04:05")
			res, e := s.db.ExecContext(ctx, "UPDATE "+table+" SET last_changed=CASE WHEN last_state IS NULL OR last_state<>? THEN ? ELSE last_changed END,last_state=?,last_checked=? WHERE id=? AND entity_id=?", state, ts, state, ts, v["id"], v["entity_id"])
			if e != nil {
				continue
			}
			n, _ := res.RowsAffected()
			v["last_state"] = state
			now := sensorReading(v)
			if n > 0 && old["alerting"] != true && now["alerting"] == true && flag(v["notify_on_alert"]) {
				s.Publish(obj{"type": "ha_sensor_alert", "printer_id": v["printer_id"], "data": obj{"printer_id": v["printer_id"], "sensor_id": v["id"], "name": v["name"], "state": state}})
			}
		}
	}
}
func (s *Service) sensorEntities(r *http.Request) (any, error) {
	base, token := s.ha(r.Context())
	if base == "" || token == "" {
		return nil, fail(400, "Home Assistant is not configured")
	}
	var states []obj
	if e := send(r.Context(), "GET", base+"/api/states", bearer(token), nil, &states); e != nil {
		return nil, e
	}
	out := []obj{}
	term := strings.ToLower(r.URL.Query().Get("search"))
	for _, v := range states {
		entity := str(v["entity_id"])
		kind, _, _ := strings.Cut(entity, ".")
		if kind != "binary_sensor" && kind != "sensor" {
			continue
		}
		a := sub(v["attributes"])
		label := str(a["friendly_name"])
		if !strings.Contains(strings.ToLower(label+entity), term) {
			continue
		}
		out = append(out, obj{"entity_id": entity, "friendly_name": label, "state": v["state"], "unit_of_measurement": a["unit_of_measurement"], "device_class": a["device_class"], "kind": map[string]string{"sensor": "numeric", "binary_sensor": "binary"}[kind]})
	}
	return out, nil
}
func (s *Service) sensors(m *http.ServeMux) {
	for _, kind := range []string{"printer", "location"} {
		base, table, scopeKey, schema := "ha-sensors", "printer_ha_sensors", "printer_id", "PrinterHASensor"
		if kind == "location" {
			base, table, scopeKey, schema = "location-ha-sensors", "location_ha_sensors", "location_id", "LocationHASensor"
		}
		allowed := func(r *http.Request, v obj) bool {
			return kind != "printer" || access.From(r.Context()).AllowsPrinter(int64(num(v[scopeKey])))
		}
		s.route(m, "GET", base+"/entities", "smart_plugs:read", s.sensorEntities)
		for _, p := range []string{base, base + "/{$}"} {
			s.route(m, "GET", p, "smart_plugs:read", func(r *http.Request) (any, error) {
				rows, e := s.list(r.Context(), "SELECT * FROM "+table+" ORDER BY sort_order,id")
				if e != nil {
					return nil, e
				}
				out := []obj{}
				for _, v := range rows {
					if allowed(r, v) && (r.URL.Query().Get(scopeKey) == "" || r.URL.Query().Get(scopeKey) == fmt.Sprint(v[scopeKey])) {
						out = append(out, v)
					}
				}
				return out, nil
			})
			s.route(m, "POST", p, "smart_plugs:create", func(r *http.Request) (any, error) {
				v, e := input(r, schema+"Create")
				if e != nil {
					return nil, e
				}
				if !allowed(r, v) {
					return nil, sql.ErrNoRows
				}
				if !validEntity(str(v["entity_id"])) {
					return nil, fail(422, "Invalid sensor entity")
				}
				if v["kind"] == nil {
					v["kind"] = "binary"
				}
				k, e := save(r.Context(), s.db, table, 0, v)
				if e != nil {
					return nil, e
				}
				return s.get(r.Context(), table, k)
			})
		}
		s.route(m, "GET", base+"/by-"+kind+"/{id}/readings", "smart_plugs:read", func(r *http.Request) (any, error) {
			if !allowed(r, obj{scopeKey: id(r)}) {
				return nil, sql.ErrNoRows
			}
			v, e := s.list(r.Context(), "SELECT * FROM "+table+" WHERE "+scopeKey+"=? ORDER BY sort_order,id", id(r))
			if e != nil {
				return nil, e
			}
			out := []obj{}
			for _, r := range v {
				out = append(out, sensorReading(r))
			}
			return out, nil
		})
		for method, perm := range map[string]string{"GET": "read", "PATCH": "update", "DELETE": "delete"} {
			s.route(m, method, base+"/{id}", "smart_plugs:"+perm, func(r *http.Request) (any, error) {
				v, e := s.get(r.Context(), table, id(r))
				if e != nil {
					return nil, e
				}
				if !allowed(r, v) {
					return nil, sql.ErrNoRows
				}
				switch r.Method {
				case "GET":
					return v, nil
				case "DELETE":
					_, e = s.db.ExecContext(r.Context(), "DELETE FROM "+table+" WHERE id=?", id(r))
					return obj{"success": e == nil}, e
				}
				values, e := input(r, schema+"Update")
				if e != nil {
					return nil, e
				}
				if entity, exists := values["entity_id"]; exists {
					if !validEntity(str(entity)) {
						return nil, fail(422, "Invalid sensor entity")
					}
					if entity != v["entity_id"] {
						values["last_state"] = nil
						values["last_changed"] = nil
						values["last_checked"] = nil
					}
				}
				if len(values) > 0 {
					_, e = save(r.Context(), s.db, table, id(r), values)
					if e != nil {
						return nil, e
					}
				}
				return s.get(r.Context(), table, id(r))
			})
		}
	}
	s.route(m, "GET", "bulk/card-plugs", "smart_plugs:read", s.cardData)
	s.route(m, "GET", "bulk/ha-sensor-readings", "smart_plugs:read", s.cardData)
}
func validEntity(s string) bool {
	d, k, ok := strings.Cut(s, ".")
	if !ok || (d != "sensor" && d != "binary_sensor") || k == "" || len(s) > 255 {
		return false
	}
	for _, r := range k {
		if r != '_' && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}
func (s *Service) cardData(r *http.Request) (any, error) {
	result := obj{}
	ids := strings.Split(r.URL.Query().Get("ids"), ",")
	if len(ids) > 512 {
		return nil, fail(422, "Too many printer IDs")
	}
	p := access.From(r.Context())
	for _, entry := range ids {
		if entry == "" {
			continue
		}
		pid, e := strconv.ParseInt(strings.TrimSpace(entry), 10, 64)
		if e != nil || pid <= 0 {
			return nil, fail(422, "Invalid printer ID")
		}
		if !p.AllowsPrinter(pid) {
			continue
		}
		key := strconv.FormatInt(pid, 10)
		if strings.HasSuffix(r.URL.Path, "ha-sensor-readings") {
			rows, e := s.list(r.Context(), "SELECT * FROM printer_ha_sensors WHERE printer_id=? AND show_on_printer_card ORDER BY sort_order,id", pid)
			if e != nil {
				return nil, e
			}
			out := []obj{}
			for _, v := range rows {
				out = append(out, sensorReading(v))
			}
			result[key] = out
			continue
		}
		rows, e := s.list(r.Context(), "SELECT * FROM smart_plugs WHERE printer_id=? ORDER BY controls_printer_power DESC,enabled DESC,id", pid)
		if e != nil {
			return nil, e
		}
		var main any
		scripts := []obj{}
		for _, v := range rows {
			script := strings.HasPrefix(str(v["ha_entity_id"]), "script.")
			if script {
				if flag(v["show_on_printer_card"]) {
					scripts = append(scripts, presentPlug(r, v))
				}
			} else if main == nil {
				main = presentPlug(r, v)
			}
		}
		result[key] = obj{"plug": main, "scripts": scripts}
	}
	return result, nil
}
