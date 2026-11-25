package operations

import (
	"context"
	"errors"
	"strings"
)

// PowerOffForDetection handles only the operator-designated printer power
// outlets. A fresh guard is required before each physical command.
func (s *Service) PowerOffForDetection(ctx context.Context, id int64, current func() bool) error {
	if id <= 0 || current == nil || !current() {
		return errors.New("Print or detection settings changed")
	}
	targets, e := s.list(ctx, "SELECT id FROM smart_plugs WHERE printer_id=? AND enabled AND controls_printer_power ORDER BY id", id)
	if e != nil {
		return e
	}
	switched := 0
	for _, target := range targets {
		p, e := s.get(ctx, plugTable, target["id"])
		if e != nil {
			return e
		}
		if int64(num(p["printer_id"])) != id || !flag(p["enabled"]) || !flag(p["controls_printer_power"]) {
			continue
		}
		switch str(p["plug_type"]) {
		case "tasmota":
		case "homeassistant":
			domain, _, _ := strings.Cut(str(p["ha_entity_id"]), ".")
			switch domain {
			case "switch", "light", "input_boolean":
			default:
				continue
			}
		case "rest":
			if str(p["rest_off_url"]) == "" {
				continue
			}
		default:
			continue
		}
		if !current() {
			return errors.New("Print or detection settings changed")
		}
		if e = s.control(ctx, p, "off"); e != nil {
			return e
		}
		switched++
	}
	if switched == 0 {
		return errors.New("No enabled, switchable printer power outlet is assigned")
	}
	return nil
}
