package operations

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/management"
)

const reorderEvent = "stock_reorder_alert"
const breakEvent = "stock_break_alert"
const lowEvent = "filament_low"
const forecastState = "stock_alerts_notified"
const lowState = "bambastic_filament_low_notified"

func fleetStock(kind string) bool { return kind == reorderEvent || kind == breakEvent }

func (s *Service) stockLoop(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	var hour time.Time
	relayed := map[string]bool{}
	for {
		at := time.Now()
		due := !at.Before(hour)
		if e := s.checkStock(ctx, at, due, relayed); e != nil && ctx.Err() == nil {
			log.Printf("stock notifications: %v", e)
		}
		if due {
			hour = at.Add(time.Hour)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

type toldForecast struct {
	level             string
	reorder, breaking bool
}

// Persist the tagged release's [SKU, severity, event subscriptions] tuples. A
// portable installation from that release retains the last crossing for each SKU.
func loadForecastAlerts(raw string) map[management.StockSKU]toldForecast {
	m := map[management.StockSKU]toldForecast{}
	var records [][]json.RawMessage
	if json.Unmarshal([]byte(raw), &records) != nil {
		return m
	}
	for _, r := range records {
		var key []string
		var level string
		var events []string
		if len(r) != 3 || json.Unmarshal(r[0], &key) != nil || len(key) != 4 || json.Unmarshal(r[1], &level) != nil || level != "break" && level != "reorder" || json.Unmarshal(r[2], &events) != nil {
			continue
		}
		v := toldForecast{level: level}
		for _, s := range events {
			v.reorder = v.reorder || s == "reorder"
			v.breaking = v.breaking || s == "break"
		}
		m[management.StockSKU(key)] = v
	}
	return m
}
func saveForecastAlerts(m map[management.StockSKU]toldForecast) string {
	keys := make([]management.StockSKU, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		for n := range keys[i] {
			if keys[i][n] != keys[j][n] {
				return keys[i][n] < keys[j][n]
			}
		}
		return false
	})
	rows := [][]any{}
	for _, k := range keys {
		prev := m[k]
		events := []string{}
		if prev.breaking {
			events = append(events, "break")
		}
		if prev.reorder {
			events = append(events, "reorder")
		}
		rows = append(rows, []any{k, prev.level, events})
	}
	b, _ := json.Marshal(rows)
	return string(b)
}
func stockNotice(ctx context.Context, tx *sql.Tx, kind string, printer any, vars obj, providers []obj, at time.Time, skipBreak bool) error {
	var title, body string
	if e := tx.QueryRowContext(ctx, "SELECT title_template,body_template FROM notification_templates WHERE event_type=?", kind).Scan(&title, &body); e != nil {
		return e
	}
	vars["timestamp"] = at.Format("2006-01-02 15:04")
	vars["app_name"] = "Bambastic"
	n := queuedNotice{kind: kind, title: render(title, vars), body: render(body, vars), printerName: str(vars["printer"]), printer: printer, when: at}
	for _, p := range providers {
		if !flag(p["on_"+kind]) || skipBreak && flag(p["on_"+breakEvent]) {
			continue
		}
		if printer != nil && p["printer_id"] != nil && num(p["printer_id"]) != num(printer) {
			continue
		}
		if e := n.enqueue(ctx, tx, p); e != nil {
			return e
		}
	}
	return nil
}
func forecastNotices(ctx context.Context, tx *sql.Tx, at time.Time, forecasts map[management.StockSKU]management.StockForecast, providers []obj) error {
	raw, e := access.Setting(ctx, tx, forecastState, "[]")
	if e != nil {
		return e
	}
	prev := loadForecastAlerts(raw)
	next := map[management.StockSKU]toldForecast{}
	reorder, breaking := false, false
	for _, p := range providers {
		reorder = reorder || flag(p["on_"+reorderEvent])
		breaking = breaking || flag(p["on_"+breakEvent])
	}
	for k, f := range forecasts {
		if f.Level == "" {
			continue
		}
		want := toldForecast{level: f.Level, reorder: reorder, breaking: f.Level == "break" && breaking}
		if !want.reorder && !want.breaking {
			continue
		}
		next[k] = want
		p := prev[k]
		vars := obj{"material": k[0], "subtype": k[1], "brand": k[2], "color": f.DisplayColor, "stock_g": fmt.Sprintf("%.0f", f.Remaining), "rate_g_day": fmt.Sprintf("%.1f", f.Rate), "days_left": f.DaysLeft, "lead_time_days": f.LeadDays}
		if want.breaking && !p.breaking {
			if e = stockNotice(ctx, tx, breakEvent, nil, vars, providers, at, false); e != nil {
				return e
			}
		}
		if want.reorder && !p.reorder {
			if e = stockNotice(ctx, tx, reorderEvent, nil, vars, providers, at, f.Level == "break"); e != nil {
				return e
			}
		}
	}
	if value := saveForecastAlerts(next); value != raw {
		return access.Put(tx, forecastState, value)
	}
	return nil
}

type lowRelayEvent struct {
	key   string
	event obj
}

func lowNotices(ctx context.Context, tx *sql.Tx, at time.Time, src string, slots []management.StockSlot, providers []obj, wanted, online bool, relayed map[string]bool) ([]lowRelayEvent, error) {
	raw, e := access.Setting(ctx, tx, lowState, "{}")
	if e != nil {
		return nil, e
	}
	seen := map[string]bool{}
	if wanted {
		_ = json.Unmarshal([]byte(raw), &seen)
		if seen == nil {
			seen = map[string]bool{}
		}
	}
	var events []lowRelayEvent
	for _, slot := range slots {
		key := fmt.Sprintf("%s/%d/%d/%d/%d", src, slot.PrinterID, slot.AMS, slot.Tray, slot.Spool)
		if slot.Percent >= slot.Threshold {
			delete(seen, key)
			delete(relayed, key)
			continue
		}
		d := obj{"printer": slot.Printer, "printer_name": slot.Printer, "printer_id": slot.PrinterID, "slot": slot.Label, "remaining_percent": int(slot.Percent), "color": slot.Color, "spool_id": slot.Spool}
		if wanted && !seen[key] {
			if e = stockNotice(ctx, tx, lowEvent, slot.PrinterID, d, providers, at, false); e != nil {
				return nil, e
			}
			seen[key] = true
		}
		if online && !relayed[key] {
			events = append(events, lowRelayEvent{key, obj{"type": lowEvent, "printer_id": slot.PrinterID, "data": d}})
		}
	}
	b, _ := json.Marshal(seen)
	if string(b) != raw {
		if e = access.Put(tx, lowState, string(b)); e != nil {
			return nil, e
		}
	}
	return events, nil
}

const stockProviders = `SELECT id,printer_id,on_filament_low,on_stock_reorder_alert,on_stock_break_alert,quiet_hours_enabled,quiet_hours_start,quiet_hours_end,daily_digest_enabled,daily_digest_time
FROM notification_providers WHERE enabled AND (on_filament_low OR on_stock_reorder_alert OR on_stock_break_alert) ORDER BY id`

func (s *Service) checkStock(ctx context.Context, at time.Time, forecastDue bool, relayed map[string]bool) error {
	providers, e := s.list(ctx, stockProviders)
	if e != nil {
		return e
	}
	wanted := func(ps []obj) (low, forecast bool) {
		for _, p := range ps {
			low = low || flag(p["on_"+lowEvent])
			forecast = forecast || flag(p["on_"+reorderEvent]) || flag(p["on_"+breakEvent])
		}
		return
	}
	notify, forecast := wanted(providers)
	enabled, online := false, false
	if s.InventoryRelay != nil {
		enabled, online = s.InventoryRelay.ConnectionStatus()
	}
	if !enabled {
		clear(relayed)
	}
	var snapshot management.StockSnapshot
	loadSlots, loadForecast := notify || online, forecastDue && forecast
	if loadSlots || loadForecast {
		snapshot, e = management.ReadStockSnapshot(ctx, s.db, loadSlots, loadForecast, at)
		if e != nil {
			return e
		}
	}
	when := at
	if z := setting(ctx, s.db, "timezone"); z != "" {
		if zone, e := time.LoadLocation(z); e == nil {
			when = at.In(zone)
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, stockProviders)
	if e != nil {
		return e
	}
	providers, e = httpx.Rows(rows)
	if e != nil {
		return e
	}
	notify, forecast = wanted(providers)
	if forecastDue && (loadForecast || !forecast) {
		if e = forecastNotices(ctx, tx, when, snapshot.Forecasts, providers); e != nil {
			return e
		}
	}
	var relayEvents []lowRelayEvent
	if loadSlots || !notify {
		relayEvents, e = lowNotices(ctx, tx, when, snapshot.Source, snapshot.Slots, providers, notify, online, relayed)
		if e != nil {
			return e
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if s.InventoryRelay != nil {
		for _, v := range relayEvents {
			if s.InventoryRelay.TryPublish(v.event) {
				relayed[v.key] = true
			}
		}
	}
	if len(providers) > 0 {
		s.signal()
	}
	return nil
}
