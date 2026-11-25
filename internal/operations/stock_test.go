package operations

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"bambastic/internal/access"
)

var forecastNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func stockPrinterAndSpool(f rig) {
	f.printer()
	f.exec("INSERT INTO spool(id,material,subtype,brand,color_name,label_weight,core_weight,weight_used,weight_locked,low_stock_threshold_pct,created_at) VALUES(1,'PLA','Matte','Example','Teal',1000,250,980,0,5,'2026-05-01')")
	f.exec("INSERT INTO spool_assignment(spool_id,printer_id,ams_id,tray_id) VALUES(1,1,0,1)")
	f.exec("INSERT INTO spool_usage_history(spool_id,weight_used,percent_used,status,created_at) VALUES(1,10,1,'completed','2026-06-13'),(1,10,1,'completed','2026-06-14')")
	f.exec("INSERT INTO filament_sku_settings(material,subtype,brand,color_name,lead_time_days,safety_margin_value,safety_margin_unit,alerts_snoozed) VALUES('PLA','Matte','Example',NULL,3,14,'days',0)")
}
func stockDestination(f rig, dest, name string, flags obj) {
	flags["name"] = name
	flags["provider_type"] = "webhook"
	flags["config"] = obj{"webhook_url": dest}
	flags["enabled"] = true
	b, e := json.Marshal(flags)
	if e != nil {
		f.t.Fatal(e)
	}
	f.call(access.Principal{Admin: true}, "POST", "notifications/", string(b), 200)
}

func TestStockDeliveryLevelsScopeRecoveryAndRestart(t *testing.T) {
	f := setup(t)
	stockPrinterAndSpool(f)
	f.exec("INSERT INTO printers(id,name,serial_number,ip_address,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear) VALUES(2,'Other','second','127.0.0.1','code',1,1,1,0,0,0,0,0,0)")
	var mu sync.Mutex
	received := map[string][]string{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload obj
		if e := json.NewDecoder(r.Body).Decode(&payload); e != nil {
			t.Error(e)
			return
		}
		event := str(payload["event"])
		switch event {
		case lowEvent:
			if str(payload["message"]) != "Hana: Slot A2 at 2%" {
				t.Errorf("low message: %s", payload["message"])
			}
		case breakEvent, reorderEvent:
			if m := str(payload["message"]); !strings.Contains(m, "PLA Matte Teal (Example)") || !strings.Contains(m, "Rate: 10.0g/day") {
				t.Errorf("forecast message: %s", m)
			}
		default:
			t.Error("unexpected delivery", event)
		}
		mu.Lock()
		received[r.URL.Path] = append(received[r.URL.Path], event)
		mu.Unlock()
		w.WriteHeader(204)
	}))
	defer h.Close()
	stockDestination(f, h.URL+"/both", "Both", obj{"on_stock_break_alert": true, "on_stock_reorder_alert": true, "on_filament_low": true, "printer_id": 1})
	stockDestination(f, h.URL+"/other", "Other", obj{"on_stock_reorder_alert": true, "on_filament_low": true, "printer_id": 2})
	relay := map[string]bool{}
	check := func(s *Service) {
		t.Helper()
		if e := s.checkStock(t.Context(), forecastNow, true, relay); e != nil {
			t.Fatal(e)
		}
		if e := s.process(t.Context()); e != nil {
			t.Fatal(e)
		}
	}
	assert := func(first, second []string) {
		t.Helper()
		mu.Lock()
		defer mu.Unlock()
		if !reflect.DeepEqual(received["/both"], first) || !reflect.DeepEqual(received["/other"], second) {
			t.Fatalf("destinations: %v; want %v / %v", received, first, second)
		}
	}
	check(f.s)
	assert([]string{breakEvent, lowEvent}, []string{reorderEvent})
	restarted, e := New(f.s.db)
	if e != nil {
		t.Fatal(e)
	}
	check(restarted)
	assert([]string{breakEvent, lowEvent}, []string{reorderEvent})
	f.exec("UPDATE spool SET weight_used=950") // Exactly the slot threshold; re-arms low. Break eases to reorder.
	check(restarted)
	assert([]string{breakEvent, lowEvent}, []string{reorderEvent})
	f.exec("UPDATE spool SET weight_used=980")
	check(restarted)
	assert([]string{breakEvent, lowEvent, breakEvent, lowEvent}, []string{reorderEvent})
	f.exec("UPDATE filament_sku_settings SET alerts_snoozed=1")
	check(restarted)
	f.exec("UPDATE filament_sku_settings SET alerts_snoozed=0")
	check(restarted)
	assert([]string{breakEvent, lowEvent, breakEvent, lowEvent, breakEvent}, []string{reorderEvent, reorderEvent})
	f.exec("UPDATE spool SET weight_used=0")
	check(restarted)
	f.exec("UPDATE spool SET weight_used=950")
	check(restarted)
	assert([]string{breakEvent, lowEvent, breakEvent, lowEvent, breakEvent, reorderEvent}, []string{reorderEvent, reorderEvent, reorderEvent})
}

type stockRelay struct {
	enabled, online, accept bool
	events                  []obj
}

func (r *stockRelay) ConnectionStatus() (bool, bool) { return r.enabled, r.online }
func (r *stockRelay) TryPublish(v map[string]any) bool {
	if !r.accept {
		return false
	}
	r.events = append(r.events, v)
	return true
}

func TestStockSpoolmanOutageAndRelay(t *testing.T) {
	f := setup(t)
	stockPrinterAndSpool(f)
	f.exec("INSERT INTO spoolman_slot_assignments(printer_id,ams_id,tray_id,spoolman_spool_id) VALUES(1,0,0,1)")
	f.exec("INSERT INTO spool_usage_history(spoolman_spool_id,weight_used,percent_used,status,created_at) VALUES(1,900,90,'completed','2026-05-02'),(1,50,5,'completed','2026-06-12'),(1,20,2,'completed','2026-06-14')")
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_enabled','true'),('forecast_global_lead_time_days','3')")
	var count atomic.Int32
	var failing atomic.Bool
	var remaining atomic.Int32
	remaining.Store(30)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/spool" {
			t.Errorf("bad path %s", r.URL)
			return
		}
		count.Add(1)
		var n int
		if e := f.s.db.QueryRowContext(r.Context(), "SELECT 1").Scan(&n); e != nil {
			t.Errorf("database held during HTTP request: %v", e)
			return
		}
		if failing.Load() {
			w.WriteHeader(503)
			return
		}
		fmt.Fprintf(w, `[{"id":1,"registered":"2026-06-01","initial_weight":1000,"remaining_weight":%d,"used_weight":%d,"filament":{"name":"Matte Scarlet Red","material":"PLA","vendor":{"name":"Example"}}}]`, remaining.Load(), 1000-remaining.Load())
	}))
	defer h.Close()
	f.s.db.SetMaxOpenConns(1)
	f.exec("INSERT INTO settings(key,value) VALUES('spoolman_url',?)", h.URL)
	relay := &stockRelay{}
	f.s.InventoryRelay = relay
	seen := map[string]bool{}
	check := func() {
		t.Helper()
		if e := f.s.checkStock(t.Context(), forecastNow, true, seen); e != nil {
			t.Fatal(e)
		}
	}
	check()
	if count.Load() != 0 {
		t.Fatal("spool query while no subscribers/relay")
	}
	relay.enabled, relay.online = true, true
	check()
	if len(relay.events) != 0 || count.Load() != 1 {
		t.Fatal("rejected relay event marked delivered")
	}
	relay.accept = true
	check()
	if len(relay.events) != 1 || sub(relay.events[0]["data"])["slot"] != "A1" || sub(relay.events[0]["data"])["remaining_percent"] != 3 {
		t.Fatal(relay.events)
	}
	relay.online = false
	check()
	relay.online = true
	check()
	if len(relay.events) != 1 {
		t.Fatal("duplicate on reconnect")
	}
	stockDestination(f, "http://127.0.0.1", "Events", obj{"on_filament_low": true, "on_stock_break_alert": true, "on_stock_reorder_alert": true})
	check()
	assertDeliveries := func(want int) {
		t.Helper()
		var n int
		if e := f.s.db.QueryRow("SELECT COUNT(*) FROM bambastic_notification_delivery").Scan(&n); e != nil || n != want {
			t.Fatalf("deliveries=%d want=%d err=%v", n, want, e)
		}
	}
	assertDeliveries(2)
	var msg string
	if e := f.s.db.QueryRow("SELECT message FROM bambastic_notification_delivery WHERE event_type=?", breakEvent).Scan(&msg); e != nil || !strings.Contains(msg, "Matte Scarlet Red") || !strings.Contains(msg, "Rate: 10.0g/day") {
		t.Fatalf("Spoolman rate/scope: %s %v", msg, e)
	}
	failing.Store(true)
	if e := f.s.checkStock(t.Context(), forecastNow, true, seen); e == nil {
		t.Fatal("remote outage treated as a successful empty inventory")
	}
	failing.Store(false)
	check()
	if len(relay.events) != 1 {
		t.Fatal("relay duplicated after outage")
	}
	assertDeliveries(2)
	remaining.Store(1000)
	check()
	remaining.Store(30)
	check()
	assertDeliveries(4)
	if len(relay.events) != 2 {
		t.Fatal("relay did not re-arm after refill")
	}
}
