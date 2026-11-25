package production

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type calibrationLink struct {
	operation sync.Mutex // serialize each read/match/write on one printer
	mu        sync.Mutex
	pending   *profileRequest
	tables    map[string][]any
}
type profileRequest struct {
	cmd, seq, nozzle string
	reply            chan row
}

func (l *calibrationLink) receive(m row) {
	cmd := val(m["command"])
	if !strings.HasPrefix(cmd, "extrusion_cali_") {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	diam := val(m["nozzle_diameter"])
	if nozzleOK(diam) {
		if cmd == "extrusion_cali_get" {
			if items, ok := m["filaments"].([]any); ok && len(items) <= 4096 {
				if l.tables == nil {
					l.tables = map[string][]any{}
				}
				if _, known := l.tables[diam]; known || len(l.tables) < 32 {
					l.tables[diam] = items
				}
			}
		} else if cmd == "extrusion_cali_set" || cmd == "extrusion_cali_del" {
			delete(l.tables, diam)
		}
	}
	p := l.pending
	if p == nil || p.cmd != cmd {
		return
	}
	seq := val(m["sequence_id"])
	noz := val(m["nozzle_diameter"])
	if noz != "" && noz != p.nozzle {
		return
	}
	if seq != p.seq && (cmd != "extrusion_cali_get" || noz != p.nozzle || (seq != "" && seq != "0" && seq != "-1")) {
		return
	}
	select {
	case p.reply <- m:
	default:
	}
}

type calibrationSession struct {
	link   *calibrationLink
	device hardware
	dual   bool
}

func (s *Service) calibrationSession(ctx context.Context, id int64) (calibrationSession, func(), error) {
	s.mu.RLock()
	d := s.devices[id]
	s.mu.RUnlock()
	if d == nil {
		return calibrationSession{}, nil, fail(409, "Printer is disconnected")
	}
	d.calibration.operation.Lock()
	s.mu.RLock()
	h := d.h
	valid := s.devices[id] == d && h != nil && h.Online()
	two := dual(d.printer)
	if obj, ok := d.report["device"].(map[string]any); ok {
		if ext, ok := obj["extruder"].(map[string]any); ok {
			if info, ok := ext["info"].([]any); ok && len(info) > 1 {
				two = true
			}
		}
	}
	s.mu.RUnlock()
	if e := ctx.Err(); e != nil {
		d.calibration.operation.Unlock()
		return calibrationSession{}, nil, e
	}
	if !valid {
		d.calibration.operation.Unlock()
		return calibrationSession{}, nil, fail(409, "Printer is disconnected")
	}
	return calibrationSession{&d.calibration, h, two}, d.calibration.operation.Unlock, nil
}
func (c calibrationSession) command(ctx context.Context, cmd, diameter string, fields row) (row, bool, error) {
	fields["nozzle_diameter"] = diameter
	msg := packet("print", cmd, fields)
	wait := &profileRequest{cmd: cmd, seq: val(fields["sequence_id"]), nozzle: diameter, reply: make(chan row, 1)}
	c.link.mu.Lock()
	c.link.pending = wait
	c.link.mu.Unlock()
	defer func() { c.link.mu.Lock(); c.link.pending = nil; c.link.mu.Unlock() }()
	timer, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := c.device.Publish(timer, msg); err != nil {
		return nil, false, fail(502, "Printer could not receive calibration command")
	}
	select {
	case reply := <-wait.reply:
		result := strings.ToLower(val(reply["result"]))
		if result == "fail" || result == "failed" || result == "error" {
			reason := val(reply["reason"])
			if len(reason) > 256 {
				reason = reason[:256]
			}
			return nil, false, fail(502, "Printer rejected calibration command: "+reason)
		}
		return reply, true, nil
	case <-timer.Done():
		if ctx.Err() != nil {
			return nil, false, ctx.Err()
		}
		if cmd == "extrusion_cali_get" {
			return nil, false, fail(504, "Printer did not answer the calibration read")
		}
		return nil, false, nil
	}
}
func nozzleOK(s string) bool {
	v, e := strconv.ParseFloat(s, 64)
	return e == nil && v >= 0.1 && v <= 2 && s == strconv.FormatFloat(v, 'f', -1, 64)
}
func (c calibrationSession) read(ctx context.Context, diameter string) ([]map[string]any, error) {
	if !nozzleOK(diameter) {
		return nil, fail(422, "Invalid nozzle diameter")
	}
	m, _, err := c.command(ctx, "extrusion_cali_get", diameter, row{"filament_id": ""})
	if err != nil {
		return nil, err
	}
	items, ok := m["filaments"].([]any)
	if !ok {
		return nil, fail(502, "Printer did not supply the calibration table")
	}
	out := []map[string]any{}
	for n, item := range items {
		f, ok := item.(map[string]any)
		if !ok {
			return nil, fail(502, "Invalid printer calibration record")
		}
		v := map[string]any{"slot_id": int64(n), "extruder_id": 0, "nozzle_id": "", "nozzle_diameter": diameter, "filament_id": "", "name": "", "k_value": "0.000000", "n_coef": "0.000000", "ams_id": 0, "tray_id": -1, "setting_id": nil}
		for _, k := range []string{"extruder_id", "ams_id", "tray_id"} {
			if f[k] != nil {
				v[k] = num(f[k])
			}
		}
		for _, k := range []string{"setting_id", "filament_id", "name", "nozzle_id", "nozzle_diameter", "k_value", "n_coef"} {
			if s := val(f[k]); s != "" {
				v[k] = s
			}
		}
		if f["cali_idx"] != nil {
			v["slot_id"] = num(f["cali_idx"])
		}
		out = append(out, v)
	}
	return out, nil
}
func (s *Service) KProfiles(ctx context.Context, id int64, diameter string) ([]map[string]any, error) {
	c, done, e := s.calibrationSession(ctx, id)
	if e != nil {
		return nil, e
	}
	defer done()
	return c.read(ctx, diameter)
}
func settingID() (string, error) {
	n, e := rand.Int(rand.Reader, big.NewInt(90000000000000000))
	if e != nil {
		return "", e
	}
	return fmt.Sprintf("PF%d", n.Int64()+10000000000000000), nil
}
func caliFields(source map[string]any, diameter string, idx int64) (row, error) {
	name := val(source["name"])
	filament := val(source["filament_id"])
	if !nozzleOK(diameter) || name == "" || len(name) > 200 || filament == "" || len(filament) > 100 || idx < -1 || idx > 65535 {
		return nil, fail(422, "Invalid calibration identity")
	}
	value, err := strconv.ParseFloat(val(source["k_value"]), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 2 {
		return nil, fail(422, "Invalid pressure advance value")
	}
	ext := num(source["extruder_id"])
	if ext < 0 || ext > 7 {
		return nil, fail(422, "Invalid extruder")
	}
	nozzle := val(source["nozzle_id"])
	if nozzle == "" {
		nozzle = "HS00-" + diameter
	}
	setting := val(source["setting_id"])
	if setting == "" && idx < 0 {
		if setting, err = settingID(); err != nil {
			return nil, err
		}
	}
	coef := val(source["n_coef"])
	if coef == "" {
		coef = "0.000000"
	}
	v, e := strconv.ParseFloat(coef, 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fail(422, "Invalid calibration coefficient")
	}
	return row{"ams_id": 0, "tray_id": 0, "cali_idx": idx, "extruder_id": ext, "filament_id": filament, "k_value": val(source["k_value"]), "name": name, "nozzle_id": nozzle, "nozzle_diameter": diameter, "n_coef": coef, "setting_id": setting}, nil
}
func (c calibrationSession) set(ctx context.Context, diameter string, entries []row) (bool, error) {
	_, ok, e := c.command(ctx, "extrusion_cali_set", diameter, row{"filaments": entries})
	return ok, e
}
func (c calibrationSession) remove(ctx context.Context, diameter string, v map[string]any) (bool, error) {
	if !nozzleOK(diameter) || num(v["slot_id"]) < 0 || v["slot_id"] == nil || val(v["filament_id"]) == "" {
		return false, fail(422, "Invalid calibration identity")
	}
	fields := row{"cali_idx": num(v["slot_id"]), "extruder_id": num(v["extruder_id"]), "filament_id": val(v["filament_id"]), "nozzle_id": val(v["nozzle_id"])}
	if !c.dual {
		fields["setting_id"] = val(v["setting_id"])
	}
	_, ok, e := c.command(ctx, "extrusion_cali_del", diameter, fields)
	return ok, e
}
func matchCalibration(v map[string]any, current []map[string]any, used map[int64]bool) map[string]any {
	candidates := []map[string]any{}
	for _, x := range current {
		if x["filament_id"] == v["filament_id"] && (v["extruder_id"] == nil || num(v["extruder_id"]) == num(x["extruder_id"])) {
			candidates = append(candidates, x)
		}
	}
	for _, key := range []string{"setting_id", "name"} {
		for _, x := range candidates {
			if !used[num(x["slot_id"])] && val(v[key]) != "" && v[key] == x[key] {
				return x
			}
		}
	}
	if len(candidates) == 1 && !used[num(candidates[0]["slot_id"])] {
		return candidates[0]
	}
	return nil
}

// RestoreKProfiles resolves stable preset identities against the printer's current
// slot numbers while holding the same per-printer lock used by manual editing.
func (s *Service) RestoreKProfiles(ctx context.Context, id int64, diameter string, profiles []map[string]any) (bool, error) {
	if len(profiles) == 0 || len(profiles) > 4096 {
		return false, fail(422, "Invalid calibration count")
	}
	c, done, e := s.calibrationSession(ctx, id)
	if e != nil {
		return false, e
	}
	defer done()
	current, e := c.read(ctx, diameter)
	if e != nil {
		return false, e
	}
	used := map[int64]bool{}
	items := []row{}
	for _, v := range profiles {
		fields := map[string]any{}
		for k, x := range v {
			fields[k] = x
		}
		idx := int64(-1)
		if m := matchCalibration(fields, current, used); m != nil {
			idx = num(m["slot_id"])
			used[idx] = true
			for _, k := range []string{"setting_id", "nozzle_id"} {
				if val(m[k]) != "" {
					fields[k] = m[k]
				}
			}
		}
		x, e := caliFields(fields, diameter, idx)
		if e != nil {
			return false, e
		}
		items = append(items, x)
	}
	return c.set(ctx, diameter, items)
}
func (s *Service) calibrationRoutes(rt register) {
	const base = "/printers/{id}/kprofiles"
	for _, m := range []string{"GET", "POST", "DELETE"} {
		rt(m, base+"/{$}", s.kprofile)
	}
	rt("POST", base+"/batch", s.kprofile)
	rt("GET", base+"/notes", s.kprofileNotes)
	rt("PUT", base+"/notes", s.kprofileNotes)
	rt("DELETE", base+"/notes/{setting}", s.kprofileNotes)
}
func (s *Service) kprofile(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := map[string]string{"GET": "kprofiles:read", "POST": "kprofiles:update", "DELETE": "kprofiles:delete"}[r.Method]
	pr, e := s.printerRow(r, perm)
	if e != nil {
		return nil, e
	}
	id := num(pr["id"])
	ctx := r.Context()
	if r.Method == "GET" {
		diameter := r.URL.Query().Get("nozzle_diameter")
		if diameter == "" {
			diameter = "0.4"
		}
		rs, e := s.KProfiles(ctx, id, diameter)
		return row{"profiles": rs, "nozzle_diameter": diameter}, e
	}
	var vs []map[string]any
	batch := strings.HasSuffix(r.URL.Path, "/batch")
	if batch {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
		dec.UseNumber()
		if dec.Decode(&vs) != nil || dec.Decode(new(any)) != io.EOF || len(vs) == 0 || len(vs) > 4096 {
			return nil, fail(422, "Expected a nonempty list of calibration profiles")
		}
	} else {
		x, e := input(w, r)
		if e != nil {
			return nil, e
		}
		vs = []map[string]any{x}
	}
	diameter := val(vs[0]["nozzle_diameter"])
	c, done, e := s.calibrationSession(ctx, id)
	if e != nil {
		return nil, e
	}
	defer done()
	if r.Method == "DELETE" {
		ok, e := c.remove(ctx, diameter, vs[0])
		return row{"success": true, "confirmed": ok, "message": "Calibration delete submitted"}, e
	}
	items := []row{}
	for _, v := range vs {
		if val(v["nozzle_diameter"]) != diameter {
			return nil, fail(422, "All profiles must use the same nozzle diameter")
		}
		idx := num(v["slot_id"])
		if idx == 0 {
			idx = -1
		}
		x, e := caliFields(v, diameter, idx)
		if e != nil {
			return nil, e
		}
		items = append(items, x)
	}
	edit := !batch && num(vs[0]["slot_id"]) > 0 && !c.dual
	if edit {
		ok, e := c.remove(ctx, diameter, vs[0])
		if e != nil {
			return nil, e
		}
		if !ok {
			return nil, fail(504, "Original calibration deletion was unconfirmed; refresh the printer table")
		}
		items[0]["cali_idx"] = -1
		items[0]["setting_id"], e = settingID()
		if e != nil {
			return nil, e
		}
	}
	confirmed, e := c.set(ctx, diameter, items)
	if e != nil {
		return nil, e
	}
	if edit {
		table, e := c.read(ctx, diameter)
		if e == nil {
			found := false
			for _, v := range table {
				sv, _ := strconv.ParseFloat(val(v["k_value"]), 64)
				expected, _ := strconv.ParseFloat(val(items[0]["k_value"]), 64)
				if v["name"] == items[0]["name"] && v["filament_id"] == items[0]["filament_id"] && num(v["extruder_id"]) == num(items[0]["extruder_id"]) && math.Abs(sv-expected) < 1e-6 {
					found = true
					break
				}
			}
			if !found {
				return nil, fail(502, "Edited calibration is absent from the printer table")
			}
			confirmed = true
		}
	}
	return row{"success": true, "confirmed": confirmed, "message": "Calibration write submitted"}, nil
}
func (s *Service) kprofileNotes(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := map[string]string{"GET": "kprofiles:read", "PUT": "kprofiles:update", "DELETE": "kprofiles:delete"}[r.Method]
	pr, e := s.printerRow(r, perm)
	if e != nil {
		return nil, e
	}
	id := num(pr["id"])
	ctx := r.Context()
	if r.Method == "GET" {
		rs, e := records(ctx, s.db, "SELECT setting_id,note FROM kprofile_notes WHERE printer_id=?", id)
		m := map[string]string{}
		for _, x := range rs {
			m[val(x["setting_id"])] = val(x["note"])
		}
		return row{"notes": m}, e
	}
	setting := r.PathValue("setting")
	note := ""
	if r.Method == "PUT" {
		v, e := input(w, r)
		if e != nil {
			return nil, e
		}
		setting = val(v["setting_id"])
		note = val(v["note"])
	}
	if setting == "" || len(setting) > 100 {
		return nil, fail(422, "Invalid setting ID")
	}
	if strings.TrimSpace(note) == "" {
		_, e = s.db.ExecContext(ctx, "DELETE FROM kprofile_notes WHERE printer_id=? AND setting_id=?", id, setting)
	} else {
		_, e = s.db.ExecContext(ctx, "INSERT INTO kprofile_notes(printer_id,setting_id,note) VALUES(?,?,?) ON CONFLICT(printer_id,setting_id) DO UPDATE SET note=excluded.note,updated_at=CURRENT_TIMESTAMP", id, setting, note)
	}
	return row{"success": true, "message": "Calibration note saved"}, e
}

func (s *Service) CalibrationConnected(id int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[id]
	return d != nil && d.h != nil && d.h.Online()
}

// MaterialSlots returns a consistent copy of the connected AMS report for inventory.
// Copying here avoids holding the device lock during a remote Spoolman request.
func (s *Service) MaterialSlots(id int64) (map[string]any, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	d := s.devices[id]
	if d == nil || d.h == nil || !d.h.Online() {
		return nil, false
	}
	b, e := json.Marshal(row{"power_on_flag": d.report["power_on_flag"], "ams": d.report["ams"], "vt_tray": d.report["vt_tray"], "vt_tray_list": d.report["vt_tray_list"], "gcode_state": d.report["gcode_state"]})
	if e != nil {
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal(b, &v) != nil {
		return nil, false
	}
	return v, true
}
