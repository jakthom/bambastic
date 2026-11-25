package management

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"math"
	"strconv"
	"strings"
	"unicode"

	"bambastic/internal/access"
	"bambastic/internal/opentag"
)

func tagText(b []byte) string {
	b, _, _ = bytes.Cut(b, []byte{0})
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(string(b), "")))
}

func openTagPrefill(raw string) (row, error) {
	if raw == "" {
		return nil, nil
	}
	if len(raw) > 510 {
		return nil, invalid("OpenTag3D record is too long")
	}
	data, e := hex.DecodeString(raw)
	if e != nil {
		return nil, invalid("Invalid OpenTag3D record")
	}
	p := opentag.Payload(data)
	if len(p) < 2 {
		return nil, invalid("Invalid OpenTag3D record")
	}
	version := binary.BigEndian.Uint16(p)
	var brand, color, rgba, diameter, weight int
	d := row{"core_weight": 0}
	switch {
	case version == 1000 && len(p) == 102:
		brand, color, rgba, diameter, weight = 27, 43, 75, 92, 94
		if p[96] != 0 {
			d["nozzle_temp_min"] = int(p[96]) * 5
		}
	case version == 2004 && len(p) == 162:
		brand, color, rgba, diameter, weight = 12, 28, 60, 140, 158
		if p[145] != 0 {
			d["nozzle_temp_min"] = int(p[145]) * 5
		}
		if p[146] != 0 {
			d["nozzle_temp_max"] = int(p[146]) * 5
		}
		d["core_weight"] = int(binary.BigEndian.Uint16(p[160:]))
		var colors []string
		for i := 64; i < 76; i += 4 {
			if !bytes.Equal(p[i:i+4], []byte{0, 0, 0, 0}) {
				colors = append(colors, strings.ToUpper(hex.EncodeToString(p[i:i+4])))
			}
		}
		if len(colors) > 0 {
			d["extra_colors"] = strings.Join(colors, ",")
		}
	default:
		return nil, nil
	}
	material := tagText(p[2:7])
	if material == "" {
		return nil, nil
	}
	d["material"], d["subtype"] = material, tagText(p[7:12])
	d["brand"], d["color_name"] = tagText(p[brand:brand+16]), tagText(p[color:color+32])
	d["rgba"] = strings.ToUpper(hex.EncodeToString(p[rgba : rgba+4]))
	d["label_weight"] = int(binary.BigEndian.Uint16(p[weight:]))
	if mm := float64(binary.BigEndian.Uint16(p[diameter:])) / 1000; mm > 0 && mm < 5 {
		d["diameter"] = mm
	}
	return d, nil
}

// Convert an authorized reader's spool blocks to the same fields used for AMS tray reports.
func scannedMaterial(tx *sql.Tx, raw any, ndef string) (row, string, error) {
	if raw != nil && ndef != "" {
		return nil, "", invalid("Select one spool tag record format")
	}
	d, e := openTagPrefill(ndef)
	if e != nil || d != nil {
		return d, "", e
	}
	if raw == nil {
		return nil, "", nil
	}
	encoded, ok := raw.(map[string]any)
	if !ok || len(encoded) > 16 {
		return nil, "", invalid("Invalid tag data blocks")
	}
	blocks := map[int][]byte{}
	for key, value := range encoded {
		n, e := strconv.Atoi(key)
		v, ok := value.(string)
		if e != nil || n < 0 || n > 63 || !ok || len(v) != 32 {
			return nil, "", invalid("Invalid tag data block")
		}
		b, e := hex.DecodeString(v)
		if e != nil {
			return nil, "", invalid("Invalid tag data block")
		}
		blocks[n] = b
	}
	uuid := ""
	if b := blocks[9]; b != nil && !bytes.Equal(b, make([]byte, 16)) {
		uuid = strings.ToUpper(hex.EncodeToString(b))
	}
	for _, id := range []int{1, 2, 4, 5} {
		if blocks[id] == nil {
			return nil, uuid, nil
		}
	}
	material := tagText(blocks[2])
	if material == "" {
		return nil, uuid, nil
	}
	b1, b5 := blocks[1], blocks[5]
	tray := row{"tray_type": material, "tray_sub_brands": tagText(blocks[4]), "tray_color": strings.ToUpper(hex.EncodeToString(b5[:4])), "tray_id_name": tagText(b1[:8]), "tray_info_idx": tagText(b1[8:]), "tray_weight": int(binary.LittleEndian.Uint16(b5[4:]))}
	if b := blocks[6]; b != nil {
		for name, offset := range map[string]int{"drying_temp": 0, "drying_time": 2, "nozzle_temp_max": 8, "nozzle_temp_min": 10} {
			if n := binary.LittleEndian.Uint16(b[offset:]); n > 0 {
				tray[name] = int(n)
			}
		}
	}
	d, e = materialFromTray(tx, tray)
	if e != nil {
		return nil, "", e
	}
	d["variant_id"] = tray["tray_id_name"]
	for _, k := range []string{"drying_time", "drying_temp", "nozzle_temp_min", "nozzle_temp_max"} {
		if v := tray[k]; v != nil {
			d[k] = v
		}
	}
	diameter := float64(math.Float32frombits(binary.LittleEndian.Uint32(b5[8:])))
	if diameter > 0 && diameter < 5 {
		d["diameter"] = math.Round(diameter*100) / 100
	}
	return d, uuid, nil
}

// An enrolled printer may have already identified this roll. Match its actual
// tag within the caller's printer scope before prefilling an unknown station scan.
func (s *Service) materialOnPrinter(r request, d row) (row, string, error) {
	tag, uuid := cleanTag(d["tag_uid"], 16), cleanTag(d["tray_uuid"], 32)
	if tag == nil && uuid == nil {
		return nil, "", nil
	}
	printers, e := list(r.tx, "SELECT id FROM printers WHERE is_active")
	if e != nil {
		return nil, "", e
	}
	var match row
	actor := access.From(r.Context())
	for _, printer := range printers {
		id := intID(printer["id"])
		if !actor.AllowsPrinter(id) {
			continue
		}
		report, online := s.MaterialSlots(id)
		if !online {
			continue
		}
		trays, _, _ := reportSlots(id, report)
		for _, tray := range trays {
			sameUID := tag != nil && cleanTag(tray["tag_uid"], 16) == tag
			sameUUID := uuid != nil && cleanTag(tray["tray_uuid"], 32) == uuid
			if !sameUID && !sameUUID {
				continue
			}
			if match != nil {
				return nil, "", nil
			}
			match = tray
		}
	}
	if match == nil || str(match["tray_type"]) == "" {
		return nil, "", nil
	}
	material, e := materialFromTray(r.tx, match)
	return material, str(cleanTag(match["tray_uuid"], 32)), e
}
