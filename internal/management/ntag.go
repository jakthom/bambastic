package management

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"
)

func ndef(sp row, v2 bool) []byte {
	size := 102
	version := 1000
	if v2 {
		size, version = 162, 2004
	}
	b := make([]byte, size)
	put16 := func(offset, n int) { binary.BigEndian.PutUint16(b[offset:], uint16(min(max(0, n), 65535))) }
	text := func(off, n int, v any) {
		p := []byte(str(v))
		p = p[:min(n, len(p))]
		for !utf8.Valid(p) && len(p) > 0 {
			p = p[:len(p)-1]
		}
		copy(b[off:off+n], bytes.Repeat([]byte(" "), n))
		copy(b[off:], p)
	}
	color := func(off int, v string) {
		v = strings.TrimPrefix(strings.TrimSpace(v), "#")
		if len(v) == 6 && v2 {
			v += "ff"
		}
		v = (v + "00000000")[:8]
		if x, e := hex.DecodeString(v); e == nil {
			copy(b[off:], x)
		}
	}
	put16(0, version)
	text(2, 5, sp["material"])
	text(7, 5, sp["subtype"])
	heat := func(k string) byte { return byte(min(255, max(0, int(num(sp[k]))/5))) }
	if v2 {
		text(12, 16, sp["brand"])
		text(28, 32, sp["color_name"])
		color(60, str(sp["rgba"]))
		n := 0
		for _, c := range strings.Split(str(sp["extra_colors"]), ",") {
			if strings.TrimSpace(c) != "" && n < 3 {
				color(64+4*n, c)
				n++
			}
		}
		put16(140, 1750)
		b[144], b[145], b[146] = heat("nozzle_temp_min"), heat("nozzle_temp_min"), heat("nozzle_temp_max")
		put16(158, int(num(sp["label_weight"])))
		put16(160, int(num(sp["core_weight"])))
	} else {
		text(27, 16, sp["brand"])
		text(43, 32, sp["color_name"])
		color(75, str(sp["rgba"]))
		put16(92, 1750)
		put16(94, int(num(sp["label_weight"])))
		b[96] = heat("nozzle_temp_min")
	}
	mime := "application/opentag3d"
	out := []byte{3, byte(3 + len(mime) + size), 0xd2, byte(len(mime)), byte(size)}
	out = append(out, []byte(mime)...)
	out = append(out, b...)
	return append(out, 0xfe)
}
func (s *Service) tagWrites(m *http.ServeMux) {
	s.route(m, "POST", "spoolbuddy/nfc/write-tag", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("WriteTagRequest")
		if e != nil {
			return nil, e
		}
		dev, e := one(r.tx, "SELECT * FROM spoolbuddy_devices WHERE device_id=?", d["device_id"])
		if e != nil {
			return nil, e
		}
		if dev["pending_command"] != nil {
			return nil, &problem{409, "Station already has a pending command"}
		}
		sid := intID(d["spool_id"])
		origin := "local"
		var v row
		if remoteEnabled(r.Context(), r.tx) {
			origin = "spoolman"
			raw, e := rawSpool(r, sid)
			if e != nil {
				return nil, e
			}
			v = remoteSpool(raw)
		} else {
			v, e = get(r.tx, "spool", sid)
			if e != nil {
				return nil, e
			}
		}
		if v["archived_at"] != nil || str(v["material"]) == "" {
			return nil, bad("Spool must be active and have a material")
		}
		var nonce [16]byte
		if _, e = rand.Read(nonce[:]); e != nil {
			return nil, e
		}
		payload := row{"spool_id": sid, "data_origin": origin, "write_id": hex.EncodeToString(nonce[:]), "ndef_data_hex": hex.EncodeToString(ndef(v, false)), "ndef_v2_hex": hex.EncodeToString(ndef(v, true))}
		if origin == "spoolman" {
			payload["base_url"] = setting(r.Context(), r.tx, "spoolman_url", "")
		}
		b, _ := json.Marshal(payload)
		e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": "write_tag", "pending_write_payload": string(b)})
		return row{"status": "queued"}, e
	})
	s.route(m, "POST", "spoolbuddy/devices/{device}/cancel-write", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if dev["pending_command"] == "write_tag" {
			e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": nil, "pending_write_payload": nil})
		}
		return row{"status": "ok"}, e
	})
	s.route(m, "POST", "spoolbuddy/nfc/write-result", inventoryWrite, func(r request) (any, error) {
		body, e := r.body()
		if e != nil {
			return nil, e
		}
		d, e := input("WriteTagResultRequest", body)
		if e != nil {
			return nil, e
		}
		dev, e := one(r.tx, "SELECT * FROM spoolbuddy_devices WHERE device_id=?", d["device_id"])
		if e != nil {
			return nil, e
		}
		var payload row
		_ = json.Unmarshal([]byte(str(dev["pending_write_payload"])), &payload)
		if dev["pending_command"] != "write_tag" || intID(payload["spool_id"]) != intID(d["spool_id"]) || body["write_id"] != nil && body["write_id"] != payload["write_id"] {
			return row{"status": "ignored"}, nil
		}
		uid := strings.ToUpper(str(d["tag_uid"]))
		if len(uid) < 8 || len(uid) > 30 || len(uid)%2 != 0 || strings.Trim(uid, "0") == "" || strings.Trim(uid, "ABCDEF0123456789") != "" {
			return nil, invalid("Invalid tag UID")
		}
		result := row{"type": "spoolbuddy_tag_written", "device_id": d["device_id"], "spool_id": d["spool_id"], "tag_uid": uid, "message": d["message"]}
		if !truth(d["success"]) {
			result["type"] = "spoolbuddy_tag_write_failed"
		} else if payload["data_origin"] == "spoolman" {
			if !remoteEnabled(r.Context(), r.tx) || setting(r.Context(), r.tx, "spoolman_url", "") != payload["base_url"] {
				result["type"] = "spoolbuddy_tag_link_failed"
				result["message"] = "Inventory connection changed during write"
			} else {
				current, e := rawSpool(r, intID(d["spool_id"]))
				if e != nil {
					return nil, e
				}
				exists, e := remoteTag(r, "", uid)
				if exists != nil && intID(exists["id"]) != intID(d["spool_id"]) {
					result["type"] = "spoolbuddy_tag_link_failed"
					result["message"] = "Tag already linked to another spool"
				} else {
					if e != nil {
						p, ok := e.(*problem)
						if !ok || p.code != 404 {
							return nil, e
						}
					}
					extra := sub(current["extra"])
					b, _ := json.Marshal(uid)
					extra["tag"] = string(b)
					if e = remote(r, "PATCH", fmt.Sprintf("spool/%d", intID(d["spool_id"])), row{"extra": extra}, nil); e != nil {
						return nil, e
					}
				}
			}
		} else {
			tag := cleanTag(uid, 16)
			n, e := count(r.tx, "SELECT count(*) FROM spool WHERE archived_at IS NULL AND id!=? AND upper(tag_uid)=?", d["spool_id"], tag)
			if e != nil {
				return nil, e
			}
			if n > 0 {
				result["type"] = "spoolbuddy_tag_link_failed"
				result["message"] = "Tag already linked to another spool"
			} else {
				res, e := r.tx.Exec("UPDATE spool SET tag_uid=?,tag_type='OpenTag3D',encode_time=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=? AND archived_at IS NULL", tag, d["spool_id"])
				if e != nil {
					return nil, e
				}
				n, _ := res.RowsAffected()
				if n == 0 {
					result["type"] = "spoolbuddy_tag_link_failed"
					result["message"] = "Spool no longer available"
				}
			}
		}
		if e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": nil, "pending_write_payload": nil}); e != nil {
			return nil, e
		}
		r.event(result)
		r.event(row{"type": "inventory_changed"})
		return row{"status": "ok"}, nil
	})
}
