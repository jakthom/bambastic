package production

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/eclipse/paho.mqtt.golang/packets"
)

// The broker only processes CONNECT. It never delivers printer telemetry.
func TestRegistrationWithMQTTLoginOnly(t *testing.T) {
	cert := httptest.NewTLSServer(nil)
	defer cert.Close()
	listener, err := tls.Listen("tcp", "127.0.0.1:0", cert.TLS)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	const code = "01Ab2345"
	logins := make(chan *packets.ConnectPacket, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := listener.Accept()
			if err != nil {
				return
			}
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			packet, err := packets.ReadPacket(c)
			login, ok := packet.(*packets.ConnectPacket)
			if err == nil && ok {
				logins <- login
				ack := packets.NewControlPacket(packets.Connack).(*packets.ConnackPacket)
				if login.Username != "bblp" || string(login.Password) != code {
					ack.ReturnCode = packets.ErrRefusedBadUsernameOrPassword
				}
				if ack.Write(c) == nil && ack.ReturnCode == packets.Accepted {
					packets.ReadPacket(c) // client disconnect
				}
			}
			c.Close()
		}
	}()
	defer func() { listener.Close(); <-done }()

	f := farmFixture(t)
	f.s.open = func(ctx context.Context, pr row, pin string, receive func(row)) (hardware, error) {
		return openLANAt(ctx, listener.Addr().String(), pr, pin, receive)
	}
	f.s.probePort = func(context.Context, string, string, string, bool) error { return nil }
	body := row{"name": "Test login", "ip_address": "127.0.0.1", "serial_number": "AbC001", "access_code": code}
	checkLogin := func(wantCode string) {
		t.Helper()
		select {
		case login := <-logins:
			if !login.UsernameFlag || !login.PasswordFlag || login.Username != "bblp" || string(login.Password) != wantCode || login.ProtocolVersion != 4 {
				t.Fatal("unexpected MQTT CONNECT fields")
			}
		case <-time.After(time.Second):
			t.Fatal("broker did not receive MQTT CONNECT")
		}
	}
	diagnose := func(wantAuth, wantReason string) {
		t.Helper()
		var result struct {
			Checks []struct {
				ID, Status string
				Params     struct{ Reason string }
			}
		}
		if err := json.Unmarshal(f.req("POST", "/printers/diagnostic", body, f.admin, "", 200), &result); err != nil {
			t.Fatal(err)
		}
		found := map[string]string{}
		for _, c := range result.Checks {
			found[c.ID] = c.Status
			if c.ID == "mqtt_auth" && c.Params.Reason != wantReason {
				t.Fatal(c)
			}
		}
		if found["mqtt_auth"] != wantAuth || found["printer_publishing"] != "skip" {
			t.Fatal(found)
		}
		checkLogin(val(body["access_code"]))
	}
	diagnose("pass", "")
	f.req("POST", "/printers/", body, f.admin, "", 200)
	checkLogin(code)
	var serial, pin string
	if err := f.s.db.QueryRow("SELECT p.serial_number,t.fingerprint FROM printers p JOIN bb_printer_trust t ON t.printer_id=p.id WHERE p.name=?", body["name"]).Scan(&serial, &pin); err != nil || serial != "AbC001" || len(pin) != 64 {
		t.Fatal("printer identity was not enrolled", err)
	}
	body["serial_number"] = "AbC002"
	body["access_code"] = "incorrect"
	diagnose("fail", "auth_rejected")
	response := string(f.req("POST", "/printers/", body, f.admin, "", 400))
	checkLogin("incorrect")
	if !strings.Contains(response, "refused the MQTT login") || strings.Contains(response, "incorrect") {
		t.Fatal("unexpected rejection message")
	}
}

// This broker behaves like a P1S which answers pushall on /report but never
// sends a PUBACK. The status session must rely on the printer's telemetry.
func TestStatusFromPrinterWithoutPublishAck(t *testing.T) {
	cert := httptest.NewTLSServer(nil)
	defer cert.Close()
	for _, tc := range []struct {
		name, report string
	}{
		{"idle", `{"print":{"command":"push_status","gcode_state":"IDLE","bed_temper":17.5}}`},
		{"temperature_delta", `{"print":{"command":"push_status","bed_temper":17.5}}`},
		{"commandless_delta", `{"print":{"bed_temper":17.5}}`},
		{"acknowledgements_only", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := tls.Listen("tcp", "127.0.0.1:0", cert.TLS)
			if err != nil {
				t.Fatal(err)
			}
			const topic = "device/P1S123/report"
			requests := make(chan *packets.PublishPacket, 3)
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				for {
					p, err := packets.ReadPacket(conn)
					if err != nil {
						return
					}
					switch p := p.(type) {
					case *packets.ConnectPacket:
						packets.NewControlPacket(packets.Connack).Write(conn)
					case *packets.SubscribePacket:
						if len(p.Topics) != 1 || p.Topics[0] != topic {
							t.Errorf("unexpected subscription %v", p.Topics)
							return
						}
						ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
						ack.MessageID, ack.ReturnCodes = p.MessageID, []byte{0}
						ack.Write(conn)
					case *packets.PublishPacket:
						requests <- p
						if strings.Contains(string(p.Payload), `"pushall"`) {
							for _, frame := range []string{
								`{"info":{"module":[{"name":"ota","sw_ver":"1.0"}]}}`,
								`{"print":{"command":"project_file","result":"SUCCESS"}}`,
								`{"print":{"sequence_id":"0","result":"SUCCESS"}}`, tc.report,
							} {
								if frame != "" {
									report := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
									report.TopicName, report.Payload = topic, []byte(frame)
									report.Write(conn)
								}
							}
						}
					case *packets.DisconnectPacket:
						return
					}
				}
			}()
			defer func() { listener.Close(); <-done }()

			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			sawTemperature := make(chan bool, 1)
			h, err := openLANAt(ctx, listener.Addr().String(), row{"serial_number": "P1S123"}, "", func(delta row) {
				if delta["bed_temper"] == 17.5 {
					sawTemperature <- true
				}
			})
			if tc.report == "" {
				if h != nil || !errors.Is(err, errPrinterStatus) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("acknowledgements alone marked the printer ready: %v", err)
				}
				return
			}
			if err != nil || !h.Online() || len(sawTemperature) != 1 {
				t.Fatalf("received status did not bring printer online: %v", err)
			}
			defer h.Close()
			for _, command := range []string{"pushall", "get_version"} {
				select {
				case p := <-requests:
					if p.TopicName != "device/P1S123/request" || p.Qos != 0 || !strings.Contains(string(p.Payload), command) {
						t.Fatalf("expected QoS 0 %s request, got %s at QoS %d", command, p.Payload, p.Qos)
					}
				case <-ctx.Done():
					t.Fatalf("printer did not receive %s", command)
				}
			}
		})
	}
}
