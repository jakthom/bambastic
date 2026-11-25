package production

import (
	"context"
	"strings"
	"testing"

	"bambastic/internal/access"
)

type calibrationHardware struct {
	*mockHardware
	reply func(row)
}

func (h calibrationHardware) Publish(ctx context.Context, p row) error {
	packet := p["print"].(row)
	h.reply(packet)
	return nil
}
func TestKProfilesMatchCurrentSlotAndRejectRefusal(t *testing.T) {
	f := farmFixture(t)
	d := f.s.devices[1]
	sets := 0
	d.h = calibrationHardware{f.hw, func(p row) {
		cmd := val(p["command"])
		seq := val(p["sequence_id"])
		noz := val(p["nozzle_diameter"])
		if cmd == "extrusion_cali_get" {
			d.calibration.receive(row{"command": cmd, "nozzle_diameter": "0.6", "sequence_id": seq, "filaments": []any{}})
			d.calibration.receive(row{"command": cmd, "nozzle_diameter": noz, "sequence_id": "wrong", "filaments": []any{}})
			d.calibration.receive(row{"command": cmd, "nozzle_diameter": noz, "sequence_id": seq, "filaments": []any{map[string]any{"cali_idx": 17, "filament_id": "GFL99", "name": "Default", "setting_id": "new", "extruder_id": 1, "k_value": "0.02", "nozzle_id": "HH00-0.4"}, map[string]any{"cali_idx": 32, "filament_id": "GFL99", "name": "Default", "setting_id": "other", "extruder_id": 0}}})
		} else {
			sets++
			items := p["filaments"].([]row)
			if len(items) != 2 || num(items[0]["cali_idx"]) != 17 || items[0]["nozzle_id"] != "HH00-0.4" || items[0]["setting_id"] != "new" || num(items[1]["cali_idx"]) != -1 {
				t.Fatalf("wrong firmware slot %+v", items)
			}
			d.calibration.receive(row{"command": cmd, "sequence_id": seq, "result": "fail", "reason": "busy"})
		}
	}}
	if rs, e := f.s.KProfiles(context.Background(), 1, "0.4"); e != nil || len(rs) != 2 || rs[0]["nozzle_diameter"] != "0.4" {
		t.Fatal(rs, e)
	}
	_, e := f.s.RestoreKProfiles(context.Background(), 1, "0.4", []map[string]any{{"slot_id": 98, "extruder_id": 1, "filament_id": "GFL99", "name": "Default", "k_value": "0.03"}, {"slot_id": 99, "extruder_id": 1, "filament_id": "GFL99", "name": "Other", "k_value": "0.05"}})
	if e == nil || !strings.Contains(e.Error(), "busy") || sets != 1 {
		t.Fatal(e)
	}
	f.req("PUT", "/printers/1/kprofiles/notes", row{"setting_id": "PF", "note": "0.4 mm PETG"}, f.admin, "", 200)
	if got := string(f.req("GET", "/printers/1/kprofiles/notes", nil, f.admin, "", 200)); !strings.Contains(got, "0.4 mm PETG") {
		t.Fatal(got)
	}
	f.req("GET", "/printers/1/kprofiles/", nil, access.Principal{Admin: true, PrinterIDs: map[int64]bool{2: true}}, "", 404)
}
