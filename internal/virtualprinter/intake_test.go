package virtualprinter

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"

	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"

	"strings"
	"testing"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/management"
	"bambastic/internal/production"
	"bambastic/internal/testdb"
	"github.com/jlaffaye/ftp"
	_ "modernc.org/sqlite"
)

type simulation struct {
	*testing.T
	*instance
	handler   *http.ServeMux
	clientTLS *tls.Config
}

func simulator(t *testing.T, mode string) simulation {
	t.Helper()
	dir := t.TempDir()
	db := testdb.Open(t)
	var e error
	if _, e = management.New(db); e != nil {
		t.Fatal(e)
	}
	farm, e := production.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	s, e := New(db, farm)
	if e != nil {
		t.Fatal(e)
	}
	m := http.NewServeMux()
	s.Register(m)
	c := cfg{ID: 77, Name: "Simulation", Serial: "SIM123", Mode: mode, Model: "BL-P001", Code: "testcode", Bind: "127.0.0.1", Enabled: true}
	tlsCfg, e := s.tlsConfig(c)
	if e != nil {
		t.Fatal(e)
	}
	pub, _, e := s.authority()
	if e != nil {
		t.Fatal(e)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pub)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	v := &instance{ctx: ctx, c: c, s: s, tls: tlsCfg, options: map[string]cachedOptions{}}
	return simulation{t, v, m, &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "127.0.0.1"}}
}

func design(t *testing.T) []byte {
	t.Helper()
	var b bytes.Buffer
	z := zip.NewWriter(&b)
	for _, n := range []string{"Metadata/plate_2.gcode", "Metadata/plate_7.gcode"} {
		w, e := z.Create(n)
		if e != nil {
			t.Fatal(e)
		}
		io.WriteString(w, "; plate simulation")
	}
	meta, e := z.Create("3D/3dmodel.model")
	if e != nil {
		t.Fatal(e)
	}
	io.WriteString(meta, `<model><metadata name="Title">Aqua &amp;amp; white</metadata><resources/></model>`)
	if e := z.Close(); e != nil {
		t.Fatal(e)
	}
	return b.Bytes()
}
func (f simulation) upload() {
	f.Helper()
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		f.Fatal(e)
	}
	defer ln.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		raw, e := ln.Accept()
		if e != nil {
			return
		}
		defer raw.Close()
		f.ftp(tls.Server(raw, f.tls))
	}()
	client, e := ftp.Dial(ln.Addr().String(), ftp.DialWithTLS(f.clientTLS.Clone()), ftp.DialWithTimeout(2*time.Second))
	if e != nil {
		f.Fatal(e)
	}
	if e = client.Login("bblp", f.c.Code); e != nil {
		f.Fatal(e)
	}
	if e = client.Stor("job.3mf", bytes.NewReader(design(f.T))); e != nil {
		f.Fatal(e)
	}
	if e = client.Quit(); e != nil {
		f.Fatal(e)
	}
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		f.Fatal("FTP session did not finish")
	}
}
func (f simulation) drainNow() {
	f.Helper()
	if _, e := f.s.db.Exec("UPDATE bb_vp_received SET ready_at=0"); e != nil {
		f.Fatal(e)
	}
	f.s.drain(f.ctx, []cfg{f.c})
	var msg string
	if e := f.s.db.QueryRow("SELECT error FROM bb_vp_received WHERE error IS NOT NULL LIMIT 1").Scan(&msg); e == nil {
		f.Fatal(msg)
	}
}
func (f simulation) count(query string) int {
	f.Helper()
	var n int
	if e := f.s.db.QueryRow(query).Scan(&n); e != nil {
		f.Fatal(e)
	}
	return n
}
func (f simulation) request(method, path, body string) []byte {
	f.Helper()
	req := httptest.NewRequest(method, "/api/v1"+path, strings.NewReader(body))
	req = req.WithContext(access.With(req.Context(), access.Principal{Admin: true}))
	res := httptest.NewRecorder()
	f.handler.ServeHTTP(res, req)
	if res.Code != 200 {
		f.Fatalf("%s %s: %d %s", method, path, res.Code, res.Body.String())
	}
	return res.Body.Bytes()
}

func TestTargetLANCodeDuringSetup(t *testing.T) {
	f := simulator(t, "queue")
	_, err := f.s.db.Exec(`INSERT INTO printers(id,name,ip_address,serial_number,access_code,nozzle_count,is_active,auto_archive,print_hours_offset,runtime_seconds,external_camera_enabled,camera_rotation,plate_detection_enabled,awaiting_plate_clear)
		VALUES(7,'Workshop','127.0.0.1','SIM007','0Ab12345',1,0,1,0,0,0,0,0,0)`)
	if err != nil {
		t.Fatal(err)
	}
	manager := access.Principal{Permissions: map[string]bool{"settings:update": true}, PrinterIDs: map[int64]bool{7: true}}
	admin := access.Principal{Admin: true}
	for _, tc := range []struct {
		name string
		who  access.Principal
		body string
		want int
	}{
		{"inherit without printer edit permission", manager, `{"target_printer_id":7}`, 200},
		{"supply the saved code", admin, `{"target_printer_id":7,"target_access_code":"0Ab12345"}`, 200},
		{"printer permission required even for a matching code", manager, `{"target_printer_id":7,"target_access_code":"0Ab12345"}`, 403},
		{"printer outside scope", access.Principal{Permissions: map[string]bool{"settings:update": true, "printers:update": true}, PrinterIDs: map[int64]bool{8: true}}, `{"target_printer_id":7,"target_access_code":"0Ab12345"}`, 404},
		{"target required", admin, `{"target_access_code":"0Ab12345"}`, 422},
		{"incomplete code", admin, `{"target_printer_id":7,"target_access_code":"1234"}`, 422},
		{"unverified replacement", admin, `{"target_printer_id":7,"target_access_code":"new12345"}`, 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/virtual-printers", strings.NewReader(tc.body))
			res := httptest.NewRecorder()
			f.handler.ServeHTTP(res, req.WithContext(access.With(req.Context(), tc.who)))
			if res.Code != tc.want || strings.Contains(res.Body.String(), "0Ab12345") || strings.Contains(res.Body.String(), "new12345") {
				t.Fatalf("unexpected response (HTTP %d)", res.Code)
			}
			if res.Code == 200 {
				var config object
				if err := json.Unmarshal(res.Body.Bytes(), &config); err != nil || config["access_code_set"] != true {
					t.Fatal("created printer did not inherit its target's code")
				}
			}
		})
	}
	if f.count("SELECT count(*) FROM virtual_printers") != 2 || f.count("SELECT count(*) FROM printers WHERE id=7 AND access_code='0Ab12345'") != 1 {
		t.Fatal("failed setup modified saved printers")
	}
}

func TestDiagnosticCertificateAndArchiveIdentity(t *testing.T) {
	f := simulator(t, "archive")
	f.request("POST", "/virtual-printers", `{"name":"Lab","mode":"archive","access_code":"testcode","enabled":false}`)
	var d struct {
		Overall string
		Checks  []object
	}
	if e := json.Unmarshal(f.request("GET", "/virtual-printers/1/diagnostic", ""), &d); e != nil || d.Overall != "warnings" || len(d.Checks) != 9 {
		t.Fatal(d, e)
	}
	var ca object
	if e := json.Unmarshal(f.request("GET", "/virtual-printers/ca-certificate", ""), &ca); e != nil || len(str(ca["fingerprint_sha256"])) != 95 || !strings.Contains(str(ca["pem"]), "BEGIN CERTIFICATE") {
		t.Fatal(ca, e)
	}
	if pem := f.request("GET", "/virtual-printers/ca-certificate?download=1", ""); string(pem) != str(ca["pem"]) || bytes.Contains(pem, []byte("PRIVATE KEY")) {
		t.Fatal("Unexpected certificate download")
	}
	f.upload()
	f.drainNow()
	if f.count("SELECT count(*) FROM print_archives WHERE print_name='Aqua & white'") != 1 {
		t.Fatal("3MF metadata title lost")
	}
	if _, e := f.s.db.Exec("UPDATE print_archives SET print_name='Operator name',notes='Keep this note'"); e != nil {
		t.Fatal(e)
	}
	f.upload()
	f.drainNow()
	if f.count("SELECT count(*) FROM print_archives") != 1 || f.count("SELECT count(*) FROM print_archives WHERE print_name='Operator name' AND notes='Keep this note'") != 1 {
		t.Fatal("Identical detached slice replaced operator metadata")
	}
}

func TestFTPSQueueAndLateMQTT(t *testing.T) {
	f := simulator(t, "queue")
	f.upload()
	f.drainNow()
	f.drainNow()
	if f.count("SELECT count(*) FROM print_archives") != 1 || f.count("SELECT count(*) FROM print_queue WHERE manual_start AND plate_id IN (2,7)") != 2 {
		t.Fatal("multi-plate upload was not staged once")
	}
	client, server := net.Pipe()
	defer client.Close()
	go func() { defer server.Close(); f.mqtt(server) }()
	client.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(client)
	payload := append(mqttString("MQTT"), 4, 0xc2, 0, 20)
	for _, s := range []string{"slicer", "bblp", f.c.Code} {
		payload = append(payload, mqttString(s)...)
	}
	if e := writePacket(client, 0x10, payload); e != nil {
		t.Fatal(e)
	}
	k, p, e := readPacket(reader)
	if e != nil || k != 0x20 || !bytes.Equal(p, []byte{0, 0}) {
		t.Fatal(k, p, e)
	}
	payload = append([]byte{0, 23}, mqttString("device/"+f.c.Serial+"/report")...)
	writePacket(client, 0x82, append(payload, 0))
	k, _, e = readPacket(reader)
	if e != nil || k != 0x90 {
		t.Fatal(k, e)
	}
	k, p, e = readPacket(reader)
	if e != nil || k != 0x30 || !bytes.Contains(p, []byte("IDLE")) {
		t.Fatal(k, e)
	}
	command := []byte(`{"print":{"command":"project_file","sequence_id":"5","file":"job.3mf","subtask_name":"slicer display name","param":"Metadata/plate_2.gcode","timelapse":true}}`)
	payload = append(mqttString("device/"+f.c.Serial+"/request"), command...)
	writePacket(client, 0x30, payload)
	k, p, e = readPacket(reader)
	if e != nil || k != 0x30 || !bytes.Contains(p, []byte("SUCCESS")) {
		t.Fatal(k, e)
	}
	k, p, e = readPacket(reader)
	if e != nil || k != 0x30 || (!bytes.Contains(p, []byte(`"gcode_state":"FINISH"`)) || !bytes.Contains(p, []byte(`"subtask_name":"slicer display name"`))) {
		t.Fatal("missing committed intake completion", k, string(p), e)
	}
	if f.count("SELECT count(*) FROM print_queue WHERE plate_id=2 AND timelapse") != 1 || f.count("SELECT count(*) FROM print_queue WHERE plate_id=7 AND timelapse") != 0 {
		t.Fatal("late options leaked between plates")
	}
}
func TestArchiveReviewAndConfigurationChange(t *testing.T) {
	for _, mode := range []string{"archive", "review"} {
		t.Run(mode, func(t *testing.T) {
			f := simulator(t, mode)
			f.upload()
			f.drainNow()
			if mode == "review" {
				var card object
				if e := json.Unmarshal(f.request("GET", "/pending-uploads/1", ""), &card); e != nil || card["display_name"] != "Aqua & white" {
					t.Fatal(card, e)
				}
				if f.count("SELECT count(*) FROM pending_uploads WHERE status='pending'") != 1 || f.count("SELECT count(*) FROM print_archives") != 0 {
					t.Fatal("review bypassed")
				}
				r := httptest.NewRequest("POST", "/api/v1/pending-uploads/1/archive", strings.NewReader("{}"))
				r = r.WithContext(access.With(r.Context(), access.Principal{Admin: true}))
				w := httptest.NewRecorder()
				f.handler.ServeHTTP(w, r)
				if w.Code != 200 {
					t.Fatal(w.Code, w.Body.String())
				}
			}
			if f.count("SELECT count(*) FROM print_archives") != 1 || f.count("SELECT count(*) FROM print_queue") != 0 {
				t.Fatal("expected archive without dispatch")
			}
			f.upload()
			f.c.Mode = "queue"
			if _, e := f.s.db.Exec("UPDATE bb_vp_received SET ready_at=0"); e != nil {
				t.Fatal(e)
			}
			f.s.drain(f.ctx, []cfg{f.c})
			if f.count("SELECT count(*) FROM bb_vp_received WHERE error IS NOT NULL") != 1 || f.count("SELECT count(*) FROM print_queue") != 0 {
				t.Fatal("upload admitted under a different mode")
			}
		})
	}
}
func TestBindAndProxyAddress(t *testing.T) {
	f := simulator(t, "archive")
	client, server := net.Pipe()
	defer client.Close()
	go func() { defer server.Close(); f.bind(server) }()
	body := []byte(`{"login":{"command":"detect"}}`)
	header := []byte{165, 165, 0, 0}
	binary.LittleEndian.PutUint16(header[2:], uint16(len(body)+6))
	client.Write(append(append(header, body...), 167, 167))
	got, e := io.ReadAll(client)
	if e != nil || len(got) < 6 || !bytes.Contains(got, []byte(f.c.Serial)) {
		t.Fatal(string(got), e)
	}
	mirrored := object{"net": object{"info": []any{object{"ip": 99}}}, "sn": "physical", "module": []any{object{"sn": "physical"}, object{"sn": "AMS-serial"}}, "ipcam": object{"url": "rtsps://192.0.2.33:322/stream"}}
	f.c.TargetIP = "192.0.2.33"
	f.virtualize(mirrored, "physical")
	address := mirrored["net"].(object)["info"].([]any)[0].(object)["ip"]
	modules := mirrored["module"].([]any)
	if address != binary.LittleEndian.Uint32(net.IPv4(127, 0, 0, 1).To4()) || mirrored["sn"] != f.c.Serial || modules[0].(object)["sn"] != f.c.Serial || modules[1].(object)["sn"] != "AMS-serial" || mirrored["ipcam"].(object)["url"] != "rtsps://127.0.0.1:322/stream" {
		t.Fatal(mirrored)
	}
	var msg object
	json.Unmarshal([]byte(`{"print":{"ipcam":"rtsps://printer:322/stream","note":"rtsp://other/contains/printer","file":"printer"}}`), &msg)
	rewrite(msg, "printer", "127.0.0.1")
	out, _ := json.Marshal(msg)
	if !bytes.Contains(out, []byte("rtsps://127.0.0.1:322/stream")) || !bytes.Contains(out, []byte("rtsp://other/contains/printer")) {
		t.Fatal(string(out))
	}
	if f.authenticateConnect(append(append(append(append(mqttString("MQTT"), 4, 0xc2, 0, 20), mqttString("client")...), mqttString("bblp")...), mqttString("bad")...)) {
		t.Fatal("accepted wrong MQTT password")
	}
}
