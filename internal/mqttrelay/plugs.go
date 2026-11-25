package mqttrelay

import (
	"context"
	"encoding/json"
	"errors"
	"math"

	"strconv"
	"strings"
	"time"

	"bambastic/internal/httpx"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type source struct {
	id              int64
	field, path, on string
	multiplier      float64
}
type plugValue struct {
	state  string
	energy object
	at     time.Time
}
type plugTopics struct {
	routes map[string][]source
	last   map[int64]plugValue
}

func (c client) subscribe(ctx context.Context, topic string, cb func(string, []byte)) error {
	return wait(ctx, c.Subscribe(topic, 1, func(_ mqtt.Client, m mqtt.Message) { cb(m.Topic(), m.Payload()) }))
}
func (c client) unsubscribe(ctx context.Context, topic string) error {
	return wait(ctx, c.Unsubscribe(topic))
}

type subscriptions interface {
	subscribe(context.Context, string, func(string, []byte)) error
	unsubscribe(context.Context, string) error
}

func (s *Service) subscribePlugs(ctx context.Context, b broker) error {
	w, ok := b.(subscriptions)
	if !ok {
		return nil
	}
	rs, e := s.db.QueryContext(ctx, "SELECT id,mqtt_topic,mqtt_multiplier,mqtt_power_topic,mqtt_power_path,mqtt_power_multiplier,mqtt_energy_topic,mqtt_energy_path,mqtt_energy_multiplier,mqtt_energy_total_path,mqtt_energy_total_multiplier,mqtt_state_topic,mqtt_state_path,mqtt_state_on_value FROM smart_plugs WHERE enabled AND plug_type='mqtt' ORDER BY id")
	if e != nil {
		return e
	}
	rows, e := httpx.Rows(rs)
	if e != nil {
		return e
	}
	routes := map[string][]source{}
	for _, v := range rows {
		for _, field := range []string{"power", "today", "total", "state"} {
			key := map[string]string{"power": "power", "today": "energy", "total": "energy", "state": "state"}[field]
			topic := str(v["mqtt_"+key+"_topic"])
			if topic == "" {
				topic = str(v["mqtt_topic"])
			}
			if topic == "" || strings.ContainsAny(topic, "#+\x00\n\r") {
				continue
			}
			k := key
			if field == "total" {
				k = "energy_total"
			}
			p := str(v["mqtt_"+k+"_path"])
			if field == "total" && p == "" {
				continue
			}
			m, _ := strconv.ParseFloat(str(v["mqtt_"+k+"_multiplier"]), 64)
			if m == 1 && k != "energy_total" {
				if n, e := strconv.ParseFloat(str(v["mqtt_multiplier"]), 64); e == nil {
					m = n
				}
			}
			routes[topic] = append(routes[topic], source{id(v["id"]), field, p, str(v["mqtt_state_on_value"]), m})
		}
	}
	s.mu.Lock()
	old := s.plugs.routes
	s.plugs.routes = routes
	if s.plugs.last == nil {
		s.plugs.last = map[int64]plugValue{}
	}
	for k, list := range old {
		if v, ok := routes[k]; !ok || !sameSources(v, list) {
			for _, x := range list {
				delete(s.plugs.last, x.id)
			}
		}
	}
	s.mu.Unlock()
	for topic := range old {
		if _, ok := routes[topic]; !ok {
			if e = w.unsubscribe(ctx, topic); e != nil {
				return e
			}
		}
	}
	for k, v := range routes {
		if x, ok := old[k]; ok && sameSources(x, v) {
			continue
		}
		if e = w.subscribe(ctx, k, s.readPlug); e != nil {
			return e
		}
	}
	return nil
}
func sameSources(a, b []source) bool {
	if len(a) != len(b) {
		return false
	}
	for i, x := range a {
		if x != b[i] {
			return false
		}
	}
	return true
}
func (s *Service) readPlug(topic string, bytes []byte) {
	if len(bytes) > 1<<20 {
		return
	}
	var v any
	if json.Unmarshal(bytes, &v) != nil {
		v = strings.TrimSpace(string(bytes))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, src := range s.plugs.routes[topic] {
		value := v
		if src.path != "" {
			for _, field := range strings.Split(src.path, ".") {
				switch a := value.(type) {
				case map[string]any:
					value = a[field]
				case []any:
					n, e := strconv.Atoi(field)
					if e != nil || n < 0 || n >= len(a) {
						value = nil
					} else {
						value = a[n]
					}
				default:
					value = nil
				}
			}
		}
		if value == nil {
			continue
		}
		reading := s.plugs.last[src.id]
		if reading.energy == nil {
			reading.energy = object{}
		}
		switch src.field {
		case "state":
			if _, ok := value.(map[string]any); ok {
				continue
			}
			reading.state = "OFF"
			if src.on != "" {
				if strings.EqualFold(str(value), src.on) {
					reading.state = "ON"
				}
			} else {
				switch strings.ToLower(str(value)) {
				case "1", "on", "true", "online":
					reading.state = "ON"
				}
			}
		default:
			n, e := strconv.ParseFloat(str(value), 64)
			if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
				continue
			}
			reading.energy[src.field] = n * src.multiplier
		}
		reading.at = time.Now()
		s.plugs.last[src.id] = reading
	}
}
func (s *Service) PlugStatus(ctx context.Context, id int64) (map[string]any, error) {
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.connected {
		return nil, errors.New("MQTT broker disconnected")
	}
	d, ok := s.plugs.last[id]
	v := object{"reachable": ok && time.Since(d.at) < 5*time.Minute, "state": nil, "energy": nil}
	if !ok {
		return v, nil
	}
	if d.state != "" {
		v["state"] = d.state
	}
	if len(d.energy) > 0 {
		energy := object{}
		for k, x := range d.energy {
			energy[k] = x
		}
		v["energy"] = energy
	}
	return v, nil
}
