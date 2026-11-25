package production

import (
	"context"
	"testing"
)

type bridgeDevice struct {
	mockHardware
	wire      slicerWire
	published chan row
}

func (h *bridgeDevice) slicerLink() *slicerWire { return &h.wire }
func (h *bridgeDevice) Publish(ctx context.Context, m row) error {
	select {
	case h.published <- m:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestSlicerTargetCorrelationAndAMSDelta(t *testing.T) {
	// Two slicers use the same application sequence. Responses can arrive out of order.
	device := &bridgeDevice{published: make(chan row, 2)}
	d := &liveDevice{h: device, printer: row{"serial_number": "physical"}, report: row{}}
	s := &Service{devices: map[int64]*liveDevice{17: d}}
	request := map[string]any{"sequence_id": "5", "command": "extrusion_cali_get"}
	got := make(chan map[string]any, 2)
	call := func() {
		reply, err := s.SlicerCommand(t.Context(), 17, "fixture", "print", request)
		if err != nil {
			t.Error(err)
		}
		got <- reply
	}
	go call()
	first := (<-device.published)["print"].(row)
	go call()
	second := (<-device.published)["print"].(row)
	if first["sequence_id"] == second["sequence_id"] {
		t.Fatal("Shared hardware sequence")
	}
	send := func(msg row) {
		device.wire.receive(map[string]any{"print": map[string]any{"sequence_id": msg["sequence_id"], "command": msg["command"], "src": msg["sequence_id"]}})
	}
	send(second)
	send(first)
	results := map[string]bool{}
	for range 2 {
		r := <-got
		if r["sequence_id"] != "5" {
			t.Fatal(r)
		}
		results[val(r["src"])] = true
	}
	if len(results) != 2 {
		t.Fatal(results)
	}
	if _, e := s.SlicerCommand(t.Context(), 17, "fixture", "print", map[string]any{"command": "project_file"}); e == nil {
		t.Fatal("Physical start bypass")
	}
	if _, e := s.SlicerCommand(t.Context(), 17, "stale", "print", request); e == nil {
		t.Fatal("Unpinned printer command")
	}
	device.wire.receive(map[string]any{"info": map[string]any{"command": "get_version", "module": []any{map[string]any{"name": "ams"}}}})
	ams := func(m map[string]any) { merge(d.report, row{"ams": m}) }
	ams(map[string]any{"ams_exist_bits": "3", "tray_exist_bits": "13", "ams": []any{map[string]any{"id": "0", "tray": []any{map[string]any{"id": "0", "tray_type": "PLA"}, map[string]any{"id": "1", "tray_type": "PETG"}}}, map[string]any{"id": "1", "tray": []any{map[string]any{"id": "0", "tray_type": "ABS"}}}}})
	ams(map[string]any{"humidity": "2", "ams": []any{map[string]any{"id": "0", "tray": []any{map[string]any{"id": "1", "cali_idx": "4"}}}}})
	ams(map[string]any{"tray_exist_bits": "12"})
	r, v, serial := s.SlicerStatus(17, "fixture")
	units := r["ams"].(map[string]any)["ams"].([]any)
	trays := units[0].(map[string]any)["tray"].([]any)
	if len(units) != 2 || len(trays) != 2 || trays[0].(map[string]any)["tray_type"] != "" || trays[1].(map[string]any)["tray_type"] != "PETG" || trays[1].(map[string]any)["cali_idx"] != "4" || serial != "physical" || v["command"] != "get_version" {
		t.Fatal(r, v, serial)
	}
	ams(map[string]any{"ams_exist_bits": "1", "ams": []any{map[string]any{"id": "0"}}})
	r, _, _ = s.SlicerStatus(17, "fixture")
	if len(r["ams"].(map[string]any)["ams"].([]any)) != 1 {
		t.Fatal("Removed unit retained")
	}
}
