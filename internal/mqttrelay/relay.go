// Package mqttrelay publishes operator events to a configured MQTT 3.1.1 broker.
package mqttrelay

import (
	"context"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type object = map[string]any
type settings struct {
	enabled, tls                 bool
	host, user, password, prefix string
	port                         int
}
type broker interface {
	online() bool
	send(context.Context, string, []byte, bool) error
	close()
}
type client struct{ mqtt.Client }

func (c client) online() bool { return c.IsConnectionOpen() }
func (c client) close()       { c.Disconnect(250) }
func wait(ctx context.Context, t mqtt.Token) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.Done():
		return t.Error()
	}
}
func (c client) send(ctx context.Context, topic string, body []byte, retain bool) error {
	return wait(ctx, c.Publish(topic, 1, retain, body))
}

type readSecret func(context.Context, string) (string, error)
type Service struct {
	plugs     plugTopics
	db        *sql.DB
	secret    readSecret
	Initial   func(context.Context, access.Principal) ([]map[string]any, error)
	mu        sync.RWMutex
	current   settings
	connected bool
	input     chan object
	connect   func(context.Context, settings) (broker, error)
}

func New(db *sql.DB, secret func(context.Context, string) (string, error)) *Service {
	return &Service{db: db, secret: secret, input: make(chan object, 512), connect: connectBroker, current: settings{prefix: "bambuddy"}}
}
func (s *Service) load(ctx context.Context) (settings, error) {
	v := settings{port: 1883, prefix: "bambuddy"}
	rs, e := s.db.QueryContext(ctx, "SELECT key,value FROM settings WHERE key IN ('mqtt_enabled','mqtt_use_tls','mqtt_broker','mqtt_port','mqtt_username','mqtt_topic_prefix')")
	if e != nil {
		return v, e
	}
	for rs.Next() {
		var k, x string
		if e = rs.Scan(&k, &x); e != nil {
			rs.Close()
			return v, e
		}
		switch k {
		case "mqtt_enabled":
			v.enabled = x == "true" || x == "1"
		case "mqtt_use_tls":
			v.tls = x == "true" || x == "1"
		case "mqtt_broker":
			v.host = strings.TrimSpace(x)
		case "mqtt_port":
			v.port, _ = strconv.Atoi(x)
		case "mqtt_username":
			v.user = x
		case "mqtt_topic_prefix":
			v.prefix = strings.Trim(x, "/")
		}
	}
	e = rs.Err()
	rs.Close()
	if e != nil {
		return v, e
	}
	if v.enabled && s.secret != nil {
		v.password, e = s.secret(ctx, "mqtt_password")
	}
	return v, e
}
func connectBroker(ctx context.Context, c settings) (broker, error) {
	host := strings.Trim(c.host, "[]")
	if host == "" || strings.ContainsAny(host, "/\\?#@\r\n\x00") || c.port <= 0 || c.port > 65535 || c.prefix == "" || len(c.prefix) > 255 || strings.ContainsAny(c.prefix, "+#\x00\r\n") {
		return nil, errors.New("invalid broker configuration")
	}
	proto := "tcp"
	if c.tls {
		proto = "ssl"
	}
	addr := net.JoinHostPort(host, strconv.Itoa(c.port))
	o := mqtt.NewClientOptions().AddBroker(proto + "://" + addr).SetClientID(fmt.Sprintf("bambastic-relay-%x", time.Now().UnixNano())).SetUsername(c.user).SetPassword(c.password).SetCleanSession(true).SetConnectionLostHandler(func(mqtt.Client, error) {}).SetAutoReconnect(false).SetConnectTimeout(5 * time.Second).SetWriteTimeout(4 * time.Second).SetKeepAlive(25 * time.Second)
	o.SetWill(c.prefix+"/status", `{"status":"offline"}`, 1, true)
	o.SetCustomOpenConnectionFn(func(_ *url.URL, _ mqtt.ClientOptions) (net.Conn, error) {
		conn, e := lanhttp.DialContext(ctx, "tcp", addr)
		if e != nil || !c.tls {
			return conn, e
		}
		secure := tls.Client(conn, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
		if e = secure.HandshakeContext(ctx); e != nil {
			conn.Close()
			return nil, e
		}
		return secure, nil
	})
	cl := mqtt.NewClient(o)
	if e := wait(ctx, cl.Connect()); e != nil {
		cl.Disconnect(50)
		return nil, e
	}
	return client{cl}, nil
}
func (s *Service) Register(m *http.ServeMux) {
	m.HandleFunc("GET /api/v1/settings/mqtt/status", func(w http.ResponseWriter, r *http.Request) {
		if !access.From(r.Context()).Has("settings:read") {
			httpx.Error(w, 403, "Permission denied")
			return
		}
		s.mu.RLock()
		c, online := s.current, s.connected
		s.mu.RUnlock()
		if !c.enabled {
			c.host = ""
			c.port = 0
		}
		httpx.JSON(w, 200, object{"enabled": c.enabled, "connected": online, "broker": c.host, "port": c.port, "topic_prefix": c.prefix})
	})
}
func (s *Service) ConnectionStatus() (enabled, connected bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current.enabled, s.connected
}
func (s *Service) Publish(v map[string]any) { s.TryPublish(v) }
func (s *Service) TryPublish(v map[string]any) bool {
	kind, _ := v["type"].(string)
	if kind != "printer_status" && kind != "print_start" && kind != "print_complete" && paths[kind] == "" {
		return false
	}
	s.mu.RLock()
	online := s.connected
	s.mu.RUnlock()
	if !online {
		return false
	}
	// Allocate one immutable copy. Event producers use several named map types
	// and may reuse their input after broadcast.
	b, e := json.Marshal(v)
	if e != nil || len(b) > 4<<20 {
		return false
	}
	var msg object
	if json.Unmarshal(b, &msg) != nil {
		return false
	}
	select {
	case s.input <- msg:
		return true
	default:
		return false
	}
}

type state struct {
	last          time.Time
	online, plate bool
	ams, hms      string
}
type stream struct {
	s        *Service
	cfg      settings
	b        broker
	printers map[int64]state
}

func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		x := stream{s: s, printers: map[int64]state{}}
		lastSubs := time.Time{}
		next := time.Time{}
		delay := time.Second
		reset := func() {
			s.mu.Lock()
			s.connected = false
			s.plugs = plugTopics{}
			lastSubs = time.Time{}
			s.mu.Unlock()
			if x.b != nil {
				if x.b.online() {
					stop, cancel := context.WithTimeout(context.Background(), time.Second)
					_ = x.send(stop, "status", object{"status": "offline"}, true)
					cancel()
				}
				x.b.close()
				x.b = nil
			}
			for {
				select {
				case <-s.input:
				default:
					return
				}
			}
		}
		defer reset()
		refresh := func() {
			c, e := s.load(ctx)
			if e != nil {
				reset()
				return
			}
			if c != x.cfg {
				reset()
				x.cfg = c
				next = time.Time{}
				delay = time.Second
				s.mu.Lock()
				s.current = c
				s.mu.Unlock()
			}
			if !c.enabled {
				return
			}
			if x.b != nil && !x.b.online() {
				reset()
			}
			if x.b != nil && time.Since(lastSubs) > 3*time.Second {
				op, cancel := context.WithTimeout(ctx, 5*time.Second)
				err := s.subscribePlugs(op, x.b)
				cancel()
				if err != nil {
					reset()
					return
				}
				lastSubs = time.Now()
			}
			if x.b != nil || time.Now().Before(next) {
				return
			}
			attempt, cancel := context.WithTimeout(ctx, 6*time.Second)
			b, e := s.connect(attempt, c)
			cancel()
			if e != nil {
				next = time.Now().Add(delay)
				delay = min(30*time.Second, delay*2)
				return
			}
			x.b = b
			x.printers = map[int64]state{}
			if e = x.send(ctx, "status", object{"status": "online"}, true); e != nil {
				reset()
				return
			}
			s.mu.Lock()
			s.connected = true
			s.mu.Unlock()
			delay = time.Second
			if s.Initial != nil {
				rs, e := s.Initial(ctx, access.Principal{Admin: true})
				if e == nil {
					for _, v := range rs {
						id := v["printer_id"]
						if id == nil {
							id = v["id"]
						}
						s.Publish(object{"type": "printer_status", "printer_id": id, "data": v})
					}
				}
			}
		}
		refresh()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				refresh()
			case v := <-s.input:
				if x.b != nil {
					if e := x.event(ctx, v); e != nil {
						reset()
					}
				}
			}
		}
	}()
	return done
}
func (x *stream) send(ctx context.Context, path string, data object, retain bool) error {
	data["timestamp"] = time.Now().UTC().Format(time.RFC3339Nano)
	b, e := json.Marshal(data)
	if e != nil {
		return e
	}
	bound, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	return x.b.send(bound, x.cfg.prefix+"/"+path, b, retain)
}

var paths = map[string]string{
	"printer_online": "printers/@/online", "printer_offline": "printers/@/offline", "plate_clear_state": "printers/@/plate_clear", "plate_cleared": "printers/@/plate_clear", "ams_changed": "printers/@/ams/changed", "spool_auto_assigned": "printers/@/ams/changed", "printer_error": "printers/@/error", "hms_error": "printers/@/error", "filament_low": "printers/@/filament/low",
	"queue_item_added": "queue/job_added", "queue_item_acked": "queue/job_started", "queue_item_completed": "queue/job_completed", "queue_item_failed": "queue/job_failed", "maintenance_alert": "maintenance/alert", "maintenance_reset": "maintenance/reset", "archive_created": "archive/created", "archive_updated": "archive/updated", "run_discarded": "printers/@/run/discarded", "smart_plug_on": "smart_plugs/on", "smart_plug_off": "smart_plugs/off", "smart_plug_energy": "smart_plugs/energy",
}

func str(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprint(v)
}
func id(v any) int64 { i, _ := strconv.ParseInt(str(v), 10, 64); return i }
func keys(m object, fields string) object {
	o := object{}
	for _, k := range strings.Fields(fields) {
		if v, ok := m[k]; ok {
			o[k] = v
		}
	}
	return o
}

var serial = regexp.MustCompile(`^[[:alnum:]_-]{1,128}$`)

func (x *stream) event(ctx context.Context, v object) error {
	kind := str(v["type"])
	data := object{}
	if m, ok := v["data"].(object); ok {
		for k, v := range m {
			data[k] = v
		}
	}
	for k, a := range v {
		if k != "type" && k != "data" {
			data[k] = a
		}
	}
	pid := id(data["printer_id"])
	var name, sn string
	var plate bool
	if pid > 0 {
		e := x.s.db.QueryRowContext(ctx, "SELECT name,serial_number,awaiting_plate_clear FROM printers WHERE id=?", pid).Scan(&name, &sn, &plate)
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if !serial.MatchString(sn) {
			return nil
		}
	}
	printer := "printers/" + sn + "/"
	identity := func(m object) object {
		if pid > 0 {
			m["printer_id"] = pid
			m["printer_name"] = name
			m["printer_serial"] = sn
		}
		return m
	}
	if kind == "printer_status" && pid > 0 {
		online, _ := data["connected"].(bool)
		prev, known := x.printers[pid]
		transition := !known || prev.online != online
		if transition {
			path, state := "offline", "offline"
			if online {
				path, state = "online", "online"
			}
			if e := x.send(ctx, printer+path, identity(object{"event": "printer_" + state}), false); e != nil {
				return e
			}
		}
		if !known || plate != prev.plate {
			if e := x.send(ctx, printer+"plate_clear", identity(object{"awaiting_plate_clear": plate}), true); e != nil {
				return e
			}
			prev.plate = plate
		}
		if !transition && time.Since(prev.last) < time.Second {
			x.printers[pid] = prev
			return nil
		}
		status := keys(data, "connected state progress remaining_time layer_num total_layers current_print subtask_name gcode_file temperatures wifi_signal chamber_light speed_level cooling_fan_speed big_fan1_speed big_fan2_speed heatbreak_fan_speed left_aux_fan_speed exhaust_fan_present door_open active_extruder tray_now ams vt_tray hms_errors")
		status["awaiting_plate_clear"] = plate
		if e := x.send(ctx, printer+"status", identity(status), true); e != nil {
			return e
		}
		for _, k := range []string{"ams", "hms_errors"} {
			v, ok := status[k]
			if !ok {
				continue
			}
			b, _ := json.Marshal(v)
			path := "ams/changed"
			last := &prev.ams
			if k == "hms_errors" {
				path = "error"
				last = &prev.hms
			}
			if known && *last != "" && string(b) != *last && string(b) != "null" && (k != "hms_errors" || string(b) != "[]") {
				if e := x.send(ctx, printer+path, identity(object{k: v}), false); e != nil {
					return e
				}
			}
			*last = string(b)
		}
		prev.online, prev.last = online, time.Now()
		x.printers[pid] = prev
		return nil
	}
	if (kind == "print_start" || kind == "print_complete") && pid > 0 {
		run := id(data["run_id"])
		payload := keys(data, "run_id archive_id queue_item_id print_name status plate_id")
		if run > 0 {
			rows, e := x.s.db.QueryContext(ctx, "SELECT id run_id,archive_id,queue_item_id,plate_id,print_name,status,started_at,completed_at,duration_seconds,duration_unmeasured,failure_reason,filament_used_grams,cost,energy_kwh FROM print_log_entries WHERE id=? AND printer_id=?", run, pid)
			if e != nil {
				return e
			}
			rs, e := httpx.Rows(rows)
			if e != nil {
				return e
			}
			if len(rs) > 0 {
				payload = rs[0]
			}
		}
		printTopic, runTopic := "print/started", "run/started"
		if kind == "print_complete" {
			printTopic, runTopic = "print/completed", "run/completed"
			switch str(payload["status"]) {
			case "failed", "cancelled", "aborted":
				printTopic = "print/failed"
			}
		}
		if e := x.send(ctx, printer+printTopic, identity(payload), false); e != nil {
			return e
		}
		if run > 0 {
			if e := x.send(ctx, printer+runTopic, identity(payload), false); e != nil {
				return e
			}
		}
		if kind == "print_complete" && id(payload["queue_item_id"]) > 0 {
			topic := "queue/job_completed"
			if printTopic == "print/failed" {
				topic = "queue/job_failed"
			}
			return x.send(ctx, topic, identity(payload), false)
		}
		return nil
	}
	topic := paths[kind]
	if topic == "" || strings.Contains(topic, "@") && pid <= 0 {
		return nil
	}
	topic = strings.Replace(topic, "@", sn, 1)
	m := keys(data, "event_id run_id archive_id print_name plate_id status success queue_item_id job_id file_name progress duration_seconds filament_used_grams cost error_code severity message reason description task_name due_date maintenance_type spool_id ams_id tray_id filament_type color_name remaining_percent plug_id plug_name state power_w energy_kwh awaiting_plate_clear")
	if data["queue_item_id"] != nil && m["job_id"] == nil {
		m["job_id"] = data["queue_item_id"]
	}
	if strings.HasSuffix(topic, "/plate_clear") {
		m["awaiting_plate_clear"] = plate
	}
	return x.send(ctx, topic, identity(m), strings.HasSuffix(topic, "/plate_clear"))
}
