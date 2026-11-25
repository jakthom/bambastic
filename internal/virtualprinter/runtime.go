package virtualprinter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type instance struct {
	c          cfg
	s          *Service
	ctx        context.Context
	cancel     context.CancelFunc
	tls        *tls.Config
	mu         sync.Mutex
	listeners  []net.Listener
	conns      map[net.Conn]bool
	options    map[string]cachedOptions
	lastUpload object
	uploadTime time.Time
}

func (s *Service) Run(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, v := range s.instances {
			v.stop()
		}
		s.instances = map[int64]*instance{}
	}()
	for {
		all, e := s.config(ctx)
		if e == nil {
			s.reconcile(ctx, all)
			s.drain(ctx, all)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.wake:
		}
	}
}
func (s *Service) reconcile(ctx context.Context, cs []cfg) {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := map[int64]cfg{}
	for _, c := range cs {
		if c.Enabled {
			want[c.ID] = c
		}
	}
	for id, v := range s.instances {
		if want[id] != v.c {
			v.stop()
			delete(s.instances, id)
			delete(s.problems, id)
		}
	}
	for id, c := range want {
		if s.instances[id] != nil {
			continue
		}
		v, e := s.listen(ctx, c)
		if e != nil {
			s.problems[id] = e.Error()
			continue
		}
		delete(s.problems, id)
		s.instances[id] = v
	}
}
func (v *instance) stop() {
	v.cancel()
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, l := range v.listeners {
		l.Close()
	}
	for c := range v.conns {
		c.Close()
	}
}
func (s *Service) listeningIP(c cfg) string {
	if c.Remote != "" {
		return c.Remote
	}
	if c.Bind != "" && c.Bind != "0.0.0.0" {
		return c.Bind
	}
	ips, _ := net.InterfaceAddrs()
	for _, addr := range ips {
		if n, ok := addr.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return "127.0.0.1"
}
func (s *Service) listen(parent context.Context, c cfg) (*instance, error) {
	if c.Code == "" || c.Serial == "" {
		return nil, errors.New("virtual printer has no access code or identity")
	}
	cfg, e := s.tlsConfig(c)
	if e != nil {
		return nil, e
	}
	ctx, cancel := context.WithCancel(parent)
	v := &instance{c: c, s: s, ctx: ctx, cancel: cancel, tls: cfg, conns: map[net.Conn]bool{}, options: map[string]cachedOptions{}}
	ports := []int{3000, 3002, 8883, 990}
	if c.Mode == "proxy" {
		if net.ParseIP(c.TargetIP) == nil {
			cancel()
			return nil, errors.New("proxy target unavailable")
		}
		ports = append(ports, 6000, 322, 2024, 2025, 2026)
		for i := 50000; i <= 50100; i++ {
			ports = append(ports, i)
		}
	} else if c.Target != 0 {
		camera := 322
		if strings.HasPrefix(models[c.Model], "P1") || strings.HasPrefix(models[c.Model], "A") {
			camera = 6000
		}
		ports = append(ports, camera)
	}
	for _, port := range ports {
		l, e := net.Listen("tcp4", net.JoinHostPort(c.Bind, strconv.Itoa(port)))
		if e != nil {
			v.stop()
			return nil, e
		}
		v.listeners = append(v.listeners, l)
	}
	// Bind all ports before advertising a usable printer.
	for i, l := range v.listeners {
		port := ports[i]
		go func(l net.Listener) {
			for {
				conn, e := l.Accept()
				if e != nil {
					return
				}
				v.mu.Lock()
				if ctx.Err() != nil || len(v.conns) >= 64 {
					v.mu.Unlock()
					conn.Close()
					continue
				}
				v.conns[conn] = true
				v.mu.Unlock()
				go func(raw net.Conn) {
					defer func() { raw.Close(); v.mu.Lock(); delete(v.conns, raw); v.mu.Unlock() }()
					raw.SetDeadline(time.Now().Add(10 * time.Minute))
					if c.Mode == "proxy" {
						if port == 8883 {
							v.mqttProxy(raw)
						} else {
							v.tunnel(raw, port)
						}
						return
					}
					switch port {
					case 3000:
						v.bind(raw)
					case 3002:
						v.bind(tls.Server(raw, v.tls))
					case 8883:
						v.mqtt(tls.Server(raw, v.tls))
					case 990:
						v.ftp(tls.Server(raw, v.tls))
					default:
						v.tunnel(raw, port)
					}
				}(conn)
			}
		}(l)
	}
	go v.discover()
	return v, nil
}
func (v *instance) bind(conn net.Conn) {
	var hdr [4]byte
	if _, e := io.ReadFull(conn, hdr[:]); e != nil || hdr[0] != 165 || hdr[1] != 165 {
		return
	}
	n := int(binary.LittleEndian.Uint16(hdr[2:]))
	if n < 6 {
		return
	}
	b := make([]byte, n-4)
	if _, e := io.ReadFull(conn, b); e != nil || !bytes.Equal(b[len(b)-2:], []byte{167, 167}) {
		return
	}
	var req struct{ Login struct{ Command string } }
	if json.Unmarshal(b[:len(b)-2], &req) != nil || req.Login.Command != "detect" {
		return
	}
	body, _ := json.Marshal(object{"login": object{"bind": "free", "command": "detect", "connect": "lan", "dev_cap": 1, "id": v.c.Serial, "model": v.c.Model, "name": v.c.Name, "sequence_id": 3021, "version": "01.07.00.00"}})
	binary.LittleEndian.PutUint16(hdr[2:], uint16(len(body)+6))
	conn.Write(append(append(hdr[:], body...), 167, 167))
}
func (v *instance) discover() {
	dst := &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 2021}
	ip := v.s.listeningIP(v.c)
	var iface *net.Interface
	ifs, _ := net.Interfaces()
	for i := range ifs {
		as, _ := ifs[i].Addrs()
		for _, a := range as {
			n, _, _ := net.ParseCIDR(a.String())
			if n != nil && n.String() == ip {
				iface = &ifs[i]
				break
			}
		}
	}
	conn, e := net.ListenMulticastUDP("udp4", iface, dst)
	if e != nil {
		return
	}
	defer conn.Close()
	stop := context.AfterFunc(v.ctx, func() { conn.Close() })
	defer stop()
	fields := fmt.Sprintf("Location: %s\r\nUSN: %s\r\nCache-Control: max-age=1800\r\nDevModel.bambu.com: %s\r\nDevName.bambu.com: %s\r\nDevConnect.bambu.com: lan\r\nDevBind.bambu.com: free\r\nDevseclink.bambu.com: secure\r\nDevVersion.bambu.com: 01.07.00.00\r\nDevCap.bambu.com: 1\r\n\r\n", ip, v.c.Serial, v.c.Model, v.c.Name)
	st := "urn:bambulab-com:device:3dprinter:1"
	notice := []byte("NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1990\r\nNT: " + st + "\r\nNTS: ssdp:alive\r\n" + fields)
	reply := []byte("HTTP/1.1 200 OK\r\nServer: UPnP/1.0\r\nST: " + st + "\r\n" + fields)
	buf := make([]byte, 4096)
	last := time.Time{}
	for v.ctx.Err() == nil {
		if time.Since(last) > 30*time.Second {
			conn.WriteToUDP(notice, dst)
			conn.WriteToUDP(notice, &net.UDPAddr{IP: net.IPv4bcast, Port: 2021})
			last = time.Now()
		}
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, peer, e := conn.ReadFromUDP(buf)
		if e == nil && bytes.HasPrefix(buf[:n], []byte("M-SEARCH ")) {
			conn.WriteToUDP(reply, peer)
		}
	}
}
func (v *instance) tunnel(conn net.Conn, port int) {
	d := net.Dialer{Timeout: 5 * time.Second}
	other, e := d.DialContext(v.ctx, "tcp", net.JoinHostPort(v.c.TargetIP, strconv.Itoa(port)))
	if e != nil {
		return
	}
	defer other.Close()
	other.SetDeadline(time.Now().Add(10 * time.Minute))
	go func() { io.Copy(other, conn); other.Close() }()
	io.Copy(conn, other)
}
func (v *instance) mqttProxy(raw net.Conn) {
	client := tls.Server(raw, v.tls)
	if client.HandshakeContext(v.ctx) != nil {
		return
	}
	pin := v.c.Pin
	if pin == "" {
		return
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) > 0 {
			sum := sha256.Sum256(cs.PeerCertificates[0].RawSubjectPublicKeyInfo)
			if hex.EncodeToString(sum[:]) == pin {
				return nil
			}
		}
		return errors.New("proxy target certificate changed")
	}}
	d := tls.Dialer{Config: cfg, NetDialer: &net.Dialer{Timeout: 5 * time.Second}}
	target, e := d.DialContext(v.ctx, "tcp", net.JoinHostPort(v.c.TargetIP, "8883"))
	if e != nil {
		return
	}
	defer target.Close()
	go func() { io.Copy(target, client); target.Close() }()
	rd := bufio.NewReader(target)
	for {
		kind, data, e := readPacket(rd)
		if e != nil {
			return
		}
		if kind>>4 == 3 {
			p := data
			_, ok := readStr(&p)
			if ok {
				if (kind>>1)&3 != 0 {
					if len(p) < 2 {
						return
					}
					p = p[2:]
				}
				var msg object
				if json.Unmarshal(p, &msg) == nil {
					rewrite(msg, v.c.TargetIP, v.s.listeningIP(v.c))
					out, _ := json.Marshal(msg)
					data = append(data[:len(data)-len(p)], out...)
				}
			}
		}
		if writePacket(client, kind, data) != nil {
			return
		}
	}
}
func rewrite(m object, from, to string) {
	for k, v := range m {
		switch t := v.(type) {
		case map[string]any:
			rewrite(t, from, to)
		case string:
			if strings.HasPrefix(t, "rtsp://") || strings.HasPrefix(t, "rtsps://") {
				u, err := url.Parse(t)
				if err == nil && u.Hostname() == from {
					port := u.Port()
					u.Host = to
					if port != "" {
						u.Host = net.JoinHostPort(to, port)
					}
					m[k] = u.String()
				}
			}
		case []any:
			for _, x := range t {
				if z, ok := x.(map[string]any); ok {
					rewrite(z, from, to)
				}
			}
		}
	}
}
func (c cfg) fingerprint() string {
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (s *Service) stage(ctx context.Context, c cfg, path, name, source string) error {
	_, e := s.db.ExecContext(ctx, "INSERT INTO bb_vp_received(vp_id,path,filename,source,config_key,ready_at) VALUES(?,?,?,?,?,?)", c.ID, path, name, source, c.fingerprint(), time.Now().Add(5*time.Second).Unix())
	s.changed()
	return e
}
func (s *Service) drain(ctx context.Context, cs []cfg) {
	rs, e := s.db.QueryContext(ctx, "SELECT id,vp_id,path,filename,source,options,config_key FROM bb_vp_received WHERE ready_at<=? AND error IS NULL ORDER BY id LIMIT 10", time.Now().Unix())
	if e != nil {
		return
	}
	type staged struct {
		id, vp                              int64
		path, name, source, opts, configKey string
	}
	items := []staged{}
	for rs.Next() {
		var t staged
		if rs.Scan(&t.id, &t.vp, &t.path, &t.name, &t.source, &t.opts, &t.configKey) == nil {
			items = append(items, t)
		}
	}
	rs.Close()
	s.db.ExecContext(ctx, "DELETE FROM bb_vp_receipts WHERE completed<unixepoch()-120")
	s.db.ExecContext(ctx, "DELETE FROM bb_vp_job WHERE created<unixepoch()-30")
	for _, t := range items {
		for _, c := range cs {
			if c.ID != t.vp || !c.Enabled || c.Mode == "proxy" {
				continue
			}
			if c.fingerprint() != t.configKey {
				s.db.ExecContext(ctx, "UPDATE bb_vp_received SET error=? WHERE id=?", "Virtual printer configuration changed after upload", t.id)
				break
			}
			var options object
			json.Unmarshal([]byte(t.opts), &options)
			if e = s.ingest(ctx, c, t.path, t.name, t.source, options, t.id); e != nil {
				if ctx.Err() == nil {
					s.db.ExecContext(ctx, "UPDATE bb_vp_received SET error=? WHERE id=?", e.Error(), t.id)
				}
			} else {
				os.Remove(t.path)
			}
			break
		}
	}
}
