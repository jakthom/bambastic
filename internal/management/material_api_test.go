package management

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"bambastic/internal/access"
	"golang.org/x/crypto/ssh"
)

func (f fixture) binary(path, typ string, body []byte) *httptest.ResponseRecorder {
	p := access.Principal{Admin: true}
	r := httptest.NewRequest("POST", "/api/v1/"+path, bytes.NewReader(body))
	r.Header.Set("Content-Type", typ)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != 200 {
		f.t.Fatal(w.Code, w.Body.String())
	}
	return w
}
func TestCSVRoundTripLabelPagesAndStatistics(t *testing.T) {
	f := setup(t)
	f.exec("INSERT INTO suppliers(name,name_key) VALUES('Supply','supply')")
	f.exec("INSERT INTO spool(material,label_weight,core_weight,weight_used,weight_locked) VALUES('ABS',100,0,0,0)")
	f.exec("DELETE FROM spool")
	var b bytes.Buffer
	w := multipart.NewWriter(&b)
	part, e := w.CreateFormFile("file", "spools.csv")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(part, "\uFEFFid;Material;Color Name;weight_used;rgba;note;suppliers;purchase_supplier;material_number\r\n1;PLA;Red;15,5;#ff0000;'=abc;Supply;Supply;42\r\n12;PETG;Blue;5;#0000ff;青漢한글;;;\r\n99;PLA;Grey;9000;#cccccc;;;;\r\n")
	w.Close()
	preview := f.binary("inventory/spools/import?dry_run=true", w.FormDataContentType(), b.Bytes())
	var report map[string]any
	json.Unmarshal(preview.Body.Bytes(), &report)
	if num(report["valid_count"]) != 2 || num(report["error_count"]) != 1 || f.value("SELECT count(*) FROM spool") != 0 {
		t.Fatal(report)
	}
	v := f.binary("inventory/spools/import", w.FormDataContentType(), b.Bytes())
	if !strings.Contains(v.Body.String(), `"created":2`) {
		t.Fatal(v.Body.String())
	}
	if f.value("SELECT id FROM spool WHERE material='PLA'") <= 12 || f.value("SELECT weight_used FROM spool WHERE id=12") != 5 || f.value("SELECT is_purchase_source FROM spool_suppliers") != 1 {
		t.Fatal("CSV import IDs, weights, suppliers")
	}
	r := httptest.NewRequest("GET", "/api/v1/inventory/spools/export", nil)
	o := httptest.NewRecorder()
	f.mux.ServeHTTP(o, r.WithContext(access.With(r.Context(), access.Principal{Admin: true})))
	if o.Code != 200 || !strings.Contains(o.Body.String(), "'=abc") || !strings.Contains(o.Body.String(), "青漢한글") {
		t.Fatal(o.Code, o.Body.String())
	}
	f.exec("INSERT INTO spool_usage_history(spool_id,weight_used,percent_used,status,cost,created_at) SELECT id,10,1,'completed',1,'2024-01-01' FROM spool WHERE material='PLA'")
	p := access.Principal{Admin: true}
	stats := f.call(p, "GET", "inventory/stats/suppliers?date_from=2025-01-01", "", 200).([]any)[0].(map[string]any)
	if stats["consumed_g"] != 0. || stats["remaining_g"] != 984.5 {
		t.Fatal(stats)
	}
	preview = f.binary("inventory/labels/preview", "application/json", []byte(`{"template":"box_40x30","spool_id":12}`))
	img, e := png.Decode(preview.Body)
	if e != nil || img.Bounds().Dx() != 472 || img.Bounds().Dy() != 354 {
		t.Fatal(img, e)
	}
	pdf := f.binary("inventory/labels", "application/json", []byte(`{"template":"avery_5160","spool_ids":[12,12],"starting_position":30}`))
	if !bytes.HasPrefix(pdf.Body.Bytes(), []byte("%PDF-")) {
		t.Fatal("missing PDF")
	}
	z := f.binary("inventory/labels", "application/json", []byte(`{"format":"png","dpi":203,"template":"box_62x29","spool_ids":[12,12]}`))
	reader, e := zip.NewReader(bytes.NewReader(z.Body.Bytes()), int64(z.Body.Len()))
	if e != nil || len(reader.File) != 2 {
		t.Fatal(reader, e)
	}
}
func TestAMSSlotCreateRepeatAndChargingWeightGuards(t *testing.T) {
	f := setup(t)
	f.printer()
	s := &Service{db: f.db}
	tray := map[string]any{"id": "0", "tray_type": "PLA", "tray_color": "FFFFFFFF", "tray_info_idx": "GFL01", "tag_uid": "12345678", "remain": 85.}
	state := "IDLE"
	s.MaterialSlots = func(id int64) (map[string]any, bool) {
		return map[string]any{"gcode_state": state, "ams": map[string]any{"ams": []any{map[string]any{"id": "0", "tray": []any{tray}}}}}, id == 1
	}
	f.mux = http.NewServeMux()
	s.Register(f.mux)
	p := access.Principal{Admin: true}
	slot := `{"printer_id":1,"ams_id":0,"tray_id":0}`
	f.call(p, "POST", "inventory/spools/from-slot", slot, 200)
	f.call(p, "POST", "inventory/spools/from-slot", slot, 200)
	if f.value("SELECT count(*) FROM spool") != 1 || f.value("SELECT count(*) FROM spool_assignment") != 1 {
		t.Fatal("repeated assignment duplicated")
	}
	f.exec("UPDATE spool SET weight_used=12,weight_locked=1")
	state = "RUNNING"
	v := sub(f.call(p, "POST", "inventory/sync-ams-weights", "", 200))
	if num(v["printing"]) != 1 || f.value("SELECT weight_used FROM spool") != 12 {
		t.Fatal(v)
	}
	state = "IDLE"
	p.PrinterIDs = map[int64]bool{2: true}
	v = sub(f.call(p, "POST", "inventory/sync-ams-weights", "", 200))
	if num(v["synced"]) != 0 {
		t.Fatal(v)
	}
	p.PrinterIDs = nil
	f.call(p, "POST", "inventory/sync-ams-weights", "", 200)
	if f.value("SELECT weight_used FROM spool") != 150 || f.value("SELECT weight_locked FROM spool") != 0 {
		t.Fatal("manual weight sync failed")
	}
}
func TestSSHUpdatePinnedSessionTransfersOnlySuppliedImage(t *testing.T) {
	newKey := func() ssh.Signer {
		_, key, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		signer, e := ssh.NewSignerFromKey(key)
		if e != nil {
			t.Fatal(e)
		}
		return signer
	}
	serverKey, clientKey := newKey(), newKey()
	conf := &ssh.ServerConfig{PublicKeyCallback: func(m ssh.ConnMetadata, p ssh.PublicKey) (*ssh.Permissions, error) {
		if m.User() == "spoolbuddy" && bytes.Equal(p.Marshal(), clientKey.PublicKey().Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected key")
	}}
	conf.AddHostKey(serverKey)
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	done := make(chan struct{})
	commands := make(chan string, 1)
	contents := make(chan string, 1)
	go func() {
		defer close(done)
		for n := 0; n < 2; n++ {
			netConn, e := listener.Accept()
			if e != nil {
				return
			}
			conn, cs, req, e := ssh.NewServerConn(netConn, conf)
			if e != nil {
				netConn.Close()
				continue
			}
			go ssh.DiscardRequests(req)
			for channel := range cs {
				ch, r, e := channel.Accept()
				if e != nil {
					continue
				}
				for call := range r {
					if call.Type != "exec" {
						call.Reply(false, nil)
						continue
					}
					var x struct{ Cmd string }
					ssh.Unmarshal(call.Payload, &x)
					commands <- x.Cmd
					call.Reply(true, nil)
					b, _ := io.ReadAll(ch)
					contents <- string(b)
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
					ch.Close()
					break
				}
			}
			conn.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	data := "local-binary-fixture"
	digest := sha256.Sum256([]byte(data))
	sum := hex.EncodeToString(digest[:])
	addr := listener.Addr().String()
	var pin string
	trust := func(s string) error { pin = s; return nil }
	if e = deliverStationBinary(ctx, addr, clientKey, "", strings.NewReader(data), sum, trust); e != nil {
		t.Fatal(e)
	}
	if pin != strings.TrimSpace(string(ssh.MarshalAuthorizedKey(serverKey.PublicKey()))) || <-contents != data {
		t.Fatal("wrong host key or stream")
	}
	cmd := <-commands
	if !strings.Contains(cmd, sum) || !strings.Contains(cmd, "sha256sum -c") || !strings.Contains(cmd, "restart spoolbuddy.service") {
		t.Fatal(cmd)
	}
	if e = deliverStationBinary(ctx, addr, clientKey, "different-host", strings.NewReader(data), sum, trust); e == nil {
		t.Fatal("accepted changed host key")
	}
	<-done
}

func TestFilamentDefaultsPreserveEditedRows(t *testing.T) {
	f := setup(t)
	if body := f.binary("filament-catalog/seed-defaults", "application/json", nil).Body.String(); !strings.Contains(body, `"created":9`) {
		t.Fatal(body)
	}
	f.exec("UPDATE filaments SET cost_per_kg=42 WHERE name='Generic PLA'")
	f.binary("filament-catalog/seed-defaults", "application/json", nil)
	if f.value("SELECT cost_per_kg FROM filaments WHERE name='Generic PLA'") != 42 || f.value("SELECT COUNT(*) FROM filaments") != 9 {
		t.Fatal("duplicate or overwritten filament")
	}
}

func TestStationCommandSecretRotationAndDiagnostics(t *testing.T) {
	f := setup(t)
	s, e := New(f.db)
	if e != nil {
		t.Fatal(e)
	}
	s.SealStationSecret = func(s string) string { return "sealed-value" }
	s.OpenStationSecret = func(k string) (string, error) {
		if k != "sealed-value" {
			t.Fatal(k)
		}
		return "replaced-device-key", nil
	}
	f.mux = http.NewServeMux()
	s.Register(f.mux)
	p := access.Principal{Admin: true, KeyID: 30}
	old := p
	newKey := access.Principal{Admin: true, KeyID: 31}
	digest := sha256.Sum256([]byte("replaced-device-key"))
	f.exec("INSERT INTO api_keys(id,name,key_hash,key_prefix,can_queue,can_control_printer,can_read_status,enabled) VALUES(31,'Station',?,'abcd',0,0,1,1)", hex.EncodeToString(digest[:]))
	f.call(p, "POST", "spoolbuddy/devices/register", `{"device_id":"desk","hostname":"bench","ip_address":"127.0.0.1"}`, 200)
	f.call(p, "POST", "spoolbuddy/devices/desk/system/config", `{"backend_url":"https://farm.example","api_key":"replaced-device-key"}`, 200)
	f.call(access.Principal{Admin: true, KeyID: 99}, "POST", "spoolbuddy/devices/desk/heartbeat", `{}`, 403)
	beat := f.call(p, "POST", "spoolbuddy/devices/desk/heartbeat", `{}`, 200).(map[string]any)
	if beat["pending_system_payload"].(map[string]any)["api_key"] != "replaced-device-key" {
		t.Fatal(beat)
	}
	var sealed string
	f.db.QueryRow("SELECT pending_system_payload FROM spoolbuddy_devices WHERE device_id='desk'").Scan(&sealed)
	if strings.Contains(sealed, "replaced-device-key") {
		t.Fatal("plaintext config persisted")
	}
	f.call(p, "POST", "spoolbuddy/devices/desk/system/command-result", `{"command":"apply_system_config","success":true}`, 200)
	f.call(newKey, "POST", "spoolbuddy/devices/desk/heartbeat", `{}`, 200)
	f.call(old, "POST", "spoolbuddy/devices/desk/heartbeat", `{}`, 403)
	f.call(newKey, "POST", "spoolbuddy/diagnostics/desk/run?diagnostic=scale", "", 200)
	f.call(newKey, "POST", "spoolbuddy/diagnostics/desk/result", `{"diagnostic":"nfc","success":true,"output":"Mismatched","exit_code":0}`, 409)
	f.call(newKey, "POST", "spoolbuddy/diagnostics/desk/result", `{"diagnostic":"scale","success":true,"output":"Stable reading","exit_code":0}`, 200)
	report := f.call(p, "GET", "spoolbuddy/diagnostics/desk/result?diagnostic=scale", "", 200).(map[string]any)
	if report["output"] != "Stable reading" {
		t.Fatal(report)
	}
}
