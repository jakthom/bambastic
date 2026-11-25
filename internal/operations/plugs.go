package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"

	"bambastic/internal/access"
)

const mask = "********"
const plugTable = "smart_plugs"

var entityID = regexp.MustCompile(`^[a-z][a-z0-9_]*\.[a-z0-9_]+$`)

func presentPlug(r *http.Request, v obj) obj {
	out := objectCopy(v)
	for _, k := range []string{"password", "rest_headers"} {
		if str(out[k]) != "" {
			out[k] = mask
		}
	}
	if !access.From(r.Context()).Has("smart_plugs:update") {
		for k := range out {
			if strings.HasPrefix(k, "rest_") && (strings.HasSuffix(k, "_url") || strings.HasSuffix(k, "_body")) {
				out[k] = nil
			}
		}
	}
	return out
}
func validPlug(v obj) error {
	switch v["plug_type"] {
	case "mqtt":
		have := false
		for _, k := range []string{"mqtt_topic", "mqtt_power_topic", "mqtt_energy_topic", "mqtt_state_topic"} {
			topic := str(v[k])
			if strings.ContainsAny(topic, "#+\x00\r\n") {
				return fail(422, "MQTT plug requires exact topic names")
			}
			have = have || topic != ""
		}
		if !have {
			return fail(422, "Provide an MQTT meter topic")
		}

	case "tasmota":
		ip := net.ParseIP(str(v["ip_address"]))
		if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
			return fail(422, "Enter the printer plug's LAN IP address")
		}
	case "homeassistant":
		if !entityID.MatchString(str(v["ha_entity_id"])) {
			return fail(422, "Invalid Home Assistant entity")
		}
	case "rest":
		for k, x := range v {
			if strings.HasPrefix(k, "rest_") && strings.HasSuffix(k, "_url") && str(x) != "" {
				if _, e := splitURL(str(x)); e != nil {
					return e
				}
			}
		}
		if str(v["rest_status_url"])+str(v["rest_on_url"])+str(v["rest_off_url"]) == "" {
			return fail(422, "A REST plug needs an endpoint")
		}
		if _, e := restHeaders(v["rest_headers"]); e != nil {
			return e
		}
	default:
		return fail(422, "This plug adapter is unavailable")
	}
	return nil
}
func (s *Service) savePlug(r *http.Request, create bool) (any, error) {
	model := "SmartPlugUpdate"
	var old = obj{}
	var e error
	if create {
		model = "SmartPlugCreate"
	} else {
		old, e = s.scoped(r, plugTable)
		if e != nil {
			return nil, e
		}
	}
	d, e := input(r, model)
	if e != nil {
		return nil, e
	}
	if p, ok := d["printer_id"]; ok && !permitted(r, p) {
		return nil, sql.ErrNoRows
	}
	v := objectCopy(old)
	for k, x := range d {
		v[k] = x
	}
	for _, secret := range []string{"password", "rest_headers"} {
		hasSecret := str(old[secret]) != ""
		newSecret, provided := d[secret]
		if provided && newSecret == mask && !hasSecret {
			return nil, fail(422, "Enter the credential")
		}
		if hasSecret && (!provided || newSecret == mask) {
			for key, x := range d {
				if (key == "plug_type" || secret == "password" && key == "ip_address" || secret == "rest_headers" && strings.HasSuffix(key, "_url")) && str(old[key]) != str(x) {
					return nil, fail(422, "Re-enter credentials for the new destination")
				}
			}
			delete(d, secret)
			v[secret] = old[secret]
		}
	}
	if e = validPlug(v); e != nil {
		return nil, e
	}
	if create {
		d["auto_off_pending"] = false
		d["auto_off_executed"] = false
		if d["plug_type"] == "homeassistant" {
			d["auto_on"] = false
			d["auto_off"] = false
		}
	}
	tx, e := s.db.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if v["printer_id"] != nil && v["plug_type"] == "tasmota" {
		var n int
		if e = tx.QueryRow("SELECT count(*) FROM smart_plugs WHERE printer_id=? AND plug_type='tasmota' AND id!=?", v["printer_id"], id(r)).Scan(&n); e != nil {
			return nil, e
		}
		if n > 0 {
			return nil, fail(409, "Printer already has a Tasmota power plug")
		}
	}
	n, e := save(r.Context(), tx, plugTable, id(r), d)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	out, e := s.get(r.Context(), plugTable, n)
	return presentPlug(r, out), e
}
func (s *Service) plugs(m *http.ServeMux) {
	const path = "smart-plugs"
	for _, p := range []string{path, path + "/{$}"} {
		s.route(m, "GET", p, "smart_plugs:read", func(r *http.Request) (any, error) {
			v, e := s.list(r.Context(), "SELECT * FROM smart_plugs ORDER BY name,id")
			if e != nil {
				return nil, e
			}
			vis := []obj{}
			for _, d := range v {
				if permitted(r, d["printer_id"]) {
					vis = append(vis, presentPlug(r, d))
				}
			}
			return vis, nil
		})
		s.route(m, "POST", p, "smart_plugs:create", func(r *http.Request) (any, error) { return s.savePlug(r, true) })
	}
	s.route(m, "GET", path+"/{id}", "smart_plugs:read", func(r *http.Request) (any, error) { d, e := s.scoped(r, plugTable); return presentPlug(r, d), e })
	s.route(m, "PATCH", path+"/{id}", "smart_plugs:update", func(r *http.Request) (any, error) { return s.savePlug(r, false) })
	s.route(m, "DELETE", path+"/{id}", "smart_plugs:delete", func(r *http.Request) (any, error) {
		if _, e := s.scoped(r, plugTable); e != nil {
			return nil, e
		}
		_, e := s.db.ExecContext(r.Context(), "DELETE FROM smart_plugs WHERE id=?", id(r))
		return obj{"message": "Smart plug deleted"}, e
	})
	for _, suffix := range []string{"", "/scripts"} {
		s.route(m, "GET", path+"/by-printer/{id}"+suffix, "smart_plugs:read", func(r *http.Request) (any, error) {
			if !permitted(r, id(r)) {
				return nil, sql.ErrNoRows
			}
			v, e := s.list(r.Context(), `SELECT * FROM smart_plugs WHERE printer_id=? ORDER BY (coalesce(ha_entity_id,'') LIKE 'script.%'),controls_printer_power DESC,enabled DESC,id`, id(r))
			if e != nil {
				return nil, e
			}
			if suffix == "" {
				if len(v) == 0 {
					return nil, nil
				}
				return presentPlug(r, v[0]), nil
			}
			out := []obj{}
			for i, x := range v {
				if x["plug_type"] == "homeassistant" && flag(x["show_on_printer_card"]) && (i != 0 || strings.HasPrefix(str(x["ha_entity_id"]), "script.")) {
					out = append(out, presentPlug(r, x))
				}
			}
			return out, nil
		})
	}
	s.route(m, "POST", path+"/{id}/control", "smart_plugs:control", func(r *http.Request) (any, error) {
		p, e := s.scoped(r, plugTable)
		if e != nil {
			return nil, e
		}
		d, e := input(r, "SmartPlugControl")
		if e != nil {
			return nil, e
		}
		e = s.control(r.Context(), p, str(d["action"]))
		return obj{"success": true, "action": d["action"]}, e
	})
	s.route(m, "GET", path+"/{id}/{action}", "smart_plugs:read", func(r *http.Request) (any, error) {
		if r.PathValue("action") != "status" {
			return nil, sql.ErrNoRows
		}
		p, e := s.scoped(r, plugTable)
		if e != nil {
			return nil, e
		}
		v, e := s.status(r.Context(), p)
		if e != nil {
			return obj{"reachable": false, "state": nil, "energy": nil}, nil
		}
		_, e = s.db.ExecContext(r.Context(), "UPDATE smart_plugs SET last_checked=CURRENT_TIMESTAMP,last_state=? WHERE id=?", v["state"], id(r))
		return v, e
	})
	s.route(m, "POST", path+"/test-connection", "smart_plugs:control", func(r *http.Request) (any, error) {
		p, e := input(r, "SmartPlugTestConnection")
		if e != nil {
			return nil, e
		}
		p["plug_type"] = "tasmota"
		if p["password"] == mask {
			saved, e := s.get(r.Context(), plugTable, p["plug_id"])
			if e != nil {
				return nil, e
			}
			if !access.From(r.Context()).Has("smart_plugs:update") || !permitted(r, saved["printer_id"]) || p["ip_address"] != saved["ip_address"] {
				return nil, fail(403, "Credential does not belong to this destination")
			}
			p["password"] = saved["password"]
		}
		if e = validPlug(p); e != nil {
			return nil, e
		}
		v, e := s.status(r.Context(), p)
		if e != nil {
			return obj{"success": false, "error": e.Error()}, nil
		}
		v["success"] = true
		return v, nil
	})
	s.route(m, "POST", path+"/ha/test-connection", "smart_plugs:control", func(r *http.Request) (any, error) {
		d, e := input(r, "HATestConnectionRequest")
		if e != nil {
			return nil, e
		}
		base, token := str(d["url"]), str(d["token"])
		if token == mask {
			u, t := s.ha(r.Context())
			if u != base || !access.From(r.Context()).Has("settings:update") {
				return nil, fail(403, "Credential does not belong to this destination")
			}
			token = t
		}
		e = send(r.Context(), "GET", strings.TrimRight(base, "/")+"/api/", bearer(token), nil, nil)
		if e != nil {
			return obj{"success": false, "error": e.Error()}, nil
		}
		return obj{"success": true, "message": "Connected to Home Assistant"}, nil
	})
	for _, kind := range []string{"entities", "sensors"} {
		s.route(m, "GET", path+"/ha/"+kind, "smart_plugs:read", func(r *http.Request) (any, error) {
			u, t := s.ha(r.Context())
			if u == "" || t == "" {
				return []obj{}, nil
			}
			var data []obj
			e := send(r.Context(), "GET", u+"/api/states", bearer(t), nil, &data)
			if e != nil {
				return nil, e
			}
			q := strings.ToLower(r.URL.Query().Get("search"))
			out := []obj{}
			for _, v := range data {
				ident := str(v["entity_id"])
				domain, _, _ := strings.Cut(ident, ".")
				attrs := sub(v["attributes"])
				label := str(attrs["friendly_name"])
				if kind == "sensors" && domain != "sensor" || kind == "entities" && q == "" && !switchable(domain) || q != "" && !strings.Contains(strings.ToLower(label+" "+ident), q) {
					continue
				}
				out = append(out, obj{"entity_id": ident, "friendly_name": label, "state": v["state"], "unit_of_measurement": attrs["unit_of_measurement"], "device_class": attrs["device_class"]})
			}
			return out, nil
		})
	}
}
func (s *Service) ha(ctx context.Context) (string, string) {
	pick := func(env, key string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		if key == "ha_token" && s.ReadSecret != nil {
			v, e := s.ReadSecret(ctx, key)
			if e != nil {
				return ""
			}
			return v
		}
		return setting(ctx, s.db, key)
	}
	return strings.TrimRight(pick("HA_URL", "ha_url"), "/"), pick("HA_TOKEN", "ha_token")
}
func bearer(token string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + token, "Content-Type": "application/json"}
}
func switchable(domain string) bool {
	switch domain {
	case "switch", "light", "input_boolean", "script", "button", "input_button", "automation", "fan":
		return true
	}
	return false
}
func restHeaders(v any) (map[string]string, error) {
	h := map[string]string{}
	if str(v) != "" {
		if e := json.Unmarshal([]byte(str(v)), &h); e != nil {
			return nil, fail(422, "REST headers must be a JSON object of strings")
		}
	}
	return h, nil
}
func tasmota(ctx context.Context, p obj, cmd string) (obj, error) {
	if e := validPlug(p); e != nil {
		return nil, e
	}
	args := url.Values{"cmnd": []string{cmd}}
	if u := str(p["username"]); u != "" {
		args.Set("user", u)
		args.Set("password", str(p["password"]))
	}
	ip := str(p["ip_address"])
	if strings.Contains(ip, ":") {
		ip = "[" + ip + "]"
	}
	var v obj
	e := send(ctx, "GET", "http://"+ip+"/cm?"+args.Encode(), nil, nil, &v)
	return v, e
}
func power(v obj) string {
	for _, k := range []string{"POWER", "POWER1"} {
		state := strings.ToUpper(str(v[k]))
		if state == "ON" || state == "OFF" {
			return state
		}
	}
	return ""
}
func readPath(v obj, path string) any {
	var current any = v
	for _, k := range strings.Split(path, ".") {
		current = sub(current)[k]
		if current == nil {
			return nil
		}
	}
	return current
}
func (s *Service) status(ctx context.Context, p obj) (obj, error) {
	v := obj{"state": nil, "device_name": p["name"], "energy": nil, "reachable": true}
	energy := obj{}
	switch p["plug_type"] {
	case "mqtt":
		if s.MQTTStatus == nil {
			return nil, fail(503, "MQTT relay is unavailable")
		}
		data, e := s.MQTTStatus(ctx, int64(num(p["id"])))
		if e != nil {
			return nil, fail(503, "MQTT broker unavailable")
		}
		data["device_name"] = p["name"]
		return data, nil
	case "tasmota":
		res, e := tasmota(ctx, p, "Power")
		if e != nil {
			return nil, e
		}
		st := power(res)
		if st == "" {
			return nil, fail(502, "Device did not report a power state")
		}
		v["state"] = st
		r, e := tasmota(ctx, p, "Status 8")
		if e == nil {
			readings := sub(sub(r["StatusSNS"])["ENERGY"])
			for goal, src := range map[string]string{"power": "Power", "voltage": "Voltage", "current": "Current", "today": "Today", "yesterday": "Yesterday", "total": "Total", "factor": "Factor", "apparent_power": "ApparentPower", "reactive_power": "ReactivePower"} {
				if readings[src] != nil {
					energy[goal] = readings[src]
				}
			}
		}
	case "homeassistant":
		u, t := s.ha(ctx)
		get := func(id string) (obj, error) {
			if !entityID.MatchString(id) {
				return nil, fail(422, "Invalid Home Assistant entity")
			}
			var state obj
			e := send(ctx, "GET", u+"/api/states/"+id, bearer(t), nil, &state)
			return state, e
		}
		state, e := get(str(p["ha_entity_id"]))
		if e != nil {
			return nil, e
		}
		v["state"] = strings.ToUpper(str(state["state"]))
		for goal, key := range map[string]string{"power": "ha_power_entity", "today": "ha_energy_today_entity", "total": "ha_energy_total_entity"} {
			if id := str(p[key]); id != "" {
				if reading, e := get(id); e == nil && reading["state"] != "unknown" && reading["state"] != "unavailable" {
					energy[goal] = num(reading["state"])
				}
			}
		}
	case "rest":
		h, e := restHeaders(p["rest_headers"])
		if e != nil {
			return nil, e
		}
		if str(p["rest_status_url"]) == "" {
			return nil, fail(422, "No REST status endpoint configured")
		}
		var data obj
		if e = send(ctx, "GET", str(p["rest_status_url"]), h, nil, &data); e != nil {
			return nil, e
		}
		val := readPath(data, str(p["rest_status_path"]))
		if val == nil {
			return nil, fail(502, "Missing status field in device response")
		}
		v["state"] = "OFF"
		if strings.EqualFold(str(val), str(p["rest_status_on_value"])) {
			v["state"] = "ON"
		}
	default:
		return nil, fail(422, "Plug adapter unavailable")
	}
	if len(energy) > 0 {
		v["energy"] = energy
	}
	if s.EventSink != nil && len(energy) > 0 {
		s.EventSink(obj{"type": "smart_plug_energy", "plug_id": p["id"], "plug_name": p["name"], "printer_id": p["printer_id"], "power_w": energy["power"], "energy_kwh": energy["total"]})
	}
	return v, nil
}
func (s *Service) control(ctx context.Context, p obj, action string) error {
	var state string
	switch p["plug_type"] {
	case "tasmota":
		res, e := tasmota(ctx, p, "Power "+strings.ToUpper(action))
		if e != nil {
			return e
		}
		state = power(res)
		if state == "" || (action != "toggle" && state != strings.ToUpper(action)) {
			return fail(502, "Plug did not confirm requested state")
		}
	case "homeassistant":
		u, t := s.ha(ctx)
		entity := str(p["ha_entity_id"])
		domain, _, _ := strings.Cut(entity, ".")
		if !entityID.MatchString(entity) || !switchable(domain) {
			return fail(422, "Unsupported Home Assistant entity")
		}
		service := "turn_" + action
		if action == "toggle" {
			service = "toggle"
		}
		if domain == "button" || domain == "input_button" {
			service = "press"
		}
		if domain == "automation" {
			service = "trigger"
		}
		if domain == "script" && action != "off" {
			service = "turn_on"
		}
		b, _ := json.Marshal(obj{"entity_id": entity})
		if e := send(ctx, "POST", u+"/api/services/"+domain+"/"+service, bearer(t), b, nil); e != nil {
			return e
		}
		if action != "toggle" {
			state = strings.ToUpper(action)
		}
	case "rest":
		if action == "toggle" {
			v, e := s.status(ctx, p)
			if e != nil {
				return e
			}
			action = "off"
			if v["state"] == "OFF" {
				action = "on"
			}
		}
		url := str(p["rest_"+action+"_url"])
		if url == "" {
			return fail(422, "Missing REST control endpoint")
		}
		method := str(p["rest_method"])
		if method == "" {
			method = "POST"
		}
		h, e := restHeaders(p["rest_headers"])
		if e != nil {
			return e
		}
		if e = send(ctx, method, url, h, []byte(str(p["rest_"+action+"_body"])), nil); e != nil {
			return e
		}
		state = strings.ToUpper(action)
	default:
		return fail(400, "Plug does not support power control")
	}
	_, e := s.db.ExecContext(ctx, "UPDATE smart_plugs SET last_state=nullif(?,''),last_checked=CURRENT_TIMESTAMP,auto_off_pending=0,auto_off_pending_since=NULL,auto_off_executed=CASE WHEN ?='ON' THEN FALSE ELSE auto_off_executed END WHERE id=?", state, state, p["id"])
	if e == nil && s.EventSink != nil && (state == "ON" || state == "OFF") {
		s.EventSink(obj{"type": "smart_plug_" + strings.ToLower(state), "state": strings.ToLower(state), "plug_id": p["id"], "plug_name": p["name"], "printer_id": p["printer_id"]})
	}
	return e
}
