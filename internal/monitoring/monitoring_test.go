package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/testdb"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChamberProtocolChecksKeyAndReadsJPEG(t *testing.T) {
	// The TLS fixture lends a loopback certificate. Its camera server uses a random port.
	fixture := httptest.NewTLSServer(http.NotFoundHandler())
	fixture.Close()
	cert := fixture.TLS.Certificates[0]
	pub := sha256.Sum256(fixture.Certificate().RawSubjectPublicKeyInfo)
	listen, e := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if e != nil {
		t.Fatal(e)
	}
	defer listen.Close()
	frame := []byte{0xff, 0xd8, 1, 2, 3, 4, 0xff, 0xd9}
	done := make(chan error, 1)
	go func() {
		c, e := listen.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		var req [80]byte
		if _, e = io.ReadFull(c, req[:]); e != nil {
			done <- e
			return
		}
		if binary.LittleEndian.Uint32(req[4:8]) != 0x3000 || string(req[16:20]) != "bblp" || string(req[48:52]) != "0123" {
			t.Error("Wrong chamber authentication format")
		}
		var head [16]byte
		binary.LittleEndian.PutUint32(head[:4], uint32(len(frame)))
		_, e = c.Write(append(head[:], frame...))
		done <- e
	}()
	ctx, cancel := context.WithCancel(context.Background())
	var got []byte
	_ = chamberCamera(ctx, listen.Addr().String(), source{code: "0123", pin: hex.EncodeToString(pub[:])}, func(data []byte) { got = data; cancel() })
	cancel()
	if e = <-done; e != nil || !bytes.Equal(got, frame) {
		t.Fatal(got, e)
	}
	go func() {
		c, e := listen.Accept()
		if e == nil {
			defer c.Close()
			_, _ = io.Copy(io.Discard, c)
		}
	}()
	if e = chamberCamera(context.Background(), listen.Addr().String(), source{code: "0123", pin: strings.Repeat("0", 64)}, func([]byte) { t.Error("untrusted camera frame") }); e == nil || !strings.Contains(e.Error(), "certificate") {
		t.Fatal(e)
	}
}
func TestDiscoveryAdvertisementAndRTSPRelay(t *testing.T) {
	ip := netip.MustParseAddr("127.0.0.1")
	ad := "NOTIFY * HTTP/1.1\r\nNT: " + target + "\r\nUSN: SERIAL123\r\nLOCATION: http://other.host/\r\nDevName.bambu.com: Workshop\r\nDevModel.bambu.com: X1C\r\n\r\n"
	p, ok := notice([]byte(ad), ip)
	if !ok || p.Name != "Workshop" || p.IP != ip.String() {
		t.Fatal(p)
	}
	if _, ok = notice([]byte(strings.Replace(ad, "SERIAL123", "BB391800001", 1)), ip); ok {
		t.Fatal("own virtual printer discovered")
	}
	a := "rtsp://127.0.0.1:321"
	b := "rtsps://device:322"
	message := "DESCRIBE " + a + "/streaming/live/1 RTSP/1.0\r\nAuthorization: Digest uri=\"" + a + "/streaming/live/1\"\r\n\r\n"
	packet := []byte{'$', 3, 0, 4, 'x', '\n', 'x', 'y'}
	var out bytes.Buffer
	_ = rtspRelay(bytes.NewReader(append([]byte(message), packet...)), &out, a, b)
	if out.String() != strings.Replace(message, a, b, 1)+string(packet) {
		t.Fatal(out.String())
	}
}

type mockFarm struct{}

func (mockFarm) InitialStatus(context.Context, access.Principal) ([]map[string]any, error) {
	return []map[string]any{{"id": int64(1), "connected": true, "current_print": "private-part", "name": "Shop", "progress": 23}}, nil
}
func TestDisplayScopesRevocationAndArchiveRange(t *testing.T) {
	dir := t.TempDir()
	db := testdb.Open(t)
	auth, e := access.New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO users(username,password_hash,role,auth_source,is_active) VALUES('admin','unused','admin','local',1)"); e != nil {
		t.Fatal(e)
	}
	s, e := New(db, dir, auth, mockFarm{})
	if e != nil {
		t.Fatal(e)
	}
	pub, api := http.NewServeMux(), http.NewServeMux()
	s.Register(pub, api)
	pub.Handle("/api/", auth.Protect(api))
	p := access.Principal{Admin: true, ID: 1}
	r := httptest.NewRequest("POST", "/api/v1/auth/tokens", strings.NewReader(`{"name":"Lobby","scope":"camwall","expires_in_days":2}`))
	w := httptest.NewRecorder()
	api.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	var o object
	if e = json.Unmarshal(w.Body.Bytes(), &o); e != nil || w.Code != 201 {
		t.Fatal(w.Code, w.Body.String(), e)
	}
	token := o["token"].(string)
	do := func(method, path, rangeHeader string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		if rangeHeader != "" {
			r.Header.Set("Range", rangeHeader)
		}
		w := httptest.NewRecorder()
		pub.ServeHTTP(w, r)
		return w
	}
	w = do("GET", "/api/v1/camwall/printers?token="+token, "")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-part") || !strings.Contains(w.Body.String(), "Shop") {
		t.Fatal(w.Code, w.Body.String())
	}
	w = do("GET", "/api/v1/printers/1/overlay-status?token="+token, "")
	if w.Code != 401 {
		t.Fatal("wall token opened overlay", w.Code)
	}
	w = do("GET", "/api/v1/archives/1/timelapse?token="+token, "")
	if w.Code != 401 {
		t.Fatal("wall token opened archive", w.Code)
	}
	r = httptest.NewRequest("DELETE", "/api/v1/auth/tokens/1", nil)
	w = httptest.NewRecorder()
	api.ServeHTTP(w, r.WithContext(access.With(r.Context(), p)))
	if w.Code != 204 {
		t.Fatal(w.Code)
	}
	w = do("GET", "/api/v1/camwall/printers?token="+token, "")
	if w.Code != 401 {
		t.Fatal("revoked token opened wall")
	}
	if e = os.WriteFile(filepath.Join(dir, "archive.mp4"), []byte("1234567890"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = db.Exec("INSERT INTO print_archives(filename,file_path,file_size,status,is_favorite,quantity,timelapse_path) VALUES('x.3mf','x',0,'archived',0,1,'archive.mp4')"); e != nil {
		t.Fatal(e)
	}
	tok, e := auth.Mint(context.Background(), p, "media", time.Hour, "")
	if e != nil {
		t.Fatal(e)
	}
	w = do("GET", "/api/v1/archives/1/timelapse?token="+tok, "bytes=2-5")
	if w.Code != 206 || w.Body.String() != "3456" {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestUDPSolicitedDiscovery(t *testing.T) {
	// Tests never send to the LAN. Simulate one real UDP response on loopback.
	receive, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if e != nil {
		t.Fatal(e)
	}
	defer receive.Close()
	send, e := net.DialUDP("udp4", nil, receive.LocalAddr().(*net.UDPAddr))
	if e != nil {
		t.Fatal(e)
	}
	defer send.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := Service{discovery: discovery{found: map[string]printer{}}}
	done := make(chan struct{})
	go func() { s.listen(ctx, receive, netip.MustParsePrefix("127.0.0.0/8")); close(done) }()
	_, _ = send.Write([]byte("HTTP/1.1 200 OK\r\nST: " + target + "\r\nUSN: SERIAL123\r\n\r\n"))
	deadline := time.Now().Add(time.Second)
	found := false
	for time.Now().Before(deadline) {
		s.mu.Lock()
		found = len(s.discovery.found) == 1
		s.mu.Unlock()
		if found {
			break
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	receive.Close()
	<-done
	if !found {
		t.Fatal("advertisement missing")
	}
}
