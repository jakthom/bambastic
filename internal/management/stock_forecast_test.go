package management

import (
	"math"
	"testing"
	"time"
)

func TestForecastMatchesPanelHistoryAndReset(t *testing.T) {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	ago := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	key := StockSKU{"PLA", "Matte", "Example", "Blue"}
	roll := func(id, used, age int) row {
		return row{"id": id, "material": key[0], "subtype": key[1], "brand": key[2], "color_name": key[3], "label_weight": 1000, "weight_used": used, "created_at": ago(age)}
	}
	a, b := roll(1, 600, 60), roll(2, 0, 10)
	h := map[int64][]stockUse{1: {{ago(40), 120}, {ago(25), 90}, {ago(12), 200}, {ago(3), 150}}}
	f := forecastStock([]row{a, b}, h, nil, 7, false, now)[key]
	if math.Abs(f.Rate-13.57710201710711) > 1e-9 || math.Abs(f.ReorderPoint-304.32790456992615) > 1e-9 || f.DaysLeft != 103 || f.Remaining != 1400 || f.Level != "" {
		t.Fatalf("history: %+v", f)
	}
	a, b = roll(1, 700, 50), roll(2, 300, 30)
	a["weight_used_baseline"] = 200
	h = map[int64][]stockUse{1: {{ago(20), 400}, {ago(10), 50}}, 2: {{ago(20), 100}, {ago(6), 100}, {ago(1), 100}}}
	f = forecastStock([]row{a, b}, h, nil, 5, false, now)[key]
	if math.Abs(f.Rate-13.942344991570476) > 1e-9 || math.Abs(f.ReorderPoint-288.583334452561) > 1e-9 || f.DaysLeft != 71 {
		t.Fatalf("per-spool reset: %+v", f)
	}
	a, b = roll(1, 1000, 120), roll(2, 1000, 120)
	c := roll(3, 100, 120)
	a["first_used"], a["archived_at"] = ago(119), ago(101)
	b["first_used"], b["archived_at"] = ago(61), ago(21)
	c["first_used"], c["last_used"] = ago(21), ago(2)
	f = forecastStock([]row{a, b, c}, nil, nil, 0, false, now)[key]
	if math.Abs(f.Rate-1100./90) > 1e-9 || f.Remaining != 900 {
		t.Fatalf("archived demand window: %+v", f)
	}
	if len(forecastStock([]row{a, b}, nil, nil, 0, false, now)) != 0 {
		t.Fatal("archived-only SKU alerted")
	}
	d := roll(4, 950, 10)
	d["first_used"] = ago(10)
	h = map[int64][]stockUse{4: {{ago(20), 1}, {ago(11), 1}}}
	f = forecastStock([]row{d}, h, nil, 5, true, now)[key]
	if f.Rate != 95 || f.Level != "break" {
		t.Fatalf("reused Spoolman id: %+v", f)
	}
	base := key
	base[3] = ""
	settings := map[StockSKU]row{base: {"lead_time_days": 7, "safety_margin_value": 5, "safety_margin_unit": "g", "alerts_snoozed": true}}
	f = forecastStock([]row{d}, h, settings, 8, true, now)[key]
	if f.Level != "" || f.LeadDays != 8 {
		t.Fatalf("colorless snooze or global lead: %+v", f)
	}
	settings[key] = row{"lead_time_days": 9, "safety_margin_value": 0, "safety_margin_unit": "g"}
	f = forecastStock([]row{d}, h, settings, 8, true, now)[key]
	if f.Level != "break" || f.LeadDays != 9 {
		t.Fatalf("color-specific settings: %+v", f)
	}
}

func TestStockSpoolmanProductIdentity(t *testing.T) {
	for _, ex := range []struct {
		material, name, finish, color string
		synth                         bool
	}{
		{"PLA", "Matte Scarlet Red", "Matte", "Scarlet Red", false},
		{"PLA", "Gold - Basic PLA", "Basic", "Gold", false},
		{"PLA", "Silk+ Baby Blue", "Silk+", "Baby Blue", false},
		{"PLA", "PLA Engineering", "Engineering", "Engineering", true},
		{"PLA Matte", "PLA Matte Grey", "", "Grey", false},
		{"PETG", "High Speed Teal", "High Speed", "Teal", false},
	} {
		v := remoteSpool(row{"id": 1, "filament": row{"material": ex.material, "name": ex.name}})
		if str(v["subtype"]) != ex.finish || str(v["color_name"]) != ex.color || truth(v["color_name_is_synthesized"]) != ex.synth {
			t.Errorf("%s: %v", ex.name, v)
		}
	}
}
