// Package monitoring handles local printer discovery, camera observation and archived media.
package monitoring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

type object = map[string]any
type Farm interface {
	InitialStatus(context.Context, access.Principal) ([]map[string]any, error)
}
type Service struct {
	plateStarts chan plateStart
	detectors   detectorService
	closing     bool
	wg          sync.WaitGroup
	edits       map[int64]*editJob
	db          *sql.DB
	dir         string
	auth        *access.Service
	farm        Farm
	mu          sync.Mutex
	discovery   discovery
	feeds       map[int64]*feed
}
type discovery struct {
	scanned, total int
	passive, scan  *search
	found          map[string]printer
}
type search struct {
	cancel         context.CancelFunc
	scanned, total int
}
type printer struct {
	Serial     string    `json:"serial"`
	Name       string    `json:"name"`
	IP         string    `json:"ip_address"`
	Model      string    `json:"model"`
	Discovered time.Time `json:"discovered_at"`
}

func New(db *sql.DB, dir string, auth *access.Service, farm Farm) (*Service, error) {
	s := &Service{db: db, dir: dir, auth: auth, farm: farm, discovery: discovery{found: map[string]printer{}}, edits: map[int64]*editJob{}, feeds: map[int64]*feed{}}
	s.plateStarts = make(chan plateStart, 128)
	return s, s.initPlates()
}
func (s *Service) Register(public, api *http.ServeMux) {
	for path := range map[string]bool{"GET /info": true, "GET /status": true, "GET /printers": true, "POST /start": true, "POST /stop": true, "POST /scan": true, "GET /scan/status": true, "POST /scan/stop": true} {
		method, route, _ := strings.Cut(path, " ")
		api.HandleFunc(method+" /api/v1/discovery"+route, s.discover)
	}
	s.registerMedia(public, api)
	s.registerDetection(public, api)
}
func (s *Service) Run(ctx context.Context) {
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.runDetection(ctx) }()
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.runPlateChecks(ctx) }()
	<-ctx.Done()
	s.mu.Lock()
	s.closing = true
	if s.discovery.passive != nil {
		s.discovery.passive.cancel()
	}
	if s.discovery.scan != nil {
		s.discovery.scan.cancel()
	}
	for _, j := range s.edits {
		if j.status == "running" {
			j.cancel()
		}
	}
	for _, f := range s.feeds {
		f.cancel()
	}
	s.mu.Unlock()
	s.wg.Wait()
}

const target = "urn:bambulab-com:device:3dprinter:1"
const group = "239.255.255.250"
const port = 2021
const query = "M-SEARCH * HTTP/1.1\r\nHOST: 239.255.255.250:2021\r\nMAN: \"ssdp:discover\"\r\nMX: 3\r\nST: " + target + "\r\n\r\n"

var serialText = regexp.MustCompile(`^[A-Za-z0-9_-]{3,64}$`)

func notice(packet []byte, src netip.Addr) (printer, bool) {
	lines := strings.Split(strings.ReplaceAll(string(packet), "\r\n", "\n"), "\n")
	if len(lines) < 2 {
		return printer{}, false
	}
	if lines[0] != "NOTIFY * HTTP/1.1" && !strings.HasPrefix(lines[0], "HTTP/1.1 200") {
		return printer{}, false
	}
	h := map[string]string{}
	for _, ln := range lines[1:] {
		k, v, ok := strings.Cut(ln, ":")
		if ok {
			h[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if h["st"] != target && h["nt"] != target {
		return printer{}, false
	}
	serial, _, _ := strings.Cut(strings.TrimPrefix(h["usn"], "uuid:"), "::")
	if !serialText.MatchString(serial) || strings.HasSuffix(serial, "391800001") {
		return printer{}, false
	}
	name := h["devname.bambu.com"]
	if name == "" {
		name = serial
	}
	return printer{serial, name, src.Unmap().String(), h["devmodel.bambu.com"], time.Now().UTC()}, true
}
func localNetworks() []string {
	seen := map[string]bool{}
	ifaces, _ := net.Interfaces()
	for _, in := range ifaces {
		if in.Flags&net.FlagUp == 0 || in.Flags&net.FlagLoopback != 0 {
			continue
		}
		a, _ := in.Addrs()
		for _, n := range a {
			if p, e := netip.ParsePrefix(n.String()); e == nil && p.Addr().Is4() && p.Addr().IsPrivate() {
				seen[p.Masked().String()] = true
			}
		}
	}
	out := []string{}
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
func (s *Service) discover(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "discovery:scan") {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/discovery")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		httpx.Error(w, 503, "Discovery service is stopping")
		return
	}
	d := &s.discovery
	switch path {
	case "/info":
		_, e := os.Stat("/.dockerenv")
		httpx.JSON(w, 200, object{"is_docker": e == nil, "ssdp_running": d.passive != nil, "scan_running": d.scan != nil, "subnets": localNetworks()})
		return
	case "/printers":
		v := make([]printer, 0, len(d.found))
		for _, p := range d.found {
			v = append(v, p)
		}
		sort.Slice(v, func(i, j int) bool { return v[i].IP < v[j].IP })
		httpx.JSON(w, 200, v)
		return
	case "/start":
		if d.passive == nil {
			seconds, _ := strconv.ParseFloat(r.URL.Query().Get("duration"), 64)
			if seconds <= 0 {
				seconds = 10
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(min(seconds, 300)*float64(time.Second)))
			job := &search{cancel: cancel}
			sockets := multicastSockets()
			if len(sockets) == 0 {
				cancel()
				httpx.Error(w, 503, "Cannot listen for SSDP discovery on a multicast interface")
				return
			}
			d.passive = job
			s.wg.Add(1)
			go func() { defer s.wg.Done(); s.passive(ctx, job, sockets) }()
		}
	case "/stop":
		if d.passive != nil {
			d.passive.cancel()
			d.passive = nil
		}
	case "/scan":
		if d.scan == nil {
			var req struct {
				Subnet  string
				Timeout float64
			}
			if !httpx.Decode(w, r, &req) {
				return
			}
			prefix, e := netip.ParsePrefix(req.Subnet)
			if e != nil || !prefix.Addr().Is4() || prefix.Bits() < 22 || !(prefix.Addr().IsPrivate() || prefix.Addr().IsLoopback() || prefix.Addr().IsLinkLocalUnicast()) {
				httpx.Error(w, 422, "Choose a local IPv4 printer subnet with up to 1,024 addresses")
				return
			}
			conn, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
			if e != nil {
				httpx.Error(w, 503, "Cannot start the discovery listener")
				return
			}
			if req.Timeout <= 0 {
				req.Timeout = 1
			}
			ctx, cancel := context.WithCancel(context.Background())
			total := 1 << (32 - prefix.Bits())
			job := &search{cancel: cancel, total: total}
			d.scan = job
			s.wg.Add(1)
			go func() {
				defer s.wg.Done()
				s.scan(ctx, job, prefix.Masked(), conn, time.Duration(min(req.Timeout, 5)*float64(time.Second)))
			}()
		}
	case "/scan/stop":
		if d.scan != nil {
			d.scan.cancel()
			d.scan = nil
		}
	}
	if strings.HasPrefix(path, "/scan") {
		j := d.scan
		if j == nil {
			httpx.JSON(w, 200, object{"running": false, "scanned": d.scanned, "total": d.total})
		} else {
			httpx.JSON(w, 200, object{"running": true, "scanned": j.scanned, "total": j.total})
		}
	} else {
		httpx.JSON(w, 200, object{"running": d.passive != nil})
	}
}
func multicastSockets() []*net.UDPConn {
	groupAddr := &net.UDPAddr{IP: net.ParseIP(group), Port: port}
	var out []*net.UDPConn
	ifaces, _ := net.Interfaces()
	for _, in := range ifaces {
		if in.Flags&(net.FlagUp|net.FlagMulticast) != (net.FlagUp | net.FlagMulticast) {
			continue
		}
		c, e := net.ListenMulticastUDP("udp4", &in, groupAddr)
		if e == nil {
			out = append(out, c)
		}
	}
	return out
}
func (s *Service) listen(ctx context.Context, c *net.UDPConn, filter netip.Prefix) {
	buf := make([]byte, 4096)
	for {
		_ = c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		n, src, e := c.ReadFromUDPAddrPort(buf)
		if ctx.Err() != nil {
			return
		}
		if e != nil {
			var ne net.Error
			if errors.As(e, &ne) && ne.Timeout() {
				continue
			}
			return
		}
		ip := src.Addr().Unmap()
		if filter.IsValid() && !filter.Contains(ip) {
			continue
		}
		if p, ok := notice(buf[:n], ip); ok {
			s.mu.Lock()
			s.discovery.found[p.IP] = p
			s.mu.Unlock()
		}
	}
}
func (s *Service) passive(ctx context.Context, j *search, sockets []*net.UDPConn) {
	defer j.cancel()
	var readers sync.WaitGroup
	for _, c := range sockets {
		readers.Add(1)
		go func() { defer readers.Done(); s.listen(ctx, c, netip.Prefix{}) }()
	}
	dst, _ := net.ResolveUDPAddr("udp4", fmt.Sprintf("%s:%d", group, port))
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for ctx.Err() == nil {
		for _, c := range sockets {
			_, _ = c.WriteToUDP([]byte(query), dst)
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	for _, c := range sockets {
		c.Close()
	}
	readers.Wait()
	s.mu.Lock()
	if s.discovery.passive == j {
		s.discovery.passive = nil
	}
	s.mu.Unlock()
}
func (s *Service) scan(ctx context.Context, j *search, prefix netip.Prefix, c *net.UDPConn, timeout time.Duration) {
	done := make(chan struct{})
	go func() { s.listen(ctx, c, prefix); close(done) }()
	// Solicit an explicit printer advertisement. Never attempt credentials during discovery.
	for ip := prefix.Addr(); prefix.Contains(ip) && ctx.Err() == nil; ip = ip.Next() {
		_, _ = c.WriteToUDPAddrPort([]byte(query), netip.AddrPortFrom(ip, port))
		s.mu.Lock()
		j.scanned++
		s.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Millisecond):
		}
	}
	select {
	case <-ctx.Done():
	case <-time.After(timeout):
	}
	j.cancel()
	c.Close()
	<-done
	s.mu.Lock()
	if s.discovery.scan == j {
		s.discovery.scanned = j.scanned
		s.discovery.total = j.total
		s.discovery.scan = nil
	}
	s.mu.Unlock()
}
