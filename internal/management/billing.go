package management

import (
	"context"
	"database/sql"

	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

func cents(v float64) float64 { return math.Round(v*100) / 100 }
func sum(tx *sql.Tx, q string, args ...any) (float64, error) {
	var n float64
	e := tx.QueryRow(q, args...).Scan(&n)
	return n, e
}
func monthStart(ctx context.Context, tx *sql.Tx) string {
	zone, e := time.LoadLocation(setting(ctx, tx, "finance_budget_reset_timezone", "UTC"))
	if e != nil {
		zone = time.UTC
	}
	day, _ := strconv.Atoi(setting(ctx, tx, "finance_budget_reset_day", "1"))
	if day < 1 || day > 31 {
		day = 1
	}
	t := time.Now().In(zone)
	first := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, zone)
	reset := func(start time.Time) time.Time {
		last := start.AddDate(0, 1, -1).Day()
		return start.AddDate(0, 0, min(day, last)-1)
	}
	if t.Before(reset(first)) {
		first = first.AddDate(0, -1, 0)
	}
	return reset(first).UTC().Format("2006-01-02 15:04:05")
}
func centerSummary(ctx context.Context, tx *sql.Tx, c row, canPrint bool) (row, error) {
	return centerAvailable(ctx, tx, c, canPrint, 0)
}
func centerAvailable(ctx context.Context, tx *sql.Tx, c row, canPrint bool, excludeQueue int64) (row, error) {
	id := c["id"]
	balance, e := sum(tx, "SELECT coalesce(round(sum(amount),2),0) FROM wallet_transactions WHERE cost_center_id=? AND NOT is_voided", id)
	if e != nil {
		return nil, e
	}
	c["total_balance"] = balance
	c["can_print"] = canPrint
	c["budget_mode"] = "none"
	c["budget_limit"] = nil
	c["budget_used"] = nil
	c["budget_available"] = nil
	limit := c["total_budget"]
	where := ""
	args := []any{id}
	mode := "total"
	if c["monthly_budget"] != nil {
		mode = "monthly"
		limit = c["monthly_budget"]
		where = " AND julianday(created_at)>=julianday(?)"
		args = append(args, monthStart(ctx, tx))
	}
	if limit != nil {
		spent, e := sum(tx, "SELECT coalesce(round(-sum(amount),2),0) FROM wallet_transactions WHERE cost_center_id=? AND NOT is_voided AND amount<0"+where, args...)
		if e != nil {
			return nil, e
		}
		reserved, e := sum(tx, `SELECT (SELECT coalesce(sum(amount),0) FROM budget_reservations WHERE cost_center_id=? AND status='active' AND NOT (source_type='print_queue' AND source_id=?))+
 (SELECT coalesce(sum(estimated_cost),0) FROM print_queue q WHERE q.cost_center_id=? AND q.id!=? AND q.status IN ('pending','printing','dispatching') AND NOT EXISTS(SELECT 1 FROM budget_reservations b WHERE b.source_type='print_queue' AND b.source_id=q.id AND b.status='active'))`, id, excludeQueue, id, excludeQueue)
		if e != nil {
			return nil, e
		}
		c["budget_mode"] = mode
		c["budget_limit"] = limit
		c["budget_used"] = spent
		c["budget_available"] = math.Max(0, cents(num(limit)-spent-reserved))
	}
	return c, nil
}
func canBill(tx *sql.Tx, c row, uid int64, admin bool) (bool, error) {
	if admin {
		return true, nil
	}
	if truth(c["is_private"]) {
		return intID(c["owner_user_id"]) == uid && uid != 0, nil
	}
	n, e := count(tx, "SELECT count(*) FROM cost_center_members WHERE cost_center_id=? AND user_id=? AND can_print", c["id"], uid)
	return n > 0, e
}
func estimate(ctx context.Context, tx *sql.Tx, archive, library, plate int64) (cost, grams float64, err error) {
	rate, _ := strconv.ParseFloat(setting(ctx, tx, "default_filament_cost", "25"), 64)
	if archive != 0 {
		a, e := get(tx, "print_archives", archive)
		if e != nil {
			return 0, 0, e
		}
		total := num(a["filament_used_grams"])
		grams = total
		if plates, ok := a["plate_estimates"].(map[string]any); ok && len(plates) > 0 {
			v, ok := plates[strconv.FormatInt(plate, 10)].(map[string]any)
			if !ok && len(plates) == 1 {
				for _, entry := range plates {
					v, _ = entry.(map[string]any)
				}
			}
			if v != nil {
				grams = num(v["grams"])
			}
		}
		cost = num(a["cost"])
		if cost > 0 && total > 0 {
			cost = cost * grams / total
		}
	} else if library != 0 {
		a, e := get(tx, "library_files", library)
		if e != nil {
			return 0, 0, e
		}
		meta, _ := a["file_metadata"].(map[string]any)
		grams = num(meta["filament_used_grams"])
		if grams == 0 {
			grams = num(meta["total_filament_used_g"])
		}
		if ps, ok := meta["plates"].([]any); ok {
			for _, v := range ps {
				p, _ := v.(map[string]any)
				if intID(p["index"]) == plate || intID(p["plate_id"]) == plate {
					if g := num(p["filament_used_grams"]); g > 0 {
						grams = g
					}
					break
				}
			}
		}
	}
	if cost <= 0 {
		cost = rate * grams / 1000
	}
	return cents(cost), grams, nil
}

// AuthorizeQueue prices work from trusted plate metadata and counts existing, unspent commitments.
func AuthorizeQueue(ctx context.Context, tx *sql.Tx, actorID, costCenterID, archiveID, libraryID, plateID int64, qty int) (float64, error) {
	return authorize(ctx, tx, actorID, costCenterID, archiveID, libraryID, plateID, qty, 0)
}
func authorize(ctx context.Context, tx *sql.Tx, actorID, costCenterID, archiveID, libraryID, plateID int64, qty int, excludeQueue int64) (float64, error) {
	if !strings.EqualFold(setting(ctx, tx, "billing_enabled", "false"), "true") {
		return 0, nil
	}
	if costCenterID == 0 {
		return 0, bad("A cost center is required")
	}
	c, e := get(tx, "cost_centers", costCenterID)
	if e != nil {
		return 0, e
	}
	allowed, e := canBill(tx, c, actorID, access.From(ctx).Admin)
	if e != nil {
		return 0, e
	}
	if !allowed {
		return 0, denied()
	}
	if !truth(c["is_active"]) {
		return 0, bad("Cost center is inactive")
	}
	charge, _, e := estimate(ctx, tx, archiveID, libraryID, plateID)
	if e != nil {
		return 0, e
	}
	if charge <= 0 {
		return 0, bad("A positive cost estimate is required when billing is enabled")
	}
	c, e = centerAvailable(ctx, tx, c, allowed, excludeQueue)
	if e != nil {
		return 0, e
	}
	if c["budget_available"] != nil && cents(charge*float64(max(qty, 1))) > num(c["budget_available"]) {
		return 0, bad("Estimated print cost exceeds available cost center budget")
	}
	return charge, nil
}

func writeLedger(tx *sql.Tx, d row) (row, error) {
	d["amount"] = cents(num(d["amount"]))
	d["is_voided"] = false
	id, e := insert(tx, "wallet_transactions", d)
	if e != nil {
		return nil, e
	}
	if e = rebuildLedger(tx); e != nil {
		return nil, e
	}
	return get(tx, "wallet_transactions", id)
}
func rebuildLedger(tx *sql.Tx) error {
	rows, e := list(tx, `SELECT t.id,t.user_id,t.cost_center_id,t.amount,c.is_private,c.owner_user_id FROM wallet_transactions t LEFT JOIN cost_centers c ON c.id=t.cost_center_id WHERE NOT t.is_voided ORDER BY julianday(t.created_at),t.id`)
	if e != nil {
		return e
	}
	users, shared := map[int64]float64{}, map[int64]float64{}
	for _, v := range rows {
		uid, cc := intID(v["user_id"]), intID(v["cost_center_id"])
		var after any
		if cc != 0 && !truth(v["is_private"]) {
			shared[cc] = cents(shared[cc] + num(v["amount"]))
			after = shared[cc]
		} else if uid != 0 && (cc == 0 || intID(v["owner_user_id"]) == uid) {
			users[uid] = cents(users[uid] + num(v["amount"]))
			after = users[uid]
		}
		if _, e = tx.Exec("UPDATE wallet_transactions SET balance_after=? WHERE id=?", after, v["id"]); e != nil {
			return e
		}
	}
	if _, e = tx.Exec("UPDATE user_wallets SET balance=0,updated_at=CURRENT_TIMESTAMP"); e != nil {
		return e
	}
	for uid, bal := range users {
		if _, e = tx.Exec("INSERT INTO user_wallets(user_id,balance) VALUES(?,?) ON CONFLICT(user_id) DO UPDATE SET balance=excluded.balance,updated_at=CURRENT_TIMESTAMP", uid, bal); e != nil {
			return e
		}
	}
	return nil
}

// SettleRun runs in the caller's completion transaction. The run's own IDs correlate cost,
// material and plate even if another printer has since updated the archive.
func SettleRun(ctx context.Context, tx *sql.Tx, runID int64) error {
	r, e := get(tx, "print_log_entries", runID)
	if e != nil {
		return e
	}
	if r["completed_at"] == nil {
		return nil
	}
	grams := math.Max(0, num(r["filament_used_grams"]))
	status := str(r["status"])
	switch status {
	case "completed", "failed", "aborted", "cancelled":
	default:
		return nil
	}
	if e = settleMaterial(ctx, tx, r, grams); e != nil {
		return e
	}
	if !strings.EqualFold(setting(ctx, tx, "billing_enabled", "false"), "true") {
		return nil
	}
	qid, aid, cc := intID(r["queue_item_id"]), intID(r["archive_id"]), intID(r["cost_center_id"])
	var charge, fullGrams float64
	var library int64
	if qid != 0 {
		q, e := get(tx, "print_queue", qid)
		if e != nil {
			return e
		}
		charge = num(q["estimated_cost"])
		library = intID(q["library_file_id"])
		if cc == 0 {
			cc = intID(q["cost_center_id"])
		}
	}
	baseline, fullGrams, e := estimate(ctx, tx, aid, library, intID(r["plate_id"]))
	if e != nil {
		return e
	}
	if charge <= 0 {
		charge = baseline
	}
	if status != "completed" {
		if fullGrams <= 0 {
			charge = 0
		} else {
			charge = cents(charge * math.Min(1, grams/fullGrams))
		}
	}
	state := "released"
	if cc != 0 && charge > 0 && !truth(r["wallet_charge_skipped"]) {
		key := str(r["billing_run_id"])
		if key == "" {
			key = fmt.Sprintf("log-%d", runID)
		}
		seen, e := count(tx, "SELECT COUNT(*) FROM wallet_transactions WHERE transaction_type='print_charge' AND print_run_id=?", key)
		if e != nil {
			return e
		}
		if seen == 0 {
			_, e = writeLedger(tx, row{"user_id": r["created_by_id"], "cost_center_id": cc, "transaction_type": "print_charge", "amount": -charge, "description": "Print charge: " + str(r["print_name"]), "print_run_id": key, "print_archive_id": r["archive_id"], "print_queue_id": r["queue_item_id"]})
			if e != nil {
				return e
			}
		}
		state = "consumed"
	}
	_, e = tx.Exec("UPDATE budget_reservations SET status=?,released_at=CURRENT_TIMESTAMP WHERE status='active' AND source_type='print_queue' AND source_id=?", state, qid)
	return e
}

// RecheckQueue validates the original owner's current authority and reprices the selected plate
// without counting the row's existing budget hold a second time.
func RecheckQueue(ctx context.Context, tx *sql.Tx, p access.Principal, queueID int64) error {
	q, e := get(tx, "print_queue", queueID)
	if e != nil {
		return e
	}
	owner := intID(q["created_by_id"])
	if owner != p.ID {
		return denied()
	}
	if !strings.EqualFold(setting(ctx, tx, "billing_enabled", "false"), "true") {
		return nil
	}
	charge, e := authorize(access.With(ctx, p), tx, owner, intID(q["cost_center_id"]), intID(q["archive_id"]), intID(q["library_file_id"]), intID(q["plate_id"]), 1, queueID)
	if e != nil {
		return e
	}
	if _, e = tx.ExecContext(ctx, "UPDATE print_queue SET estimated_cost=? WHERE id=?", charge, queueID); e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "UPDATE budget_reservations SET amount=? WHERE status='active' AND source_type='print_queue' AND source_id=?", charge, queueID)
	return e
}
