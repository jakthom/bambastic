package spoolbuddy

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureScale struct{}

func (fixtureScale) Raw() (int, bool, error) { return 700, true, nil }
func (fixtureScale) Close() error            { return nil }

type fixtureReader struct{ reads int }

func (r *fixtureReader) ReadTag() (*Tag, error) {
	r.reads++
	if r.reads <= 8 {
		return &Tag{"01234567", 0}, nil
	}
	return nil, nil
}
func (r *fixtureReader) Close() error { return nil }
func TestStationRegistersReportsCalibratedWeightAndRecoversConnection(t *testing.T) {
	var mu sync.Mutex
	registered, scans, removed, beats := 0, 0, 0, 0
	stable := false
	recovered := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "station-secret" {
			t.Error("missing device key")
		}
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		mu.Lock()
		defer mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/api/v1/spoolbuddy/") {
		case "devices/register":
			registered++
			json.NewEncoder(w).Encode(map[string]any{"tare_offset": 100, "calibration_factor": 0.5})
		case "devices/station/heartbeat":
			beats++
			if beats == 2 {
				w.WriteHeader(503)
				return
			}
			if beats > 2 {
				recovered = true
			}
			json.NewEncoder(w).Encode(map[string]any{"tare_offset": 100, "calibration_factor": 0.5})
		case "nfc/tag-scanned":
			scans++
			if body["tag_uid"] != "01234567" {
				t.Error(body)
			}
			w.Write([]byte("{}"))
		case "nfc/tag-removed":
			removed++
			w.Write([]byte("{}"))
		case "scale/reading":
			if body["stable"] == true && body["weight_grams"] == 300. {
				stable = true
			}
			w.Write([]byte("{}"))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	s := New(Config{Backend: server.URL, APIKey: "station-secret", DeviceID: "station", Hostname: "pi", IPAddress: "127.0.0.1", Heartbeat: 40 * time.Millisecond, Sample: 2 * time.Millisecond, WeightReport: 7 * time.Millisecond, TagPoll: 3 * time.Millisecond}, Hardware{OpenScale: func() (Scale, error) { return fixtureScale{}, nil }, OpenNFC: func() (NFC, error) { return &fixtureReader{}, nil }})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	for ctx.Err() == nil {
		mu.Lock()
		ok := registered >= 2 && scans == 1 && removed == 1 && stable && recovered
		mu.Unlock()
		if ok {
			cancel()
			break
		}
		time.Sleep(8 * time.Millisecond)
	}
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	mu.Lock()
	defer mu.Unlock()
	if registered < 2 || scans != 1 || removed != 1 || !stable || !recovered {
		t.Fatalf("registration=%d scans=%d removals=%d calibrated=%v reconnected=%v", registered, scans, removed, stable, recovered)
	}
}

type cascadeBus struct {
	responses [][]byte
	current   []byte
	frames    [][]byte
}

func (b *cascadeBus) Command(cmd []byte, n int) ([]byte, error) {
	switch cmd[0] {
	case 9:
		b.frames = append(b.frames, append([]byte{}, cmd[2:]...))
		b.current = b.responses[0]
		b.responses = b.responses[1:]
	case 4:
		v := make([]byte, 4)
		if cmd[1] == 0x13 {
			binary.LittleEndian.PutUint32(v, uint32(len(b.current)))
		}
		return v, nil
	case 0xa:
		return b.current[:n], nil
	}
	return nil, nil
}
func (*cascadeBus) Close() error { return nil }
func TestCompleteCascadeUIDAndSigned24BitADC(t *testing.T) {
	b := &cascadeBus{responses: [][]byte{{4, 0}, {0x88, 1, 2, 3, 0x88 ^ 1 ^ 2 ^ 3}, {4}, {4, 5, 6, 7, 4 ^ 5 ^ 6 ^ 7}, {0}}}
	tag, e := (&pn5180{b}).ReadTag()
	if e != nil || tag == nil || tag.UID != "01020304050607" || tag.SAK != 0 {
		t.Fatal(tag, e)
	}
	for input, want := range map[[3]byte]int{{0xff, 0xff, 0xff}: -1, {0x80, 0, 0}: -8388608, {0x7f, 0xff, 0xff}: 8388607} {
		if got := signedADC(input); got != want {
			t.Errorf("%x = %d, want %d", input, got, want)
		}
	}
}

type tagMemory struct {
	mem    [1024]byte
	last   []byte
	writes int
	damage bool
}

func (b *tagMemory) Command(cmd []byte, n int) ([]byte, error) {
	switch cmd[0] {
	case 9:
		msg := cmd[2:]
		switch msg[0] {
		case 0x26, 0x52:
			b.last = []byte{4, 0}
		case 0x93:
			if msg[1] == 0x20 {
				b.last = []byte{1, 2, 3, 4, 1 ^ 2 ^ 3 ^ 4}
			} else {
				b.last = []byte{0}
			}
		case 0x30:
			start := int(msg[1]) * 4
			b.last = b.mem[start : start+16]
		case 0xa2:
			start := int(msg[1]) * 4
			copy(b.mem[start:], msg[2:])
			if b.damage {
				b.mem[start] ^= 1
			}
			b.writes++
			b.last = []byte{0xa}
		}
	case 4:
		out := make([]byte, 4)
		if cmd[1] == 0x13 {
			binary.LittleEndian.PutUint32(out, uint32(len(b.last)))
		}
		return out, nil
	case 0xa:
		return b.last[:n], nil
	}
	return nil, nil
}
func (*tagMemory) Close() error { return nil }
func TestTagCapacityWriteAndReadback(t *testing.T) {
	record := func(n int) []byte {
		mime := "application/opentag3d"
		v := []byte{3, byte(3 + len(mime) + n), 0xd2, byte(len(mime)), byte(n)}
		v = append(v, []byte(mime)...)
		v = append(v, make([]byte, n)...)
		return append(v, 0xfe)
	}
	small, large := record(102), record(162)
	for _, v := range []struct {
		size              byte
		damage, wantError bool
		version           string
	}{{8, false, true, ""}, {18, false, false, "1.000"}, {62, false, false, "2.004"}, {18, true, true, ""}} {
		b := &tagMemory{damage: v.damage}
		copy(b.mem[12:16], []byte{0xe1, 0x10, v.size, 0})
		msg, e := (&pn5180{b}).WriteTag("01020304", small, large)
		if (e != nil) != v.wantError || e == nil && !strings.Contains(msg, v.version) {
			t.Fatal(v, msg, e)
		}
		if b.mem[12] != 0xe1 || v.size == 8 && b.writes != 0 {
			t.Fatal("wrote outside user data or exceeded capacity")
		}
	}
}

type authorizedTagBus struct {
	cascadeBus
	auth [][]byte
	deny bool
}

func (b *authorizedTagBus) Command(cmd []byte, n int) ([]byte, error) {
	if cmd[0] == 0x0c {
		b.auth = append(b.auth, append([]byte(nil), cmd...))
		if b.deny {
			return []byte{1}, nil
		}
		return []byte{0}, nil
	}
	return b.cascadeBus.Command(cmd, n)
}
func TestTagReadUsesOnlyProvisionedSectorCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tag-keys.json")
	t.Setenv("SPOOLBUDDY_TAG_KEYS_FILE", path)
	if e := os.WriteFile(path, []byte(`{"01020304":{"2":{"type":"B","key":"000102030405"}}}`), 0600); e != nil {
		t.Fatal(e)
	}
	uuid := []byte("0123456789abcdef")
	b := &authorizedTagBus{cascadeBus: cascadeBus{responses: [][]byte{uuid}}}
	p := pn5180{b}
	data := p.TagMetadata(&Tag{"01020304", 8})
	if len(b.auth) != 1 || b.auth[0][7] != 0x61 || b.auth[0][8] != 9 || data["tray_uuid"] != "30313233343536373839616263646566" {
		t.Fatal("credential-scoped read failed")
	}
	b.deny = true
	if data = p.TagMetadata(&Tag{"01020304", 8}); data["raw_blocks"] != nil || len(b.auth) != 2 {
		t.Fatal("unsuccessful tag authentication returned data")
	}
	if data = p.TagMetadata(&Tag{"05060708", 8}); len(b.auth) != 2 || data["tray_uuid"] != nil {
		t.Fatal("used another tag's credentials")
	}
}

func TestReadableOpenTagMetadataAndReadProtectedFallback(t *testing.T) {
	payload := make([]byte, 102)
	binary.BigEndian.PutUint16(payload, 1000)
	copy(payload[2:], "PLA  ")
	const mime = "application/opentag3d"
	record := []byte{3, byte(3 + len(mime) + len(payload)), 0xd2, byte(len(mime)), byte(len(payload))}
	record = append(record, []byte(mime)...)
	record = append(record, payload...)
	record = append(record, 0xfe)
	cc := make([]byte, 16)
	cc[0], cc[1], cc[2] = 0xe1, 0x10, 18
	blocks := [][]byte{cc}
	for pos := 0; pos < len(record); pos += 16 {
		b := make([]byte, 16)
		copy(b, record[pos:])
		blocks = append(blocks, b)
	}
	p := pn5180{&cascadeBus{responses: blocks}}
	result := p.TagMetadata(&Tag{"01020304050607", 0})
	if result["opentag3d_ndef"] != hex.EncodeToString(record) {
		t.Fatal("station lost part of the readable NDEF record")
	}
	cc[3] = 0xf0
	p = pn5180{&cascadeBus{responses: [][]byte{cc}}}
	if result = p.TagMetadata(&Tag{"01020304050607", 0}); result["opentag3d_ndef"] != nil {
		t.Fatal("treated protected data as readable")
	}
}
