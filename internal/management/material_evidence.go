package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

type spoolInterval struct {
	Layer int   `json:"layer"`
	Spool int64 `json:"spool"`
}
type feederInterval struct {
	Layer int `json:"layer"`
	Tray  int `json:"tray"`
}
type holderEvidence struct {
	Spools      []spoolInterval `json:"spools,omitempty"`
	Assignment  int64           `json:"assignment,omitempty"`
	StartRemain *int            `json:"start_remain,omitempty"`
	EndRemain   *int            `json:"end_remain,omitempty"`
	UID, UUID   string
	ChangedTag  bool `json:"changed_tag,omitempty"`
	AwaitingTag bool `json:"awaiting_tag,omitempty"`
}
type runEvidence struct {
	Remote                 bool   `json:"remote,omitempty"`
	Base                   string `json:"base,omitempty"`
	Layer, Layers, Percent int
	Mapping                []int
	Feed                   []feederInterval
	Holders                map[int]*holderEvidence
}

func (o *runEvidence) holder(addr int) *holderEvidence {
	if v := o.Holders[addr]; v != nil {
		return v
	}
	v := &holderEvidence{}
	o.Holders[addr] = v
	return v
}
func (h *holderEvidence) interval(spool int64, layer int) {
	n := len(h.Spools)
	if n == 0 && spool == 0 {
		return
	}
	if n > 0 && h.Spools[n-1].Spool == spool {
		return
	}
	if n == 0 {
		layer = 0
	}
	v := spoolInterval{layer, spool}
	if n > 0 && h.Spools[n-1].Layer == layer {
		h.Spools[n-1] = v
		return
	}
	h.Spools = append(h.Spools, v)
}
func (h *holderEvidence) assign(spool int64, layer int) {
	if h.Assignment != spool {
		h.AwaitingTag = h.Assignment > 0 && spool > 0
		h.Assignment = spool
		h.interval(spool, layer)
	}
}
func readRunEvidence(ctx context.Context, db access.Query, id int64) (runEvidence, error) {
	var raw string
	o := runEvidence{Holders: map[int]*holderEvidence{}}
	err := db.QueryRowContext(ctx, "SELECT observations FROM bambastic_material_evidence WHERE run_id=?", id).Scan(&raw)
	if err == nil {
		err = json.Unmarshal([]byte(raw), &o)
	}
	return o, err
}
func storeEvidence(ctx context.Context, tx *sql.Tx, id int64, o runEvidence) error {
	b, e := json.Marshal(o)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, `INSERT INTO bambastic_material_evidence(run_id,observations) VALUES(?,?)
	ON CONFLICT(run_id) DO UPDATE SET observations=excluded.observations WHERE bambastic_material_evidence.observations!=excluded.observations`, id, string(b))
	return e
}
func noteAssignment(r request, k slotKey, spool int64, remote bool) error {
	var id int64
	e := r.tx.QueryRowContext(r.Context(), `SELECT l.id FROM print_log_entries l JOIN bambastic_material_evidence e ON e.run_id=l.id WHERE l.printer_id=? AND l.completed_at IS NULL ORDER BY l.id DESC LIMIT 1`, k.printer).Scan(&id)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	o, e := readRunEvidence(r.Context(), r.tx, id)
	if e != nil {
		return e
	}
	if o.Remote != remote || remote && strings.TrimRight(setting(r.Context(), r.tx, "spoolman_url", ""), "/") != o.Base {
		return nil
	}
	o.holder(slotAddress(int(k.ams), int(k.tray))).assign(spool, o.Layer)
	return storeEvidence(r.Context(), r.tx, id, o)
}
func telemetryInt(v any, max int) (int, bool) {
	if v == nil {
		return 0, false
	}
	n, e := strconv.ParseFloat(str(v), 64)
	if e != nil || n < 0 || n > float64(max) || math.Trunc(n) != n {
		return 0, false
	}
	return int(n), true
}
func reportMapping(x any, wire bool) []int {
	items, ok := x.([]any)
	if !ok || len(items) > 128 {
		return nil
	}
	result := make([]int, len(items))
	valid := false
	for i, v := range items {
		n, ok := telemetryInt(v, 65535)
		result[i] = -1
		if !ok || n == 65535 || n == 255 {
			continue
		}
		if wire {
			unit, slot := n>>8, n&255
			if unit == 255 {
				if slot < 247 {
					continue
				}
				n = slot
			} else if slot <= 8 {
				n = slotAddress(unit, slot)
			} else {
				continue
			}
		}
		if n <= 255 {
			result[i] = n
			valid = true
		}
	}
	if !valid {
		return nil
	}
	return result
}

// ObserveMaterial records physical telemetry only for a confirmed run. An absent
// reading is kept distinct from a measured zero, including across a server restart.
func ObserveMaterial(ctx context.Context, tx *sql.Tx, rid, printer int64, report map[string]any, opening, terminal bool) error {
	o, e := readRunEvidence(ctx, tx, rid)
	if errors.Is(e, sql.ErrNoRows) {
		if !opening {
			return nil
		}
		o.Remote = remoteEnabled(ctx, tx)
		if o.Remote {
			o.Base = strings.TrimRight(setting(ctx, tx, "spoolman_url", ""), "/")
		}
		var mapping sql.NullString
		e = tx.QueryRowContext(ctx, "SELECT q.ams_mapping FROM print_log_entries l LEFT JOIN print_queue q ON q.id=l.queue_item_id WHERE l.id=?", rid).Scan(&mapping)
		if e == nil && mapping.Valid {
			_ = json.Unmarshal([]byte(mapping.String), &o.Mapping)
		}
	}
	if e != nil {
		return e
	}
	if layer, ok := telemetryInt(report["layer_num"], 1000000); ok && layer > o.Layer {
		o.Layer = layer
	}
	if total, ok := telemetryInt(report["total_layer_num"], 1000000); ok && total > o.Layers {
		o.Layers = total
	}
	if p, ok := telemetryInt(report["mc_percent"], 100); ok && p > o.Percent {
		o.Percent = p
	}
	if len(o.Mapping) == 0 {
		o.Mapping = reportMapping(report["ams_mapping"], false)
		if len(o.Mapping) == 0 {
			o.Mapping = reportMapping(report["mapping"], true)
		}
	}
	ams := sub(report["ams"])
	now := ams["tray_now"]
	if now == nil {
		now = report["tray_now"]
	}
	if addr, ok := telemetryInt(now, 254); ok && !(terminal && o.Layer == 0) {
		n := len(o.Feed)
		if n == 0 {
			o.Feed = append(o.Feed, feederInterval{0, addr})
		} else if o.Feed[n-1].Tray != addr {
			v := feederInterval{o.Layer, addr}
			if o.Layer == o.Feed[n-1].Layer {
				o.Feed[n-1] = v
			} else {
				o.Feed = append(o.Feed, v)
			}
		}
	}
	table, col := "spool_assignment", "spool_id"
	if o.Remote {
		table, col = "spoolman_slot_assignments", "spoolman_spool_id"
	}
	if o.Remote == remoteEnabled(ctx, tx) && (!o.Remote || o.Base == strings.TrimRight(setting(ctx, tx, "spoolman_url", ""), "/")) {
		assigned, e := list(tx, "SELECT ams_id,tray_id,"+col+" spool_id FROM "+table+" WHERE printer_id=?", printer)
		if e != nil {
			return e
		}
		current := map[int]int64{}
		for _, v := range assigned {
			addr := slotAddress(int(num(v["ams_id"])), int(num(v["tray_id"])))
			current[addr] = intID(v["spool_id"])
			o.holder(addr).assign(current[addr], o.Layer)
		}
		for addr, h := range o.Holders {
			if current[addr] == 0 && h.Assignment > 0 {
				h.assign(0, o.Layer)
			}
		}
	}
	slots, _, _ := reportSlots(printer, report)
	if terminal {
		for _, h := range o.Holders {
			h.EndRemain = nil
		}
	}
	for k, v := range slots {
		h := o.holder(slotAddress(int(k.ams), int(k.tray)))
		if v["exists"] == false {
			continue
		}
		uid, uuid := str(cleanTag(v["tag_uid"], 16)), str(cleanTag(v["tray_uuid"], 32))
		changed := uid != "" && h.UID != "" && uid != h.UID || uuid != "" && h.UUID != "" && uuid != h.UUID
		if changed {
			h.ChangedTag = true
			// Physical identity changed before the operator identified the new roll.
			// The tag for an explicitly assigned replacement may arrive on a later push.
			if !h.AwaitingTag {
				h.interval(0, o.Layer)
			}
			h.AwaitingTag = false
		}
		if uid != "" {
			h.UID = uid
		}
		if uuid != "" {
			h.UUID = uuid
		}
		if raw, ok := telemetryInt(v["remain"], 100); ok {
			if opening && h.StartRemain == nil {
				h.StartRemain = &raw
			}
			if terminal {
				h.EndRemain = &raw
			}
		}
	}
	return storeEvidence(ctx, tx, rid, o)
}

// MaterialCloseInputs contains immutable plate extrusion and independently read
// remote reference weights. Neither file decompression nor network I/O needs a write transaction.
type MaterialCloseInputs struct {
	RunID     int64
	layers    *layerExtrusion
	remoteNet map[int64]float64
}

func PrepareMaterialClose(ctx context.Context, db *sql.DB, id, printer, plate int64, path string, complete bool, report map[string]any) MaterialCloseInputs {
	out := MaterialCloseInputs{RunID: id}
	o, e := readRunEvidence(ctx, db, id)
	if e != nil {
		return out
	}
	split := len(o.Feed) > 1
	for _, v := range o.Holders {
		if len(v.Spools) > 1 {
			split = true
			break
		}
	}
	if path != "" && (split || !complete) {
		out.layers, _ = readExtrusion(ctx, path, plate)
	}
	if !o.Remote || o.Base == "" {
		return out
	}
	base, e := access.Setting(ctx, db, "spoolman_url", "")
	if e != nil || strings.TrimRight(base, "/") != o.Base {
		return out
	}
	on, e := access.Setting(ctx, db, "spoolman_enabled", "false")
	if e != nil || !strings.EqualFold(on, "true") {
		return out
	}
	disconnected, e := access.Setting(ctx, db, "bambastic_spoolman_disconnected", "")
	if e != nil || strings.EqualFold(disconnected, "true") {
		return out
	}
	out.remoteNet = map[int64]float64{}
	slots, _, _ := reportSlots(printer, report)
	for k, slot := range slots {
		h := o.Holders[slotAddress(int(k.ams), int(k.tray))]
		end, known := telemetryInt(slot["remain"], 100)
		if h == nil || h.Assignment == 0 || len(h.Spools) != 1 || h.ChangedTag || h.StartRemain == nil || !known || *h.StartRemain <= end || slot["exists"] == false {
			continue
		}
		out.remoteNet[h.Assignment] = 0
	}
	if len(out.remoteNet) == 0 {
		return out
	}
	network, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var spools []row
	if len(out.remoteNet) == 1 {
		for sp := range out.remoteNet {
			var v row
			if remoteCall(network, o.Base, "GET", fmt.Sprintf("spool/%d", sp), nil, &v) == nil && intID(v["id"]) == sp {
				spools = []row{v}
			}
		}
	} else {
		if remoteCall(network, o.Base, "GET", "spool?allow_archived=true", nil, &spools) != nil {
			return out
		}
	}
	for _, v := range spools {
		sp := intID(v["id"])
		if _, needed := out.remoteNet[sp]; !needed {
			continue
		}
		net := num(v["initial_weight"])
		if net <= 0 {
			net = num(sub(v["filament"])["weight"])
		}
		if net > 0 {
			out.remoteNet[sp] = net
		}
	}
	return out
}
