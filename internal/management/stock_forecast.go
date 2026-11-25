package management

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

type StockSKU [4]string // material, subtype, brand, color name

type StockForecast struct {
	SKU                           StockSKU
	Level                         string // "", "reorder", "break"
	DisplayColor                  string
	Remaining, Rate, ReorderPoint float64
	DaysLeft, LeadDays            int
}

type StockSlot struct {
	PrinterID, AMS, Tray, Spool int64
	Printer, Label, Color       string
	Percent, Threshold          float64
}

type StockSnapshot struct {
	Source    string
	Slots     []StockSlot
	Forecasts map[StockSKU]StockForecast
}

func skuOf(v row) StockSKU {
	return StockSKU{str(v["material"]), str(v["subtype"]), str(v["brand"]), str(v["color_name"])}
}
func stockTime(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t.UTC()
	}
	for _, fmt := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999Z07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999", "2006-01-02"} {
		if t, e := time.Parse(fmt, str(v)); e == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

type stockUse struct {
	at    time.Time
	grams float64
}

func historyRate(days map[int64]float64, now time.Time) (float64, float64, bool) {
	if len(days) < 2 {
		return 0, 0, false
	}
	dates := make([]int64, 0, len(days))
	for day := range days {
		dates = append(dates, day)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i] < dates[j] })
	total, mean, spread := 0., 0., 0.
	for i := 1; i < len(dates); i++ {
		rate := days[dates[i]] / float64(dates[i]-dates[i-1])
		age := now.Sub(time.Unix(dates[i]*86400, 0)).Hours() / 24
		w := math.Exp(-math.Ln2 * age / 30)
		if w <= 0 {
			continue
		}
		next := total + w
		delta := rate - mean
		mean += delta * w / next
		spread += w * delta * (rate - mean)
		total = next
	}
	if total <= 0 {
		return 0, 0, false
	}
	return mean, math.Sqrt(max(0, spread/total)), true
}

func deltaRate(spools []row, firstUse map[int64]time.Time, usedBefore bool, now time.Time) float64 {
	const day = 24 * time.Hour
	startWindow := now.Add(-90 * day)
	first := now
	type span struct {
		from, to time.Time
		g        float64
	}
	var spans []span
	for _, sp := range spools {
		g := num(sp["weight_used"]) - num(sp["weight_used_baseline"])
		if g <= 0 {
			continue
		}
		end := stockTime(sp["last_used"])
		if end.IsZero() {
			end = stockTime(sp["archived_at"])
		}
		if end.IsZero() || end.After(now) {
			end = now
		}
		from := stockTime(sp["first_used"])
		if from.IsZero() {
			from = firstUse[intID(sp["id"])]
		}
		if from.IsZero() {
			from = stockTime(sp["created_at"])
		}
		if from.IsZero() || from.After(end) {
			from = end
		}
		spans = append(spans, span{from, end, g})
		if from.Before(first) {
			first = from
		}
	}
	if !usedBefore && first.After(startWindow) {
		startWindow = first
	}
	period := now.Sub(startWindow)
	if period < day {
		return 0
	}
	var sum float64
	for _, s := range spans {
		elapsed := s.to.Sub(s.from)
		if elapsed < day {
			if !s.to.Before(startWindow) {
				sum += s.g
			}
			continue
		}
		from := s.from
		if startWindow.After(from) {
			from = startWindow
		}
		inside := s.to.Sub(from)
		if inside > 0 {
			sum += s.g * float64(inside) / float64(elapsed)
		}
	}
	return sum * float64(day) / float64(period)
}

func forecastStock(spools []row, usage map[int64][]stockUse, settings map[StockSKU]row, lead int, remote bool, now time.Time) map[StockSKU]StockForecast {
	type group struct {
		rolls          []row
		remaining      float64
		representative row
		old            bool
	}
	groups := map[StockSKU]*group{}
	for _, sp := range spools {
		key := skuOf(sp)
		g := groups[key]
		if g == nil {
			g = &group{}
			groups[key] = g
		}
		if sp["archived_at"] == nil {
			if g.representative == nil {
				g.representative = sp
			}
			g.remaining += max(0, num(sp["label_weight"])-num(sp["weight_used"]))
		} else if stockTime(sp["archived_at"]).Before(now.AddDate(0, 0, -90)) {
			g.old = g.old || num(sp["weight_used"]) > 0
			continue
		}
		g.rolls = append(g.rolls, sp)
	}
	out := map[StockSKU]StockForecast{}
	for key, g := range groups {
		if g.representative == nil {
			continue
		}
		conf := settings[key]
		if conf == nil {
			fallback := key
			fallback[3] = ""
			conf = settings[fallback]
		}
		r := StockForecast{SKU: key, DisplayColor: key[3], Remaining: g.remaining, LeadDays: max(lead, int(num(conf["lead_time_days"]))), DaysLeft: -1}
		if truth(g.representative["color_name_is_synthesized"]) {
			r.DisplayColor = ""
		}
		days := map[int64]float64{}
		earliest := map[int64]time.Time{}
		for _, sp := range g.rolls {
			id := intID(sp["id"])
			created := stockTime(sp["created_at"])
			for _, v := range usage[id] {
				if v.at.IsZero() || remote && v.at.Before(created) {
					continue
				}
				if first := earliest[id]; first.IsZero() || v.at.Before(first) {
					earliest[id] = v.at
				}
				if num(sp["weight_used_baseline"]) == 0 {
					days[v.at.Unix()/86400] += v.grams
				}
			}
		}
		rate, sigma, ok := historyRate(days, now)
		if !ok {
			rate = deltaRate(g.rolls, earliest, g.old, now)
			sigma = rate * 0.2
		}
		r.Rate = rate
		if rate > 0 {
			margin := 14. * rate
			if conf != nil {
				margin = num(conf["safety_margin_value"])
				if conf["safety_margin_unit"] != "g" {
					margin *= rate
				}
			}
			r.ReorderPoint = rate*float64(r.LeadDays) + 1.65*sigma*math.Sqrt(float64(r.LeadDays)) + margin
			r.DaysLeft = int(math.Floor(r.Remaining / rate))
			if !truth(conf["alerts_snoozed"]) {
				switch {
				case r.LeadDays > 0 && r.DaysLeft <= r.LeadDays:
					r.Level = "break"
				case math.Floor((r.Remaining-r.ReorderPoint)/rate) <= 0:
					r.Level = "reorder"
				}
			}
		}
		out[key] = r
	}
	return out
}

func stockSlotLabel(ams, tray int64, model string) string {
	switch {
	case ams == 255 || ams == 254:
		if model == "H3C" || model == "N10" || tray >= 2 {
			return fmt.Sprintf("Ext-%d", 256-slotAddress(255, int(tray)))
		}
		if tray == 1 {
			return "Ext-R"
		}
		return "Ext-L"
	case ams >= 128:
		return fmt.Sprintf("HT-%c", 'A'+ams-128)
	case ams == 6 || ams == 16:
		return fmt.Sprintf("Lite-%d", tray+1)
	case ams == 32:
		return fmt.Sprintf("3H-%d", tray+1)
	default:
		return fmt.Sprintf("%c%d", 'A'+ams, tray+1)
	}
}

func stockSource(ctx context.Context, db access.Query) (string, error) {
	on, e := access.Setting(ctx, db, "spoolman_enabled", "false")
	if e != nil {
		return "", e
	}
	if !strings.EqualFold(on, "true") {
		return "native", nil
	}
	url, e := access.Setting(ctx, db, "spoolman_url", "")
	if e != nil {
		return "", e
	}
	dis, e := access.Setting(ctx, db, "bambastic_spoolman_disconnected", "false")
	if e != nil {
		return "", e
	}
	if url == "" || strings.EqualFold(dis, "true") {
		return "", errors.New("Spoolman unavailable")
	}
	return strings.TrimRight(url, "/"), nil
}

// ReadStockSnapshot uses the same stock identities and adjusted weights as the
// inventory page. Spoolman is contacted before opening the database transaction.
func ReadStockSnapshot(ctx context.Context, db *sql.DB, slots, forecasts bool, now time.Time) (StockSnapshot, error) {
	var out StockSnapshot
	source, e := stockSource(ctx, db)
	if e != nil {
		return out, e
	}
	out.Source = source
	remote := source != "native"
	var rolls []row
	if remote {
		needed := forecasts
		if slots && !needed {
			e = db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM spoolman_slot_assignments)").Scan(&needed)
			if e != nil {
				return out, e
			}
		}
		if needed {
			var raw []row
			path := "spool"
			if forecasts {
				path += "?allow_archived=true"
			}
			if e = remoteCall(ctx, source, "GET", path, nil, &raw); e != nil {
				return out, e
			}
			for _, v := range raw {
				if intID(v["id"]) > 0 {
					rolls = append(rolls, remoteSpool(v))
				}
			}
		}
	}
	tx, e := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if e != nil {
		return out, e
	}
	defer tx.Rollback()
	unchanged, e := stockSource(ctx, tx)
	if e != nil {
		return out, e
	}
	if unchanged != source {
		return out, errors.New("inventory source changed")
	}
	if !remote {
		where := ""
		if !forecasts {
			where = " WHERE id IN(SELECT spool_id FROM spool_assignment) AND archived_at IS NULL"
		}
		rolls, e = list(tx, `SELECT id,material,subtype,brand,color_name,label_weight,weight_used,weight_used_baseline,created_at,archived_at,last_used,low_stock_threshold_pct FROM spool`+where+" ORDER BY id")
		if e != nil {
			return out, e
		}
	}
	if slots {
		table, col := "spool_assignment", "spool_id"
		if remote {
			table, col = "spoolman_slot_assignments", "spoolman_spool_id"
		}
		bound, e := list(tx, "SELECT a.printer_id,a.ams_id,a.tray_id,a."+col+" spool_id,p.name,p.model FROM "+table+" a JOIN printers p ON a.printer_id=p.id ORDER BY a.printer_id,a.ams_id,a.tray_id")
		if e != nil {
			return out, e
		}
		byID := map[int64]row{}
		for _, sp := range rolls {
			byID[intID(sp["id"])] = sp
		}
		threshold, e := strconv.ParseFloat(setting(ctx, tx, "low_stock_threshold", "20"), 64)
		if e != nil || !(threshold > 0 && threshold <= 100) {
			threshold = 20
		}
		for _, b := range bound {
			id := intID(b["spool_id"])
			sp := byID[id]
			net := num(sp["label_weight"])
			if net <= 0 || sp["archived_at"] != nil {
				continue
			}
			pct := 100 * max(0, net-num(sp["weight_used"])) / net
			th := num(sp["low_stock_threshold_pct"])
			if th <= 0 {
				th = threshold
			}
			a, t := int64(num(b["ams_id"])), int64(num(b["tray_id"]))
			out.Slots = append(out.Slots, StockSlot{PrinterID: intID(b["printer_id"]), AMS: a, Tray: t, Spool: id, Printer: str(b["name"]), Label: stockSlotLabel(a, t, strings.ToUpper(str(b["model"]))), Color: str(sp["color_name"]), Percent: pct, Threshold: th})
		}
	}
	if forecasts {
		col, filter := "spool_id", "spoolman_spool_id IS NULL"
		if remote {
			col, filter = "spoolman_spool_id", "spoolman_spool_id IS NOT NULL"
		}
		h, e := list(tx, "SELECT "+col+" spool_id,weight_used,created_at FROM spool_usage_history WHERE "+filter+" ORDER BY created_at DESC,id DESC LIMIT 5000")
		if e != nil {
			return out, e
		}
		history := map[int64][]stockUse{}
		for _, v := range h {
			if id := intID(v["spool_id"]); id > 0 {
				history[id] = append(history[id], stockUse{stockTime(v["created_at"]), num(v["weight_used"])})
			}
		}
		rows, e := list(tx, "SELECT material,subtype,brand,color_name,lead_time_days,safety_margin_value,safety_margin_unit,alerts_snoozed FROM filament_sku_settings")
		if e != nil {
			return out, e
		}
		conf := map[StockSKU]row{}
		for _, v := range rows {
			conf[skuOf(v)] = v
		}
		lead, _ := strconv.Atoi(setting(ctx, tx, "forecast_global_lead_time_days", "0"))
		out.Forecasts = forecastStock(rolls, history, conf, max(0, lead), remote, now.UTC())
	}
	return out, tx.Commit()
}
