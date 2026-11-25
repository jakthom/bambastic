package virtualprinter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

func readPacket(r *bufio.Reader) (byte, []byte, error) {
	k, e := r.ReadByte()
	if e != nil {
		return 0, nil, e
	}
	n := 0
	for shift := 0; shift < 28; shift += 7 {
		b, e := r.ReadByte()
		if e != nil {
			return 0, nil, e
		}
		n += int(b&127) << shift
		if n > 4<<20 {
			return 0, nil, errors.New("MQTT message too large")
		}
		if b&128 == 0 {
			data := make([]byte, n)
			_, e = io.ReadFull(r, data)
			return k, data, e
		}
	}
	return 0, nil, errors.New("invalid MQTT length")
}
func writePacket(w io.Writer, k byte, data []byte) error {
	hdr := []byte{k}
	for n := len(data); ; {
		b := byte(n % 128)
		n /= 128
		if n != 0 {
			b |= 128
		}
		hdr = append(hdr, b)
		if n == 0 {
			break
		}
	}
	if _, e := w.Write(hdr); e != nil {
		return e
	}
	_, e := w.Write(data)
	return e
}
func readStr(p *[]byte) (string, bool) {
	d := *p
	if len(d) < 2 {
		return "", false
	}
	n := int(binary.BigEndian.Uint16(d[:2]))
	if n > len(d)-2 {
		return "", false
	}
	*p = d[2+n:]
	return string(d[2 : 2+n]), true
}
func mqttString(s string) []byte { return append([]byte{byte(len(s) >> 8), byte(len(s))}, s...) }
func (v *instance) authenticateConnect(p []byte) bool {
	protocol, ok := readStr(&p)
	if !ok || protocol != "MQTT" || len(p) < 4 || p[0] != 4 {
		return false
	}
	flags := p[1]
	p = p[4:]
	if _, ok = readStr(&p); !ok {
		return false
	}
	if flags&4 != 0 {
		if _, ok = readStr(&p); !ok {
			return false
		}
		if _, ok = readStr(&p); !ok {
			return false
		}
	}
	if flags&0xc0 != 0xc0 {
		return false
	}
	user, ok := readStr(&p)
	if !ok {
		return false
	}
	pass, ok := readStr(&p)
	return ok && user == "bblp" && subtle.ConstantTimeCompare([]byte(pass), []byte(v.c.Code)) == 1
}

var topicSerial = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,48}$`)
var plateParam = regexp.MustCompile(`(?:^|/)plate_(\d+)\.gcode$`)

type cachedOptions struct {
	data object
	at   time.Time
}

func (v *instance) optionsFor(name string) object {
	v.mu.Lock()
	defer v.mu.Unlock()
	o := v.options[name]
	delete(v.options, name)
	if time.Since(o.at) > 30*time.Second {
		return nil
	}
	return o.data
}
func (v *instance) remember(ctx context.Context, name string, o object) {
	if name == "" || name == "." || name == "/" {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	b, _ := json.Marshal(o)
	tx, e := v.s.db.BeginTx(ctx, nil)
	if e != nil {
		return
	}
	defer tx.Rollback()
	res, e := tx.ExecContext(ctx, "UPDATE bb_vp_received SET options=?,ready_at=unixepoch() WHERE id=(SELECT max(id) FROM bb_vp_received WHERE vp_id=? AND filename=? AND config_key=? AND error IS NULL)", string(b), v.c.ID, name, v.c.fingerprint())
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		for _, field := range []string{"use_ams", "timelapse", "vibration_cali", "layer_inspect", "bed_levelling", "flow_cali", "nozzle_offset_cali", "ams_mapping"} {
			value := o[field]
			if value == nil {
				continue
			}
			if field == "ams_mapping" {
				if !v.c.SaveMapping || v.c.Target == 0 || v.c.ForceColor {
					continue
				}
				js, _ := json.Marshal(value)
				value = string(js)
			}
			res, e = tx.ExecContext(ctx, "UPDATE print_queue SET "+field+"=? WHERE status='pending' AND id IN (SELECT queue_id FROM bb_vp_job WHERE vp_id=? AND filename=? AND created>=unixepoch()-30 AND config_key=? AND created=(SELECT max(created) FROM bb_vp_job WHERE vp_id=? AND filename=?)) AND (?=0 OR plate_id=?)", value, v.c.ID, name, v.c.fingerprint(), v.c.ID, name, number(o["plate_id"]), number(o["plate_id"]))
			if e != nil {
				return
			}
			changed, _ := res.RowsAffected()
			n += changed
		}
	}
	if tx.Commit() != nil {
		return
	}
	if n == 0 {
		if len(v.options) >= 128 {
			v.options = map[string]cachedOptions{}
		}
		v.options[name] = cachedOptions{o, time.Now()}
	}
	v.s.changed()
}
func (v *instance) report() object {
	p := object{"command": "push_status", "msg": 0, "sequence_id": "0", "gcode_state": "IDLE", "mc_percent": 0, "mc_remaining_time": 0, "nozzle_temper": 0, "bed_temper": 0, "sdcard": true, "gcode_file": "", "subtask_name": "", "ams": object{"ams": []any{}, "ams_exist_bits": "0", "tray_exist_bits": "0"}, "ipcam": object{"ipcam_dev": "0"}}
	if v.c.Target != 0 {
		raw, _, serial := v.s.farm.SlicerStatus(v.c.Target, v.c.Pin)
		if raw != nil {
			v.virtualize(raw, serial)
			for k, x := range raw {
				p[k] = x
			}
			p["command"] = "push_status"
			p["msg"] = 0
			p["sequence_id"] = "0"
			switch p["gcode_state"] {
			case "RUNNING", "PAUSE", "PREPARE", "SLICING", "INIT":
				p["gcode_state"] = "FINISH"
			}
		}
	}
	v.mu.Lock()
	if v.lastUpload != nil && time.Since(v.uploadTime) < 15*time.Second {
		for k, x := range v.lastUpload {
			p[k] = x
		}
	}
	v.mu.Unlock()
	return object{"print": p}
}
func (v *instance) uploadStatus(state, name, subtask string) object {
	if subtask == "" {
		subtask = strings.TrimSuffix(name, filepath.Ext(name))
	}
	pct := "0"
	progress := 0
	if state == "FINISH" {
		pct = "100"
		progress = 100
	}
	event := object{"command": "push_status", "sequence_id": "0", "msg": 1, "gcode_state": state, "mc_percent": progress, "gcode_file_prepare_percent": pct, "gcode_file": name, "subtask_name": subtask, "mc_remaining_time": 0}
	v.mu.Lock()
	v.lastUpload = event
	v.uploadTime = time.Now()
	v.mu.Unlock()
	return object{"print": event}
}
func projectName(m object) string {
	name := str(m["file"])
	if name == "" {
		u, e := url.Parse(str(m["url"]))
		if e == nil {
			name = u.Path
			if name == "" {
				name = u.Host
			}
		}
	}
	if name == "" {
		name = str(m["subtask_name"])
		if name != "" && !strings.EqualFold(filepath.Ext(name), ".3mf") {
			name += ".3mf"
		}
	}
	return filepath.Base(name)
}

func (v *instance) awaitIntake(ctx context.Context, name, subtask string, push func(object) error) {
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	timeout := time.NewTimer(90 * time.Second)
	defer timeout.Stop()
	for {
		var state string
		e := v.s.db.QueryRowContext(ctx, `SELECT coalesce((SELECT CASE WHEN error IS NULL THEN 'PREPARE' ELSE 'FAILED' END FROM bb_vp_received WHERE vp_id=? AND filename=? AND config_key=? ORDER BY id DESC LIMIT 1),(SELECT 'FINISH' FROM bb_vp_receipts WHERE vp_id=? AND filename=? AND config_key=? AND completed>unixepoch()-120),'PREPARE')`, v.c.ID, name, v.c.fingerprint(), v.c.ID, name, v.c.fingerprint()).Scan(&state)
		if e == nil && (state == "FINISH" || state == "FAILED") {
			if ctx.Err() == nil {
				_ = push(v.uploadStatus(state, name, subtask))
			}
			return
		}
		if e != nil && !errors.Is(e, sql.ErrNoRows) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			_ = push(v.uploadStatus("FAILED", name, subtask))
			return
		case <-tick.C:
		}
	}
}

// Rewrite every embedded physical serial matching the target. Bambu Studio
// reads net.info[].ip as little-endian IPv4 when choosing its FTP destination.
func (v *instance) virtualize(m object, physicalSerial string) {
	ip := v.s.listeningIP(v.c)
	rewrite(m, v.c.TargetIP, ip)
	var walk func(object)
	walk = func(m object) {
		for key, value := range m {
			switch x := value.(type) {
			case string:
				if key == "sn" && x == physicalSerial && physicalSerial != "" {
					m[key] = v.c.Serial
				}
			case map[string]any:
				walk(x)
			case []any:
				for _, entry := range x {
					if child, ok := entry.(map[string]any); ok {
						walk(child)
					}
				}
			}
		}
	}
	walk(m)
	if netInfo, ok := m["net"].(map[string]any); ok {
		entries, _ := netInfo["info"].([]any)
		for _, entry := range entries {
			if x, ok := entry.(map[string]any); ok {
				if addr := net.ParseIP(ip).To4(); addr != nil {
					x["ip"] = binary.LittleEndian.Uint32(addr)
				}
			}
		}
	}
}

func (v *instance) forward(section string, m object, reply object) object {
	stack, e := v.s.config(v.ctx)
	current := false
	for _, c := range stack {
		if e == nil && v.c.ID == c.ID && c.Enabled && c.fingerprint() == v.c.fingerprint() {
			current = true
			break
		}
	}
	if current {
		raw, err := v.s.farm.SlicerCommand(v.ctx, v.c.Target, v.c.Pin, section, m)
		if err == nil {
			_, _, serial := v.s.farm.SlicerStatus(v.c.Target, v.c.Pin)
			v.virtualize(raw, serial)
			return object{section: raw}
		}
		reply["reason"] = err.Error()
	} else {
		reply["reason"] = "Virtual printer configuration changed"
	}
	reply["result"] = "fail"
	return object{section: reply}
}

func (v *instance) response(msg object) object {
	for _, section := range []string{"pushing", "info", "print", "system", "xcam", "camera"} {
		m, ok := msg[section].(map[string]any)
		if !ok {
			continue
		}
		command := str(m["command"])
		reply := object{"command": command, "sequence_id": m["sequence_id"], "result": "success", "reason": ""}
		if len(msg) != 1 {
			return object{section: object{"command": command, "sequence_id": m["sequence_id"], "result": "fail", "reason": "Expected one command"}}
		}
		if section == "pushing" && command == "start" {
			return v.report()
		}
		switch command {
		case "pushall", "push_status":
			return v.report()
		case "get_version":
			if v.c.Target != 0 {
				_, version, serial := v.s.farm.SlicerStatus(v.c.Target, v.c.Pin)
				if version != nil {
					v.virtualize(version, serial)
					version["sequence_id"] = m["sequence_id"]
					return object{section: version}
				}
			}
			reply["module"] = []object{{"name": "ota", "sw_ver": "01.07.00.00", "hw_ver": v.c.Model, "sn": v.c.Serial}, {"name": "mc", "sw_ver": "01.07.00.00", "hw_ver": v.c.Model, "sn": v.c.Serial}}
		case "project_file", "gcode_file":
			name := projectName(m)
			if !acceptedName(name) {
				reply["result"] = "fail"
				reply["reason"] = "invalid upload name"
				return object{section: reply}
			}
			o := object{}
			for _, k := range []string{"ams_mapping", "use_ams", "timelapse", "vibration_cali", "layer_inspect"} {
				if m[k] != nil {
					o[k] = m[k]
				}
			}
			for _, k := range []string{"bed_leveling", "flow_cali", "nozzle_offset_cali"} {
				if value, exists := m[k]; exists {
					field := k
					if k == "bed_leveling" {
						field = "bed_levelling"
					}
					switch str(value) {
					case "auto", "2":
						o[field] = "auto"
					case "on", "true", "1":
						o[field] = "on"
					case "off", "false", "0":
						o[field] = "off"
					}
				}
			}
			if hit := plateParam.FindStringSubmatch(str(m["param"])); hit != nil {
				o["plate_id"], _ = strconv.ParseInt(hit[1], 10, 64)
			}
			v.remember(v.ctx, name, o)
			st := v.uploadStatus("PREPARE", name, str(m["subtask_name"]))["print"].(object)
			for _, key := range []string{"gcode_state", "subtask_name", "gcode_file", "gcode_file_prepare_percent"} {
				reply[key] = st[key]
			}
			reply["param"] = m["param"]
			reply["result"] = "SUCCESS"
			reply["msg"] = 0
		case "get_accessories":
			if v.c.Target != 0 {
				return v.forward(section, m, reply)
			}
			reply["nozzle_diameter"] = "0.4"
		default:
			if v.c.Target != 0 {
				return v.forward(section, m, reply)
			}
			reply["result"] = "fail"
			reply["reason"] = "unsupported command on intake printer"
		}
		return object{section: reply}
	}
	return nil
}
func (v *instance) observeIntake(parent context.Context, name, subtask string, push func(object) error) context.CancelFunc {
	ctx, cancel := context.WithCancel(parent)
	go func() { defer cancel(); v.awaitIntake(ctx, name, subtask, push) }()
	return cancel
}
func (v *instance) mqtt(conn net.Conn) {
	var writeMu sync.Mutex
	send := func(k byte, p []byte) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		return writePacket(conn, k, p)
	}
	ctx, cancel := context.WithCancel(v.ctx)
	defer cancel()
	var stopObserve context.CancelFunc
	reader := bufio.NewReader(conn)
	kind, p, e := readPacket(reader)
	if e != nil || kind>>4 != 1 {
		return
	}
	if !v.authenticateConnect(p) {
		send(0x20, []byte{0, 4})
		return
	}
	if send(0x20, []byte{0, 0}) != nil {
		return
	}
	report := "device/" + v.c.Serial + "/report"
	request := "device/" + v.c.Serial + "/request"
	subscribed := false
	var topicMu sync.RWMutex
	push := func(m object) error {
		b, _ := json.Marshal(m)
		topicMu.RLock()
		topic := report
		topicMu.RUnlock()
		return send(0x30, append(mqttString(topic), b...))
	}
	var pulses sync.Once
	for {
		conn.SetReadDeadline(time.Now().Add(90 * time.Second))
		k, data, e := readPacket(reader)
		if e != nil {
			return
		}
		switch k >> 4 {
		case 8:
			if len(data) < 2 {
				return
			}
			ack := append([]byte{}, data[:2]...)
			data = data[2:]
			for len(data) > 0 {
				topic, ok := readStr(&data)
				if !ok || len(data) < 1 {
					return
				}
				data = data[1:]
				parts := strings.Split(topic, "/")
				if len(parts) == 3 && parts[0] == "device" && topicSerial.MatchString(parts[1]) && (parts[2] == "report" || parts[2] == "#") {
					topicMu.Lock()
					report = "device/" + parts[1] + "/report"
					request = "device/" + parts[1] + "/request"
					topicMu.Unlock()
					subscribed = true
					ack = append(ack, 0)
				} else {
					ack = append(ack, 0x80)
				}
			}
			if send(0x90, ack) != nil {
				return
			}
			if subscribed {
				if push(v.report()) != nil {
					return
				}
				pulses.Do(func() {
					go func() {
						ticker := time.NewTicker(5 * time.Second)
						defer ticker.Stop()
						for {
							select {
							case <-ctx.Done():
								return
							case <-ticker.C:
								if push(v.report()) != nil {
									return
								}
							}
						}
					}()
				})
			}
		case 3:
			topic, ok := readStr(&data)
			if !ok {
				return
			}
			qos := (k >> 1) & 3
			if qos > 1 {
				return
			}
			if qos == 1 {
				if len(data) < 2 {
					return
				}
				if send(0x40, data[:2]) != nil {
					return
				}
				data = data[2:]
			}
			if topic != request {
				continue
			}
			var cmd object
			if json.Unmarshal(bytes.TrimRight(data, "\x00"), &cmd) == nil {
				if res := v.response(cmd); res != nil && subscribed {
					if push(res) != nil {
						return
					}
					p, _ := cmd["print"].(map[string]any)
					reply, _ := res["print"].(map[string]any)
					if (p["command"] == "project_file" || p["command"] == "gcode_file") && reply["result"] == "SUCCESS" {
						if stopObserve != nil {
							stopObserve()
						}
						stopObserve = v.observeIntake(ctx, projectName(p), str(p["subtask_name"]), push)
					}
				}
			}
		case 12:
			if send(0xd0, nil) != nil {
				return
			}
		case 14:
			return
		default:
			return
		}
	}
}
