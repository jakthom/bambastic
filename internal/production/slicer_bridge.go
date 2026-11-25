package production

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

// Correlate commands from multiple slicers on the enrolled connection. Two
// clients can use identical sequence numbers, so allocate a printer sequence
// for each request and restore the original before returning its reply.
type slicerWire struct {
	mu      sync.Mutex
	version map[string]any
	pending map[string]slicerRequest
}
type slicerRequest struct {
	section, command string
	reply            chan map[string]any
}
type slicerTransport interface {
	hardware
	slicerLink() *slicerWire
}

func (l *lan) slicerLink() *slicerWire { return &l.wire }
func cloneWire(o map[string]any) map[string]any {
	if o == nil {
		return nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return nil
	}
	var out map[string]any
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out
}
func (w *slicerWire) receive(o map[string]any) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for section, val0 := range o {
		m, ok := val0.(map[string]any)
		if !ok {
			continue
		}
		cmd := val(m["command"])
		if section == "info" && cmd == "get_version" && m["module"] != nil {
			w.version = m
		}
		req, ok := w.pending[val(m["sequence_id"])]
		if !ok || req.section != section || req.command != cmd {
			continue
		}
		select {
		case req.reply <- m:
		default:
		}
	}
}

func (s *Service) slicerDevice(id int64, pin string) (*liveDevice, slicerTransport) {
	d := s.devices[id]
	if d == nil || pin == "" {
		return nil, nil
	}
	h, ok := d.h.(slicerTransport)
	if !ok || !h.Online() || h.Fingerprint() != pin {
		return nil, nil
	}
	return d, h
}

// SlicerStatus returns a detached copy so virtual identity rewriting never
// modifies fleet state. The certificate pin belongs to the virtual target.
func (s *Service) SlicerStatus(id int64, pin string) (status, version map[string]any, serial string) {
	s.mu.RLock()
	d, h := s.slicerDevice(id, pin)
	if h == nil {
		s.mu.RUnlock()
		return
	}
	status, serial = cloneWire(d.report), val(d.printer["serial_number"])
	s.mu.RUnlock()
	w := h.slicerLink()
	w.mu.Lock()
	version = cloneWire(w.version)
	w.mu.Unlock()
	return
}

func permittedSlicerCommand(section, cmd string) bool {
	switch section {
	case "print":
		return in(cmd, "get_accessories", "get_print_option", "print_option", "get_active_nozzle", "extrusion_cali_get", "extrusion_cali_sel", "extrusion_cali_set", "extrusion_cali_del", "ams_filament_setting", "ams_change_filament", "ams_user_setting", "ams_control", "ams_drying", "ams_dry", "set_airduct", "set_chamber_temp", "set_accessories", "set_nozzle", "set_filament_setting", "print_speed", "pause", "resume", "stop")
	case "xcam":
		return in(cmd, "xcam_control_set", "xcam_control_get", "spaghetti_detector", "first_layer_inspector", "printing_monitor", "buildplate_marker_detector")
	case "system":
		return in(cmd, "ledctrl", "get_accessories", "set_accessories", "get_camera_setting", "set_camera_setting", "get_stage")
	case "camera":
		return in(cmd, "ipcam_record_set", "ipcam_timelapse", "ipcam_resolution_set", "ipcam_rtsp_set", "ipcam_control")
	case "info":
		return in(cmd, "get_accessories", "get_board_info", "get_active_nozzle")
	}
	return false
}

func (s *Service) SlicerCommand(ctx context.Context, id int64, pin, section string, m map[string]any) (map[string]any, error) {
	cmd := val(m["command"])
	if !permittedSlicerCommand(section, cmd) {
		return nil, errors.New("Unsupported command on intake printer")
	}
	s.mu.RLock()
	_, h := s.slicerDevice(id, pin)
	s.mu.RUnlock()
	if h == nil {
		return nil, errors.New("Target printer is disconnected")
	}
	fields := row(cloneWire(m))
	original := fields["sequence_id"]
	msg := packet(section, cmd, fields)
	seq := val(fields["sequence_id"])
	wait := slicerRequest{section, cmd, make(chan map[string]any, 1)}
	w := h.slicerLink()
	w.mu.Lock()
	if w.pending == nil {
		w.pending = map[string]slicerRequest{}
	}
	if len(w.pending) >= 32 {
		w.mu.Unlock()
		return nil, errors.New("Target printer is busy")
	}
	w.pending[seq] = wait
	w.mu.Unlock()
	defer func() { w.mu.Lock(); delete(w.pending, seq); w.mu.Unlock() }()
	bounded, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	if err := h.Publish(bounded, msg); err != nil {
		return nil, errors.New("Target printer could not receive the command")
	}
	select {
	case <-bounded.Done():
		return nil, errors.New("Target printer did not acknowledge the command")
	case response := <-wait.reply:
		out := cloneWire(response)
		out["sequence_id"] = original
		return out, nil
	}
}
