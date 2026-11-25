package management

import (
	"bambastic/internal/access"
	"bambastic/internal/testdb"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"math"
	"net/http"
	"testing"
)

func TestStationScanPrefillsQuickAddAndLinksBothStickers(t *testing.T) {
	db := testdb.Open(t)
	svc, e := New(db)
	if e != nil {
		t.Fatal(e)
	}
	mux := http.NewServeMux()
	svc.Register(mux)
	f := fixture{t, db, mux}
	var event row
	svc.SetEventSink(func(v map[string]any) { event = v })
	admin := access.Principal{Admin: true}
	scan := func(v row, status int) row {
		t.Helper()
		raw, e := json.Marshal(v)
		if e != nil {
			t.Fatal(e)
		}
		return f.call(admin, "POST", "spoolbuddy/nfc/tag-scanned", string(raw), status).(map[string]any)
	}
	code := func(s string) string { p := make([]byte, 16); copy(p, s); return hex.EncodeToString(p) }
	b5 := make([]byte, 16)
	copy(b5, []byte{12, 44, 204, 255})
	binary.LittleEndian.PutUint16(b5[4:], 750)
	binary.LittleEndian.PutUint32(b5[8:], math.Float32bits(1.75))
	uuid := "01020304050607080102030405060708"
	body := row{"device_id": "desk", "tag_uid": "01234567", "raw_blocks": map[string]string{"1": code("A00-B08\000GFA00"), "2": code("PLA"), "4": code("PLA Basic"), "5": hex.EncodeToString(b5), "9": uuid}}
	scan(body, 200)
	data, ok := event["tag_data"].(row)
	if !ok || data["material"] != "PLA" || data["rgba"] != "0C2CCCFF" || num(data["label_weight"]) != 750 || num(data["diameter"]) != 1.75 {
		t.Fatal(event)
	}
	for _, v2 := range []bool{false, true} {
		p := ndef(row{"material": "PETG", "subtype": "Matte", "brand": "Workshop", "color_name": "Aqua", "rgba": "4499AAFF", "label_weight": 800, "core_weight": 210}, v2)
		scan(row{"device_id": "desk", "tag_uid": "01234568", "opentag3d_ndef": hex.EncodeToString(p)}, 200)
		v := event["tag_data"].(row)
		if v["brand"] != "Workshop" || v["material"] != "PETG" || num(v["label_weight"]) != 800 || v["rgba"] != "4499AAFF" {
			t.Fatal(v)
		}
	}
	f.call(admin, "POST", "inventory/spools", `{"material":"PLA","label_weight":750}`, 200)
	if _, e = db.Exec("UPDATE spool SET tag_uid='01234567'"); e != nil {
		t.Fatal(e)
	}
	if r := scan(body, 200); r["matched"] != true {
		t.Fatal(r)
	}
	if r := scan(row{"device_id": "desk", "tag_uid": "76543210", "tray_uuid": uuid}, 200); r["matched"] != true {
		t.Fatal("second sticker", r)
	}
	f.printer()
	tray := row{"id": "0", "tray_type": "ABS", "tray_color": "AABBDDEE", "tag_uid": "CAFEBABE", "tray_uuid": "99999999999999999999999999999999"}
	svc.MaterialSlots = func(id int64) (map[string]any, bool) {
		return map[string]any{"ams": map[string]any{"ams": []any{row{"id": "0", "tray": []any{tray}}}, "tray_exist_bits": "1"}}, id == 1
	}
	detected := row{"device_id": "desk", "tag_uid": "CAFEBABE", "sak": 8}
	scan(detected, 200)
	if v, ok := event["tag_data"].(row); !ok || v["material"] != "ABS" || event["tray_uuid"] != tray["tray_uuid"] {
		t.Fatal("missing matched AMS report", event)
	}
	// Restricted station credentials must not learn material from an inaccessible printer.
	admin = access.Principal{ID: 8, Permissions: map[string]bool{inventoryWrite: true}, PrinterIDs: map[int64]bool{2: true}}
	scan(detected, 200)
	if event["tag_data"] != nil || event["tray_uuid"] != nil {
		t.Fatal("restricted printer data exposed", event)
	}
	scan(row{"device_id": "desk", "tag_uid": "01234568", "raw_blocks": map[string]string{"1": "bad"}}, 422)
}
