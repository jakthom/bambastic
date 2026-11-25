package production

import (
	"bambastic/internal/access"
	"bambastic/internal/management"
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"
)

type usagePrices struct {
	enabled, disconnected bool
	base                  string
	rates                 map[int64]float64
}

func currentPriceSource(ctx context.Context, db access.Query) (usagePrices, error) {
	var s usagePrices
	on, e := access.Setting(ctx, db, "spoolman_enabled", "false")
	if e != nil {
		return s, e
	}
	s.enabled = strings.EqualFold(on, "true")
	if !s.enabled {
		return s, nil
	}
	disconnected, e := access.Setting(ctx, db, "bambastic_spoolman_disconnected", "false")
	if e != nil {
		return s, e
	}
	s.disconnected = strings.EqualFold(disconnected, "true")
	s.base, e = access.Setting(ctx, db, "spoolman_url", "")
	return s, e
}

type repriceCount struct{ updated, preserved, changed, unreachable int }

func (s *Service) reprice(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "archives:update_all"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	actor := access.From(ctx)
	cfg, e := currentPriceSource(ctx, s.db)
	if e != nil {
		return nil, e
	}
	if cfg.enabled && !cfg.disconnected && cfg.base != "" {
		var anyUsage bool
		if e = s.db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM spool_usage_history WHERE spoolman_spool_id IS NOT NULL)").Scan(&anyUsage); e != nil {
			return nil, e
		}
		if anyUsage {
			cfg.rates, _ = management.SpoolmanRates(ctx, cfg.base)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	current, e := currentPriceSource(ctx, tx)
	if e != nil {
		return nil, e
	}
	if current.enabled != cfg.enabled || current.base != cfg.base || current.disconnected != cfg.disconnected {
		return nil, fail(409, "Spoolman settings changed; retry the recalculation")
	}
	text, e := access.Setting(ctx, tx, "default_filament_cost", "25")
	if e != nil {
		return nil, e
	}
	def, ok := decimal(text)
	if !ok || def < 0 {
		def = 25
	}
	archives, e := records(ctx, tx, "SELECT id,printer_id,cost,filament_type,filament_used_grams FROM print_archives")
	if e != nil {
		return nil, e
	}
	allRuns, e := records(ctx, tx, "SELECT id,archive_id,printer_id,status,started_at,completed_at,cost,filament_used_grams FROM print_log_entries ORDER BY id")
	if e != nil {
		return nil, e
	}
	runs := map[int64][]row{}
	for _, run := range allRuns {
		k := num(run["archive_id"])
		runs[k] = append(runs[k], run)
	}
	allUsage, e := records(ctx, tx, `SELECT h.*,s.id current_spool,s.cost_per_kg,a.run_id charged_run FROM spool_usage_history h LEFT JOIN spool s ON s.id=h.spool_id LEFT JOIN bambastic_material_allocations a ON a.usage_id=h.id ORDER BY h.id`)
	if e != nil {
		return nil, e
	}
	usage := map[int64][]row{}
	for _, v := range allUsage {
		k := num(v["archive_id"])
		usage[k] = append(usage[k], v)
	}
	filaments, e := records(ctx, tx, "SELECT type,cost_per_kg FROM filaments ORDER BY id")
	if e != nil {
		return nil, e
	}
	catalog := map[string]float64{}
	for _, f := range filaments {
		catalog[val(f["type"])], _ = decimal(f["cost_per_kg"])
	}
	out := new(repriceCount)
	for _, archive := range archives {
		k := num(archive["id"])
		visible := scope(actor, num(archive["printer_id"]))
		for _, r := range runs[k] {
			visible = visible && scope(actor, num(r["printer_id"]))
		}
		if !visible {
			continue
		}
		if e = out.archive(ctx, tx, archive, runs[k], usage[k], catalog, cfg, def); e != nil {
			return nil, e
		}
	}
	if actor.PrinterIDs == nil && len(usage[0]) > 0 {
		if e = out.archive(ctx, tx, nil, runs[0], usage[0], catalog, cfg, def); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	msg := fmt.Sprintf("Recalculated costs for %d archives", out.updated)
	if out.preserved > 0 {
		msg += fmt.Sprintf("; kept %d priced from Spoolman", out.preserved)
	}
	if out.unreachable > 0 {
		msg += fmt.Sprintf("; Spoolman not reachable, %d usage records kept their price", out.unreachable)
	}
	return row{"updated": out.updated, "preserved": out.preserved, "usage_repriced": out.changed, "spoolman_unreachable": out.unreachable, "message": msg}, nil
}
func sameMoney(a, b any) bool {
	x, ok := decimal(a)
	y, valid := decimal(b)
	return ok == valid && x == y
}
func useCost(value any, grams, def float64, remote bool) float64 {
	if cost, ok := decimal(value); ok {
		return cost
	}
	if remote {
		return math.RoundToEven(grams*def/10) / 100
	}
	return 0
}

// Legacy usage was linked to an archive and printer, without a run ID. Match
// only a run whose start/end window covers that row's completion timestamp.
func runForUsage(u row, byID map[int64]row, ordered []row) row {
	if run := byID[num(u["charged_run"])]; run != nil {
		return run
	}
	t := reportDate(u["created_at"])
	pid := num(u["printer_id"])
	if t.IsZero() || pid == 0 && u["archive_id"] == nil {
		return nil
	}
	var candidate row
	for _, r := range ordered {
		if pid != 0 && num(r["printer_id"]) != pid {
			continue
		}
		start := reportDate(r["started_at"])
		if start.IsZero() {
			continue
		}
		if start.After(t) {
			break
		}
		end := reportDate(r["completed_at"])
		if end.IsZero() || !t.After(end.Add(30*time.Minute)) {
			candidate = r
		} else {
			candidate = nil
		}
	}
	return candidate
}
func (c *repriceCount) archive(ctx context.Context, tx *sql.Tx, archive row, runs, usage []row, catalog map[string]float64, source usagePrices, def float64) error {
	byID := map[int64]row{}
	var latest row
	printed := 0.0
	for _, r := range runs {
		byID[num(r["id"])] = r
		latest = r
		g, _ := decimal(r["filament_used_grams"])
		printed += g
	}
	ordered := append([]row(nil), runs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		return reportDate(ordered[i]["started_at"]).Before(reportDate(ordered[j]["started_at"]))
	})
	owners := map[int64]row{}
	follows := archive != nil && latest != nil && latest["status"] == "completed" && (latest["cost"] == nil || sameMoney(latest["cost"], archive["cost"]))
	for _, u := range usage {
		r := runForUsage(u, byID, ordered)
		owners[num(u["id"])] = r
		if r == nil || r["id"] != latest["id"] {
			follows = false
		}
	}
	tracked, total := 0.0, 0.0
	priced, dirty := map[int64]bool{}, map[int64]bool{}
	for _, u := range usage {
		g, _ := decimal(u["weight_used"])
		cost := u["cost"]
		var rate float64
		known := false
		remote := u["spool_id"] == nil && u["spoolman_spool_id"] != nil
		r := owners[num(u["id"])]
		own := r != nil && (!follows || r["id"] != latest["id"])
		rid := num(r["id"])
		if own {
			priced[rid] = true
		}
		switch {
		case u["current_spool"] != nil:
			rate, known = decimal(u["cost_per_kg"])
			if !known {
				rate = def
			}
			rate /= 1000
			known = true
		case remote && source.enabled:
			if source.rates == nil {
				c.unreachable++
			} else {
				rate, known = source.rates[num(u["spoolman_spool_id"])]
			}
		}
		if known {
			var next any
			if rate > 0 {
				next = math.RoundToEven(g*rate*100) / 100
			}
			if !sameMoney(cost, next) {
				if _, e := tx.ExecContext(ctx, "UPDATE spool_usage_history SET cost=? WHERE id=?", next, u["id"]); e != nil {
					return e
				}
				c.changed++
				delta := useCost(next, g, def, remote) - useCost(cost, g, def, remote)
				if own && delta != 0 {
					old, _ := decimal(r["cost"])
					r["cost"] = math.RoundToEven(max(0, old+delta)*100) / 100
					dirty[rid] = true
				}
				cost = next
			}
		}
		if n, ok := decimal(cost); ok {
			tracked += g
			total += n
		}
	}
	for k := range dirty {
		if _, e := tx.ExecContext(ctx, "UPDATE print_log_entries SET cost=? WHERE id=?", byID[k]["cost"], k); e != nil {
			return e
		}
	}
	if archive == nil {
		return nil
	}
	g, _ := decimal(archive["filament_used_grams"])
	var price float64
	switch {
	case total > 0 && tracked > 0:
		if g <= 0 {
			price = total / float64(max(1, len(runs)))
		} else {
			if printed <= 0 {
				printed = g
			}
			covered := g * min(1, tracked/printed)
			price = covered*total/tracked + (g-covered)*def/1000
		}
	case source.enabled && archive["cost"] != nil:
		c.preserved++
		return nil
	case g > 0 && val(archive["filament_type"]) != "":
		rate, ok := catalog[strings.TrimSpace(strings.Split(val(archive["filament_type"]), ",")[0])]
		if !ok {
			rate = def
		}
		price = g * rate / 1000
	default:
		return nil
	}
	price = math.Round(price*100) / 100
	if sameMoney(archive["cost"], price) {
		return nil
	}
	if _, e := tx.ExecContext(ctx, "UPDATE print_archives SET cost=? WHERE id=?", price, archive["id"]); e != nil {
		return e
	}
	if latest != nil && !priced[num(latest["id"])] && latest["status"] == "completed" && (latest["cost"] == nil || sameMoney(latest["cost"], archive["cost"])) {
		if _, e := tx.ExecContext(ctx, "UPDATE print_log_entries SET cost=? WHERE id=?", price, latest["id"]); e != nil {
			return e
		}
	}
	c.updated++
	return nil
}
