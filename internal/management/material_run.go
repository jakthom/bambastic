package management

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

func initializeMaterialRuns(db *sql.DB) error {
	tx, e := db.Begin()
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if _, e = tx.Exec(`CREATE TABLE IF NOT EXISTS bambastic_material_runs(run_id INTEGER PRIMARY KEY REFERENCES print_log_entries(id) ON DELETE CASCADE,settled INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS bambastic_material_allocations(id INTEGER PRIMARY KEY,run_id INTEGER NOT NULL REFERENCES bambastic_material_runs(run_id) ON DELETE CASCADE,
 spool_id INTEGER REFERENCES spool(id) ON DELETE SET NULL,remote_spool_id INTEGER,base_url TEXT NOT NULL DEFAULT '', fraction REAL NOT NULL DEFAULT 1,
 state TEXT NOT NULL DEFAULT 'tracking',grams REAL NOT NULL DEFAULT 0,usage_id INTEGER REFERENCES spool_usage_history(id) ON DELETE SET NULL, attempted_at DATETIME,last_error TEXT);
 CREATE TABLE IF NOT EXISTS bambastic_material_evidence(run_id INTEGER PRIMARY KEY REFERENCES bambastic_material_runs(run_id) ON DELETE CASCADE,observations TEXT NOT NULL);`); e != nil {
		return e
	}
	// Earlier Go installations stored a single spool per run. Preserve their sent/uncertain delivery state.
	for _, src := range []string{"bambastic_run_spools", "bambastic_remote_runs"} {
		n, e := count(tx, "SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", src)
		if e != nil {
			return e
		}
		if n == 0 {
			continue
		}
		state := "settled"
		fields := "run_id,spool_id"
		selectFields := "run_id,spool_id"
		if src == "bambastic_remote_runs" {
			state = "state!='tracking'"
			fields = "run_id,remote_spool_id,base_url,state,grams,usage_id,attempted_at,last_error"
			selectFields = "run_id,spool_id,base_url,state,grams,usage_id,attempted_at,last_error"
		}
		if _, e = tx.Exec("INSERT OR IGNORE INTO bambastic_material_runs(run_id,settled) SELECT run_id," + state + " FROM " + src); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO bambastic_material_allocations(" + fields + ") SELECT " + selectFields + " FROM " + src + " legacy WHERE NOT EXISTS(SELECT 1 FROM bambastic_material_allocations new WHERE new.run_id=legacy.run_id)"); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func slotAddress(ams, tray int) int {
	if ams == 255 {
		if tray < 2 {
			return 254 + tray
		}
		return 245 + tray
	}
	if ams == 32 {
		return 28 + tray
	}
	if ams == 16 {
		return 24 + tray
	}
	if ams >= 128 {
		return ams
	}
	return 4*ams + tray
}
func plateAllocations(tx *sql.Tx, run, q row, mapping []int, slots map[int]int64) (map[int64]float64, error) {
	var asset row
	var e error
	var meta row
	if aid := intID(run["archive_id"]); aid > 0 {
		asset, e = get(tx, "print_archives", aid)
		if e != nil {
			return nil, e
		}
		meta = sub(asset["extra_data"])
	}
	if (meta == nil || meta["plates"] == nil) && intID(q["library_file_id"]) > 0 {
		asset, e = get(tx, "library_files", intID(q["library_file_id"]))
		if e != nil {
			return nil, e
		}
		meta = sub(asset["file_metadata"])
	}
	plates, _ := meta["plates"].([]any)
	var plate row
	for _, p := range plates {
		v := sub(p)
		if intID(v["index"]) == intID(run["plate_id"]) || intID(run["plate_id"]) == 0 && len(plates) == 1 {
			plate = v
			break
		}
	}
	filaments, _ := plate["filaments"].([]any)
	gramsBySpool := map[int64]float64{}
	var total float64
	for _, f := range filaments {
		v := sub(f)
		grams := num(v["used_g"])
		if grams <= 0 {
			continue
		}
		total += grams
		index := intID(v["id"]) - 1
		if index < 0 || index >= int64(len(mapping)) {
			continue
		}
		if id := slots[mapping[index]]; id > 0 {
			gramsBySpool[id] += grams
		}
	}
	if total == 0 {
		return nil, nil
	}
	for id, n := range gramsBySpool {
		gramsBySpool[id] = n / total
	}
	return gramsBySpool, nil
}

// SnapshotRun freezes each resolvable physical spool and the selected plate's material weights at print start.
func SnapshotRun(ctx context.Context, tx *sql.Tx, runID int64) error {
	run, e := get(tx, "print_log_entries", runID)
	if e != nil {
		return e
	}
	res, e := tx.ExecContext(ctx, "INSERT OR IGNORE INTO bambastic_material_runs(run_id) VALUES(?)", runID)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n == 0 {
		return e
	}
	remote := remoteEnabled(ctx, tx)
	table, key, base := "spool_assignment", "spool_id", ""
	if remote {
		table, key, base = "spoolman_slot_assignments", "spoolman_spool_id", setting(ctx, tx, "spoolman_url", "")
	}
	rows, e := list(tx, "SELECT ams_id,tray_id,"+key+" spool_id FROM "+table+" WHERE printer_id=?", run["printer_id"])
	if e != nil {
		return e
	}
	slots := map[int]int64{}
	for _, v := range rows {
		address := slotAddress(int(num(v["ams_id"])), int(num(v["tray_id"])))
		slots[address] = intID(v["spool_id"])
	}
	var mapping []int
	var q row
	if id := intID(run["queue_item_id"]); id > 0 {
		q, e = get(tx, "print_queue", id)
		if e != nil {
			return e
		}
		if q["ams_mapping"] != nil {
			if e = json.Unmarshal([]byte(str(q["ams_mapping"])), &mapping); e != nil {
				return nil
			}
		}
	}
	allocations, e := plateAllocations(tx, run, q, mapping, slots)
	if e != nil {
		return e
	}
	if allocations == nil || len(mapping) == 0 {
		allocations = map[int64]float64{}
		targets := map[int]bool{}
		for _, addr := range mapping {
			if addr >= 0 && addr != 255 {
				targets[addr] = true
			}
		}
		if len(targets) == 1 {
			for addr := range targets {
				if id := slots[addr]; id > 0 {
					allocations[id] = 1
				}
			}
		} else if len(mapping) == 0 && len(slots) == 1 {
			for _, id := range slots {
				allocations[id] = 1
			}
		}
	}
	for spool, fraction := range allocations {
		local, external := any(spool), any(nil)
		if remote {
			local, external = nil, spool
		}
		_, e = tx.ExecContext(ctx, "INSERT INTO bambastic_material_allocations(run_id,spool_id,remote_spool_id,base_url,fraction) VALUES(?,?,?,?,?)", runID, local, external, base, fraction)
		if e != nil {
			return e
		}
	}
	return nil
}
func settleMaterial(ctx context.Context, tx *sql.Tx, run row, grams float64) error {
	rid := intID(run["id"])
	res, e := tx.ExecContext(ctx, "UPDATE bambastic_material_runs SET settled=1 WHERE run_id=? AND settled=0", rid)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n == 0 {
		return e
	}
	v, e := list(tx, "SELECT * FROM bambastic_material_allocations WHERE run_id=?", rid)
	if e != nil {
		return e
	}
	for _, a := range v {
		part := grams * num(a["fraction"])
		status := str(run["status"])
		// Plate proportions describe a finished toolpath. A stopped multi-material run needs observed per-tool extrusion.
		if a["state"] == "attributed" {
			part = num(a["grams"])
		}
		if part <= 0 || a["state"] != "attributed" && num(a["fraction"]) < 1 && status != "completed" {
			continue
		}
		hist := row{"printer_id": run["printer_id"], "archive_id": run["archive_id"], "print_name": run["print_name"], "status": status, "percent_used": 0, "weight_used": part}
		state := "settled"
		if id := intID(a["spool_id"]); id > 0 {
			sp, e := get(tx, "spool", id)
			if e != nil {
				return e
			}
			rate := num(sp["cost_per_kg"])
			if sp["cost_per_kg"] == nil {
				rate, _ = strconv.ParseFloat(setting(ctx, tx, "default_filament_cost", "25"), 64)
			}
			if _, e = tx.ExecContext(ctx, "UPDATE spool SET weight_used=weight_used+?,last_used=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE id=?", part, id); e != nil {
				return e
			}
			hist["spool_id"] = id
			hist["cost"] = cents(rate * part / 1000)
			if w := num(sp["label_weight"]); w > 0 {
				hist["percent_used"] = math.Round(part / w * 100)
			}
		} else if id := intID(a["remote_spool_id"]); id > 0 {
			if status != "completed" && strings.EqualFold(setting(ctx, tx, "spoolman_report_partial_usage", "true"), "false") {
				continue
			}
			hist["spoolman_spool_id"] = id
			// Per-print accounting is independent of the deprecated AMS weight-sync switch.
			state = "pending"
		} else {
			continue
		}
		h, e := insert(tx, "spool_usage_history", hist)
		if e != nil {
			return e
		}
		if _, e = tx.Exec("UPDATE bambastic_material_allocations SET usage_id=?,grams=?,state=? WHERE id=?", h, part, state, a["id"]); e != nil {
			return e
		}
	}
	return nil
}
