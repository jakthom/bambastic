package production

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/eclipse/paho.mqtt.golang/packets"
)

type hardware interface {
	Publish(context.Context, row) error
	Upload(context.Context, string, string, func(int64)) error
	Online() bool
	Close()
	Fingerprint() string
}

// A nil report handler requests an authenticated session for credential verification only.
type openDevice func(context.Context, row, string, func(row)) (hardware, error)
type liveDevice struct {
	drift       kDrift
	kReport     row
	kEpoch      uint64
	logs        messageHistory
	calibration calibrationLink
	h           hardware
	report      row
	printer     row
	sampleAt    time.Time
}
type lan struct {
	wire        slicerWire
	history     *messageHistory
	client      mqtt.Client
	printer     row
	fingerprint string
}

func await(ctx context.Context, t mqtt.Token) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.Done():
		return t.Error()
	}
}

var (
	errPrinterKeyChanged = errors.New("printer certificate changed")
	errPrinterStatus     = errors.New("could not receive printer status after MQTT login")
)

func checkedTLS(pin string) (*tls.Config, *string) {
	var got string
	var first sync.Once
	// Printers present device-issued certificates. Enrol the key at explicit registration
	// and verify the key on subsequent connections, even when the IP address changes.
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, ClientSessionCache: tls.NewLRUClientSessionCache(4), VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("printer certificate missing")
		}
		sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
		current := hex.EncodeToString(sum[:])
		first.Do(func() { got = current })
		if current != got || pin != "" && current != pin {
			return errPrinterKeyChanged
		}
		return nil
	}}
	return cfg, &got
}

// DeviceTLS verifies a previously enrolled printer public key.
func DeviceTLS(pin string) *tls.Config {
	cfg, _ := checkedTLS(pin)
	if pin == "" {
		cfg.VerifyConnection = func(tls.ConnectionState) error { return errors.New("printer has no enrolled key") }
	}
	return cfg
}
func openLAN(ctx context.Context, p row, pin string, receive func(row)) (hardware, error) {
	return openLANAt(ctx, net.JoinHostPort(val(p["ip_address"]), "8883"), p, pin, receive)
}

func openLANAt(ctx context.Context, address string, p row, pin string, receive func(row)) (hardware, error) {
	serial := val(p["serial_number"])
	cfg, key := checkedTLS(pin)
	o := mqtt.NewClientOptions()
	o.AddBroker("ssl://" + address)
	o.SetProtocolVersion(4) // MQTT 3.1.1
	o.SetClientID("bb-" + uuid()[:18])
	o.SetUsername("bblp")
	o.SetPassword(val(p["access_code"]))
	o.SetTLSConfig(cfg)
	o.SetAutoReconnect(false)
	o.SetConnectTimeout(5 * time.Second)
	o.SetWriteTimeout(5 * time.Second)
	o.SetOrderMatters(true)
	o.SetKeepAlive(25 * time.Second)
	c := mqtt.NewClient(o)
	if e := await(ctx, c.Connect()); e != nil {
		if ctx.Err() != nil {
			c.Disconnect(100)
		}
		return nil, e
	}
	l := &lan{client: c, printer: p, fingerprint: *key}
	if receive == nil {
		return l, nil
	}
	first := make(chan struct{}, 1)
	topic := "device/" + serial + "/report"
	sub := c.Subscribe(topic, 0, func(_ mqtt.Client, m mqtt.Message) {
		var envelope map[string]any
		if len(m.Payload()) > 4<<20 || json.Unmarshal(m.Payload(), &envelope) != nil {
			return
		}
		l.wire.receive(envelope)
		print, _ := envelope["print"].(map[string]any)
		status := isPrinterStatus(print)
		info, _ := envelope["info"].(map[string]any)
		modules, _ := info["module"].([]any)
		for _, child := range modules {
			mod, _ := child.(map[string]any)
			if mod["name"] == "ota" && val(mod["sw_ver"]) != "" {
				if print == nil {
					print = map[string]any{}
				}
				print["firmware_version"] = mod["sw_ver"]
				break
			}
		}
		if len(print) == 0 {
			return
		}
		receive(print)
		if status {
			select {
			case first <- struct{}{}:
			default:
			}
		}
	})
	e := await(ctx, sub)
	if ack, ok := sub.(*mqtt.SubscribeToken); e == nil && ok && ack.Result()[topic] == 0x80 {
		e = errors.New("printer rejected the status subscription")
	}
	if e == nil {
		e = l.Publish(ctx, row{"pushing": row{"sequence_id": "0", "command": "pushall"}})
	}
	if e == nil {
		select {
		case <-first:
		case <-ctx.Done():
			e = ctx.Err()
		}
	}
	if e != nil {
		l.Close()
		return nil, fmt.Errorf("%w: %w", errPrinterStatus, e)
	}
	l.Publish(ctx, packet("info", "get_version", nil))
	return l, nil
}

func isPrinterStatus(print row) bool {
	switch print["command"] {
	case "push_status":
		return true
	case nil, "":
		// Some firmware omits the command on status deltas. A bare sequence
		// number or command result gives us no telemetry.
		if print["result"] == nil && print["reason"] == nil {
			for key := range print {
				if key != "command" && key != "sequence_id" {
					return true
				}
			}
		}
	}
	return false
}

func mqttAuthRejected(err error) bool {
	return errors.Is(err, packets.ErrorRefusedBadUsernameOrPassword) || errors.Is(err, packets.ErrorRefusedNotAuthorised)
}

func (l *lan) Fingerprint() string { return l.fingerprint }
func (l *lan) Online() bool        { return l.client.IsConnectionOpen() }
func (l *lan) Close()              { l.client.Disconnect(100) }
func (l *lan) Publish(ctx context.Context, m row) error {
	topic := "device/" + val(l.printer["serial_number"]) + "/request"
	if l.history != nil {
		l.history.add(topic, "out", m)
	}
	qos := byte(1)
	push, _ := m["pushing"].(row)
	info, _ := m["info"].(row)
	if len(m) == 1 && (push["command"] == "pushall" || info["command"] == "get_version") {
		// P1S firmware can answer these reads without sending a QoS 1 PUBACK.
		// The status subscription supplies the evidence that the printer answered.
		qos = 0
	}
	return await(ctx, l.client.Publish(topic, qos, false, jstr(m)))
}

type progressReader struct {
	r   io.Reader
	ctx context.Context
	n   int64
	f   func(int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	if e := p.ctx.Err(); e != nil {
		return 0, e
	}
	n, e := p.r.Read(b)
	p.n += int64(n)
	if n > 0 {
		p.f(p.n)
	}
	return n, e
}
func (l *lan) Upload(ctx context.Context, src, name string, progress func(int64)) error {
	conn, e := l.files(ctx)
	if e != nil {
		return e
	}
	defer conn.Quit()
	f, e := os.Open(src)
	if e != nil {
		return e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return e
	}
	e = conn.Stor(name, &progressReader{f, ctx, 0, progress})
	if e != nil {
		return e
	}
	n, e := conn.FileSize(name)
	if e != nil {
		return e
	}
	if n != info.Size() {
		return errors.New("incomplete transfer on printer")
	}
	return nil
}
func (s *Service) printers(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "printers:read"); e != nil {
		return nil, e
	}
	rs, e := records(r.Context(), s.db, "SELECT * FROM printers ORDER BY name,id")
	out := []row{}
	p := access.From(r.Context())
	for _, v := range rs {
		if p.AllowsPrinter(num(v["id"])) {
			out = append(out, showPrinter(v, p))
		}
	}
	return out, e
}
func (s *Service) printerRow(r *http.Request, perm string) (row, error) {
	if e := permitted(r, perm); e != nil {
		return nil, e
	}
	if !access.From(r.Context()).AllowsPrinter(pathID(r)) {
		return nil, fail(404, "Printer not found")
	}
	return one(r.Context(), s.db, "SELECT * FROM printers WHERE id=?", pathID(r))
}
func (s *Service) readPrinter(w http.ResponseWriter, r *http.Request) (any, error) {
	v, e := s.printerRow(r, "printers:read")
	return showPrinter(v, access.From(r.Context())), e
}

var serialPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,48}$`)

func (s *Service) addPrinter(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "printers:create"); e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	if e = encodeWLED(m); e != nil {
		return nil, e
	}
	ip := net.ParseIP(val(m["ip_address"]))
	serial := val(m["serial_number"])
	name := strings.TrimSpace(val(m["name"]))
	code := val(m["access_code"])
	if ip == nil || ip.IsMulticast() || ip.IsUnspecified() || !serialPattern.MatchString(serial) || name == "" || len(name) > 200 || len(code) == 0 || len(code) > 32 {
		return nil, fail(422, "Name, IP address, serial number and access code required")
	}
	ctx, cancel := context.WithTimeout(r.Context(), 9*time.Second)
	defer cancel()
	exist, e := one(ctx, s.db, "SELECT count(*) n FROM printers WHERE serial_number=?", serial)
	if e != nil {
		return nil, e
	}
	if num(exist["n"]) > 0 {
		return nil, fail(400, "Printer serial number already registered")
	}
	dev, e := s.open(ctx, m, "", nil)
	if e != nil {
		if mqttAuthRejected(e) {
			return nil, fail(400, "The printer refused the MQTT login. Check the LAN access code on the printer.")
		}
		return nil, fail(400, "Could not establish an MQTT session with the printer on port 8883.")
	}
	pin := dev.Fingerprint()
	dev.Close()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	v := row{"name": name, "serial_number": serial, "ip_address": ip.String(), "access_code": code, "model": m["model"], "location": m["location"], "nozzle_count": 1, "is_active": true, "auto_archive": true, "print_hours_offset": 0, "runtime_seconds": 0, "external_camera_enabled": false, "camera_rotation": 0, "plate_detection_enabled": false, "awaiting_plate_clear": false}
	if e = encodeCamera(m, nil); e != nil {
		return nil, e
	}
	for _, key := range cameraFields {
		if x, ok := m[key]; ok {
			v[key] = x
		}
	}
	v["wled_config"] = m["wled_config"]
	if m["auto_archive"] != nil {
		v["auto_archive"] = m["auto_archive"]
	}
	k, e := insert(ctx, tx, "printers", v)
	if e != nil {
		return nil, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO bb_printer_trust VALUES(?,?)", k, pin); e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	v, e = one(ctx, s.db, "SELECT * FROM printers WHERE id=?", k)
	return showPrinter(v, access.From(r.Context())), e
}
func (s *Service) editPrinter(w http.ResponseWriter, r *http.Request) (any, error) {
	pr, e := s.printerRow(r, "printers:update")
	if e != nil {
		return nil, e
	}
	m, e := input(w, r)
	if e != nil {
		return nil, e
	}
	if sn, ok := m["serial_number"]; ok && val(sn) != val(pr["serial_number"]) {
		return nil, fail(422, "The enrolled printer serial number cannot be reassigned")
	}
	if e = encodeCamera(m, pr); e != nil {
		return nil, e
	}
	if val(m["access_code"]) == "" {
		delete(m, "access_code")
	}
	if (m["ip_address"] != nil && m["ip_address"] != pr["ip_address"]) || (m["access_code"] != nil && m["access_code"] != pr["access_code"]) {
		desired := row{}
		for k, v := range pr {
			desired[k] = v
		}
		for _, k := range []string{"ip_address", "access_code"} {
			if m[k] != nil {
				desired[k] = m[k]
			}
		}
		ip := net.ParseIP(val(desired["ip_address"]))
		if ip == nil || ip.IsUnspecified() || ip.IsMulticast() || len(val(desired["access_code"])) > 32 {
			return nil, fail(422, "Invalid printer address or access code")
		}
		trust, e := one(r.Context(), s.db, "SELECT fingerprint FROM bb_printer_trust WHERE printer_id=?", pr["id"])
		if e != nil {
			return nil, e
		}
		if e = s.VerifyPrinterConnection(r.Context(), desired, val(trust["fingerprint"])); e != nil {
			return nil, fail(409, e.Error())
		}
	}
	if e = encodeWLED(m); e != nil {
		return nil, e
	}
	if e = update(r.Context(), s.db, "printers", pathID(r), m, append([]string{"name", "ip_address", "access_code", "is_active", "model", "location", "auto_archive", "print_hours_offset", "wear_cost_per_hour", "dispatch_priority", "wled_config"}, cameraFields...)); e != nil {
		return nil, e
	}
	return s.readPrinter(w, r)
}

// VerifyPrinterConnection checks a replacement address or LAN code against the enrolled printer.
func (s *Service) VerifyPrinterConnection(ctx context.Context, printer map[string]any, pin string) error {
	if pin == "" {
		return errors.New("The printer has no enrolled certificate")
	}
	ctx, cancel := context.WithTimeout(ctx, 9*time.Second)
	defer cancel()
	h, err := s.open(ctx, printer, pin, nil)
	if mqttAuthRejected(err) {
		return errors.New("The printer refused the LAN access code. Check the code on the printer")
	}
	if err != nil {
		return errors.New("Could not verify the enrolled printer. Check its address and connection")
	}
	h.Close()
	return nil
}

func (s *Service) deletePrinter(w http.ResponseWriter, r *http.Request) (any, error) {
	if _, e := s.printerRow(r, "printers:delete"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	v, e := one(ctx, s.db, "SELECT count(*) n FROM print_queue WHERE printer_id=? AND status IN ('pending','printing','dispatching')", pathID(r))
	if e != nil {
		return nil, e
	}
	if num(v["n"]) > 0 {
		return nil, fail(409, "Printer has queued or running work")
	}
	if _, e = s.db.ExecContext(ctx, "DELETE FROM printers WHERE id=?", pathID(r)); e != nil {
		return nil, fail(409, "Printer is referenced by production history")
	}
	s.kick()
	return row{"status": "deleted"}, nil
}
func (s *Service) state(k int64, pr row) row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[k]
	rp := row{}
	online := false
	if d != nil {
		rp = d.report
		online = d.h != nil && d.h.Online()
	}
	st := val(rp["gcode_state"])
	if !online {
		st = "OFFLINE"
	} else if st == "" {
		st = "UNKNOWN"
	}
	ams := []any{}
	if a, ok := rp["ams"].(map[string]any); ok {
		if arr, ok := a["ams"].([]any); ok {
			ams = arr
		}
	}
	v := row{"id": k, "printer_id": k, "name": pr["name"], "connected": online, "state": st, "progress": num(rp["mc_percent"]), "remaining_time": num(rp["mc_remaining_time"]), "current_print": rp["subtask_name"], "gcode_file": rp["gcode_file"], "subtask_name": rp["subtask_name"], "layer_num": num(rp["layer_num"]), "total_layers": num(rp["total_layer_num"]), "temperatures": map[string]any{"nozzle": rp["nozzle_temper"], "nozzle_target": rp["nozzle_target_temper"], "bed": rp["bed_temper"], "bed_target": rp["bed_target_temper"], "chamber": rp["chamber_temper"]}, "ams": ams, "vt_tray": rp["vt_tray"], "vir_slot": rp["vir_slot"], "ams_exists": len(ams) > 0, "awaiting_plate_clear": flag(pr["awaiting_plate_clear"]), "hms_errors": rp["hms"]}
	augmentState(v, pr, rp)
	return v
}
func (s *Service) status(w http.ResponseWriter, r *http.Request) (any, error) {
	v, e := s.printerRow(r, "printers:read")
	if e != nil {
		return nil, e
	}
	return s.state(pathID(r), v), nil
}
func (s *Service) clearPlate(w http.ResponseWriter, r *http.Request) (any, error) {
	if _, e := s.printerRow(r, "printers:clear_plate"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if _, e = tx.ExecContext(ctx, "UPDATE printers SET awaiting_plate_clear=0 WHERE id=?", pathID(r)); e != nil {
		return nil, e
	}
	d, e := access.Setting(ctx, tx, "confirm_default_good_on_plate_clear", "false")
	if e != nil {
		return nil, e
	}
	if flag(d) {
		run, e := one(ctx, tx, "SELECT l.* FROM print_log_entries l JOIN print_archives a ON a.id=l.archive_id WHERE l.printer_id=? AND l.status='completed' AND l.confirm_requested AND l.user_verdict IS NULL AND l.confirm_token_used_at IS NULL AND a.deleted_at IS NULL ORDER BY l.completed_at DESC,l.id DESC LIMIT 1", pathID(r))
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, e
		}
		if run != nil {
			a, e := one(ctx, tx, "SELECT * FROM print_archives WHERE id=?", run["archive_id"])
			if e != nil {
				return nil, e
			}
			if e = recordOutcome(ctx, tx, a, run, row{"user_verdict": "good"}, "plate_clear"); e != nil {
				return nil, e
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	s.kick()
	return row{"status": "cleared"}, nil
}
func (s *Service) deviceCommand(ctx context.Context, k int64, act string) (any, error) {
	s.mu.RLock()
	d := s.devices[k]
	var h hardware
	if d != nil {
		h = d.h
	}
	s.mu.RUnlock()
	if h == nil || !h.Online() {
		return nil, fail(503, "Printer is offline")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	if e := h.Publish(ctx, row{"print": row{"command": act, "sequence_id": "2000", "param": ""}}); e != nil {
		return nil, fail(503, "Printer did not acknowledge command delivery")
	}
	return row{"status": "sent", "printer_id": k}, nil
}
func (s *Service) control(w http.ResponseWriter, r *http.Request) (any, error) {
	if _, e := s.printerRow(r, "printers:control"); e != nil {
		return nil, e
	}
	a := r.PathValue("action")
	if a != "pause" && a != "resume" && a != "stop" {
		return nil, fail(404, "Unknown printer action")
	}
	return s.deviceCommand(r.Context(), pathID(r), a)
}
func startMessage(name string, v row) row {
	remote := "/" + filepath.Base(name)
	if strings.HasSuffix(name, ".gcode") {
		return row{"print": row{"command": "gcode_file", "sequence_id": startSequence(name), "param": remote}}
	}
	tri := func(x any) int {
		switch val(x) {
		case "on":
			return 1
		case "off":
			return 0
		default:
			return 2
		}
	}
	p := row{"sequence_id": startSequence(name), "command": "project_file", "param": fmt.Sprintf("Metadata/plate_%d.gcode", num(v["plate_id"])), "url": "ftp://" + remote, "file": remote, "subtask_name": strings.TrimSuffix(name, filepath.Ext(name)), "md5": "", "bed_type": "auto", "timelapse": flag(v["timelapse"]), "bed_leveling": tri(v["bed_levelling"]) == 1, "auto_bed_leveling": tri(v["bed_levelling"]), "flow_cali": tri(v["flow_cali"]) == 1, "auto_flow_cali": tri(v["flow_cali"]), "layer_inspect": flag(v["layer_inspect"]), "vibration_cali": flag(v["vibration_cali"]), "use_ams": flag(v["use_ams"])}
	p["extrude_cali_flag"] = tri(v["flow_cali"])
	p["nozzle_offset_cali"] = tri(v["nozzle_offset_cali"])
	for _, key := range []string{"nozzle_mapping", "nozzles_info"} {
		if x := unpack(v[key]); x != nil {
			p[key] = x
		}
	}
	if ams := unpack(v["ams_mapping"]); ams != nil {
		p["ams_mapping"] = ams
	}
	return row{"print": p}
}
