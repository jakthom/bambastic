package mqttrelay

import (
	"context"
	"database/sql"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"bambastic/internal/access"
	"github.com/eclipse/paho.mqtt.golang/packets"
	_ "modernc.org/sqlite"
)

func TestMQTTRelayRetainLastWillAndTransitions(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	arrived := make(chan *packets.PublishPacket, 16)
	hello := make(chan *packets.ConnectPacket, 1)
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		c, e := l.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		for {
			p, e := packets.ReadPacket(c)
			if e != nil {
				return
			}
			switch v := p.(type) {
			case *packets.ConnectPacket:
				hello <- v
				packets.NewControlPacket(packets.Connack).Write(c)
			case *packets.PublishPacket:
				a := packets.NewControlPacket(packets.Puback).(*packets.PubackPacket)
				a.MessageID = v.MessageID
				a.Write(c)
				arrived <- v
			case *packets.SubscribePacket:
				ack := packets.NewControlPacket(packets.Suback).(*packets.SubackPacket)
				ack.MessageID = v.MessageID
				for range v.Topics {
					ack.ReturnCodes = append(ack.ReturnCodes, 1)
				}
				ack.Write(c)
				reading := packets.NewControlPacket(packets.Publish).(*packets.PublishPacket)
				reading.TopicName = "meter/sensors"
				reading.Payload = []byte(`{"outlet":"enabled","current":[42],"energy":{"kwh":2.5,"today":0.5}}`)
				reading.Write(c)
			case *packets.PingreqPacket:
				packets.NewControlPacket(packets.Pingresp).Write(c)
			case *packets.DisconnectPacket:
				return
			}
		}
	}()
	db, e := sql.Open("sqlite", filepath.Join(t.TempDir(), "r.db"))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_, e = db.Exec(`CREATE TABLE smart_plugs(id INTEGER,enabled BOOLEAN,plug_type TEXT,mqtt_topic TEXT,mqtt_multiplier REAL,mqtt_power_topic TEXT,mqtt_power_path TEXT,mqtt_power_multiplier REAL,mqtt_energy_topic TEXT,mqtt_energy_path TEXT,mqtt_energy_multiplier REAL,mqtt_energy_total_path TEXT,mqtt_energy_total_multiplier REAL,mqtt_state_topic TEXT,mqtt_state_path TEXT,mqtt_state_on_value TEXT); INSERT INTO smart_plugs VALUES(7,1,'mqtt','meter/sensors',1,NULL,'current.0',2,NULL,'energy.today',1,'energy.kwh',1,NULL,'outlet','enabled');CREATE TABLE settings(key TEXT PRIMARY KEY,value TEXT);CREATE TABLE printers(id INTEGER,name TEXT,serial_number TEXT,awaiting_plate_clear BOOLEAN);INSERT INTO printers VALUES(1,'Station','SN123',1);INSERT INTO settings VALUES('mqtt_enabled','true'),('mqtt_broker','127.0.0.1'),('mqtt_port',?),('mqtt_username','automation'),('mqtt_topic_prefix','farm')`, port)
	if e != nil {
		t.Fatal(e)
	}
	s := New(db, func(context.Context, string) (string, error) { return "broker-password", nil })
	s.Initial = func(context.Context, access.Principal) ([]map[string]any, error) {
		return []object{{"printer_id": 1, "connected": true, "state": "IDLE", "access_code": "private-device-token"}}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := s.Start(ctx)
	defer cancel()
	select {
	case v := <-hello:
		if v.WillTopic != "farm/status" || !v.WillRetain || v.WillQos != 1 || v.Username != "automation" || string(v.Password) != "broker-password" {
			t.Fatal("incorrect MQTT connect")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no connection")
	}
	read := func(topic string, retained bool) object {
		t.Helper()
		select {
		case p := <-arrived:
			if p.TopicName != "farm/"+topic || p.Retain != retained || p.Qos != 1 || strings.Contains(string(p.Payload), "private-device-token") {
				t.Fatal(p.TopicName, p.Qos, p.Retain, string(p.Payload))
			}
			var v object
			if e := json.Unmarshal(p.Payload, &v); e != nil {
				t.Fatal(e)
			}
			return v
		case <-time.After(4 * time.Second):
			t.Fatal("missing " + topic)
			return nil
		}
	}
	read("status", true)
	read("printers/SN123/online", false)
	read("printers/SN123/plate_clear", true)
	read("printers/SN123/status", true)
	s.Publish(object{"type": "printer_status", "printer_id": 1, "data": object{"connected": true, "progress": 20}})
	s.Publish(object{"type": "archive_created", "archive_id": 7, "access_code": "private-device-token"})
	read("archive/created", false)
	s.Publish(object{"type": "printer_status", "printer_id": 1, "data": object{"connected": false}})
	read("printers/SN123/offline", false)
	v := read("printers/SN123/status", true)
	if v["connected"] != false {
		t.Fatal(v)
	}
	m := http.NewServeMux()
	s.Register(m)
	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/api/v1/settings/mqtt/status", nil)
	m.ServeHTTP(w, req)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = httptest.NewRecorder()
	m.ServeHTTP(w, req.WithContext(access.With(ctx, access.Principal{Admin: true})))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"connected":true`) || strings.Contains(w.Body.String(), "password") {
		t.Fatal(w.Body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		d, e := s.PlugStatus(ctx, 7)
		if e == nil && d["state"] == "ON" {
			energy, ok := d["energy"].(map[string]any)
			if !ok || energy["power"] != 84.0 || energy["today"] != 0.5 || energy["total"] != 2.5 {
				t.Fatal(d)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("missing MQTT energy/state", d, e)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	v = read("status", true)
	if v["status"] != "offline" {
		t.Fatal(v)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("relay did not stop")
	}
	<-stopped
}
