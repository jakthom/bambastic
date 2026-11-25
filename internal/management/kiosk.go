package management

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"strings"
	"time"
)

func device(r request) (row, error) {
	return one(r.tx, "SELECT * FROM spoolbuddy_devices WHERE device_id=?", r.PathValue("device"))
}
func presentDevice(v row) row {
	t, _ := time.Parse(time.RFC3339Nano, str(v["last_seen"]))
	v["online"] = time.Since(t) < 90*time.Second
	for _, key := range []string{"pending_write_payload", "pending_system_payload", "ssh_host_key"} {
		delete(v, key)
	}
	if st, ok := v["system_stats"].(string); ok {
		var decoded any
		if json.Unmarshal([]byte(st), &decoded) == nil {
			v["system_stats"] = decoded
		}
	}
	v["ssh_public_key"] = nil
	return v
}
func (s *Service) kiosk(m *http.ServeMux) {
	s.tagWrites(m)
	s.stationRoutes(m)
	const dev = "spoolbuddy/devices"
	const dpath = dev + "/{device}"
	s.route(m, "GET", dev, inventoryRead, func(r request) (any, error) {
		v, e := list(r.tx, "SELECT * FROM spoolbuddy_devices ORDER BY hostname,device_id")
		for _, d := range v {
			presentDevice(d)
		}
		return v, e
	})
	s.route(m, "POST", dev+"/register", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("DeviceRegisterRequest")
		if e != nil {
			return nil, e
		}
		dev, e := one(r.tx, "SELECT id FROM spoolbuddy_devices WHERE device_id=?", d["device_id"])
		d["last_seen"] = now()
		var id int64
		if e == sql.ErrNoRows {
			d["display_brightness"] = 100
			d["display_blank_timeout"] = 0
			d["nfc_ok"] = false
			d["scale_ok"] = false
			d["uptime_s"] = 0
			id, e = insert(r.tx, "spoolbuddy_devices", d)
		} else if e == nil {
			id = intID(dev["id"])
			delete(d, "tare_offset")
			delete(d, "calibration_factor")
			e = update(r.tx, "spoolbuddy_devices", id, d)
		}
		if e != nil {
			return nil, e
		}
		if e = bindStation(r, intID(id)); e != nil {
			return nil, e
		}
		v, e := get(r.tx, "spoolbuddy_devices", id)
		if e != nil {
			return nil, e
		}
		return presentDevice(v), nil
	})
	s.route(m, "DELETE", dpath, inventoryWrite, func(r request) (any, error) {
		return row{"status": "deleted", "device_id": r.PathValue("device")}, affect(r.tx, "DELETE FROM spoolbuddy_devices WHERE device_id=?", r.PathValue("device"))
	})
	s.route(m, "POST", dpath+"/heartbeat", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if e = bindStation(r, intID(dev["id"])); e != nil {
			return nil, e
		}
		d, e := r.input("HeartbeatRequest")
		if e != nil {
			return nil, e
		}
		reply := obj(dev, "pending_command pending_write_payload pending_system_payload tare_offset calibration_factor display_brightness display_blank_timeout")
		if dev["pending_command"] == "write_tag" {
			var v row
			if e = json.Unmarshal([]byte(str(dev["pending_write_payload"])), &v); e != nil {
				return nil, e
			}
			delete(v, "base_url")
			reply["pending_write_payload"] = v
		} else if stationCommand(str(dev["pending_command"])) {
			if e = s.stationPayload(dev, reply); e != nil {
				return nil, e
			}
		} else {
			d["pending_command"] = nil
		}
		if truth(d["scale_ok"]) {
			d["has_scale"] = true
		}
		if truth(d["nfc_ok"]) {
			d["has_nfc"] = true
		}
		d["last_seen"] = now()
		d["updated_at"] = now()
		if e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), d); e != nil {
			return nil, e
		}
		return reply, nil
	})
	s.route(m, "GET", dpath+"/calibration", inventoryRead, func(r request) (any, error) { d, e := device(r); return obj(d, "tare_offset calibration_factor"), e })
	for _, op := range []string{"tare", "set-tare", "set-factor"} {
		s.route(m, "POST", dpath+"/calibration/"+op, inventoryWrite, func(r request) (any, error) {
			dev, e := device(r)
			if e != nil {
				return nil, e
			}
			v := row{}
			switch op {
			case "tare":
				v["pending_command"] = "tare"
			case "set-tare":
				d, e := r.input("SetTareRequest")
				if e != nil {
					return nil, e
				}
				v["tare_offset"] = d["tare_offset"]
				v["last_calibrated_at"] = now()
			case "set-factor":
				d, e := r.input("SetCalibrationFactorRequest")
				if e != nil {
					return nil, e
				}
				tare := num(dev["tare_offset"])
				if d["tare_raw_adc"] != nil {
					tare = num(d["tare_raw_adc"])
				}
				delta := num(d["raw_adc"]) - tare
				if delta == 0 {
					return nil, bad("Calibration readings must differ")
				}
				v["calibration_factor"] = num(d["known_weight_grams"]) / delta
				v["last_calibrated_at"] = now()
			}
			if e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), v); e != nil {
				return nil, e
			}
			if op == "tare" {
				return row{"status": "ok", "message": "Tare command queued"}, nil
			}
			dev, e = device(r)
			return obj(dev, "tare_offset calibration_factor"), e
		})
	}
	for _, method := range []string{"GET", "PUT"} {
		s.route(m, method, dpath+"/display", inventoryWrite, func(r request) (any, error) {
			d, e := device(r)
			if e != nil {
				return nil, e
			}
			if method == "GET" {
				return row{"brightness": d["display_brightness"], "blank_timeout": d["display_blank_timeout"]}, nil
			}
			v, e := r.input("DisplaySettingsRequest")
			if e != nil {
				return nil, e
			}
			if e = update(r.tx, "spoolbuddy_devices", intID(d["id"]), row{"display_brightness": v["brightness"], "display_blank_timeout": v["blank_timeout"]}); e != nil {
				return nil, e
			}
			v["status"] = "ok"
			return v, nil
		})
	}
	s.route(m, "POST", "spoolbuddy/scale/reading", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("ScaleReadingRequest")
		if e != nil {
			return nil, e
		}
		d["type"] = "spoolbuddy_weight"
		r.event(d)
		return row{"status": "ok"}, nil
	})
	s.route(m, "POST", "spoolbuddy/scale/update-spool-weight", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("UpdateSpoolWeightRequest")
		if e != nil {
			return nil, e
		}
		if strings.EqualFold(setting(r.Context(), r.tx, "spoolman_enabled", "false"), "true") {
			return weighRemote(r, intID(d["spool_id"]), num(d["weight_grams"]))
		}
		spool, e := get(r.tx, "spool", intID(d["spool_id"]))
		if e != nil {
			return nil, e
		}
		g := num(d["weight_grams"])
		if g < 0 {
			return nil, bad("Weight must be positive")
		}
		used := math.Max(0, num(spool["label_weight"])-math.Max(0, g-num(spool["core_weight"])))
		e = update(r.tx, "spool", intID(spool["id"]), row{"weight_locked": true, "weight_used": used, "last_weighed_at": now(), "last_scale_weight": g, "updated_at": now()})
		r.event(row{"type": "inventory_changed"})
		return row{"status": "ok", "weight_used": used}, e
	})
	s.route(m, "POST", "spoolbuddy/nfc/tag-scanned", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("TagScannedRequest")
		if e != nil {
			return nil, e
		}
		data, uuid, e := scannedMaterial(r.tx, d["raw_blocks"], str(d["opentag3d_ndef"]))
		if e != nil {
			return nil, e
		}
		if data == nil && uuid == "" && s.MaterialSlots != nil && (num(d["sak"]) == 8 || num(d["sak"]) == 24) {
			data, uuid, e = s.materialOnPrinter(r, d)
			if e != nil {
				return nil, e
			}
		}
		if uuid != "" {
			if existing := str(d["tray_uuid"]); existing != "" && !strings.EqualFold(existing, uuid) {
				return nil, invalid("Tag UUID differs from the supplied spool data")
			}
			d["tray_uuid"] = uuid
		}
		var sp row
		if remoteEnabled(r.Context(), r.tx) {
			sp, e = remoteTag(r, str(d["tray_uuid"]), str(d["tag_uid"]))
		} else {
			sp, e = byTag(r, str(d["tray_uuid"]), str(d["tag_uid"]), false)
			if e == nil && cleanTag(sp["tray_uuid"], 32) == nil {
				if shared := cleanTag(d["tray_uuid"], 32); shared != nil {
					_, e = r.tx.Exec("UPDATE spool SET tray_uuid=? WHERE id=? AND (tray_uuid IS NULL OR tray_uuid='')", shared, sp["id"])
					if e == nil {
						sp["tray_uuid"] = shared
					}
				}
			}
		}
		var p *problem
		if e != nil && !(errors.As(e, &p) && p.code == 404) {
			return nil, e
		}
		res := row{"status": "ok", "matched": e == nil, "spool_id": nil}
		if sp != nil {
			res["spool_id"] = sp["id"]
		}
		event := row{"type": "spoolbuddy_unknown_tag", "device_id": d["device_id"], "tag_uid": d["tag_uid"], "tray_uuid": d["tray_uuid"], "sak": d["sak"], "tag_type": d["tag_type"]}
		if sp == nil && data != nil {
			event["tag_data"] = data
		}
		if sp != nil {
			event["type"] = "spoolbuddy_tag_matched"
			event["spool"] = obj(sp, "id material subtype color_name color_name_is_synthesized rgba extra_colors effect_type brand label_weight core_weight weight_used tag_uid tray_uuid slicer_filament slicer_filament_name storage_location location_id")
			event["spool_id"] = sp["id"]
		}
		r.event(event)
		return res, nil
	})
	s.route(m, "POST", "spoolbuddy/nfc/tag-removed", inventoryWrite, func(r request) (any, error) {
		d, e := r.input("TagRemovedRequest")
		if e != nil {
			return nil, e
		}
		d["type"] = "spoolbuddy_tag_removed"
		r.event(d)
		return row{"status": "ok"}, nil
	})
}
