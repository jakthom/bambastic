package monitoring

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	"bambastic/internal/production"
)

type source struct {
	id                                                                int64
	address, model, code, pin, externalURL, externalType, snapshotURL string
	external                                                          bool
}
type feed struct {
	source             source
	cancel             context.CancelFunc
	listeners          map[chan []byte]bool
	last               []byte
	lastFrame, started time.Time
	err                error
}

func (s *Service) device(r *http.Request) (source, error) {
	id := num(r.PathValue("id"))
	p := access.From(r.Context())
	if !p.AllowsPrinter(id) {
		return source{}, errors.New("Printer not found")
	}
	var v source
	v.id = id
	e := s.db.QueryRowContext(r.Context(), `SELECT p.ip_address,COALESCE(p.model,''),p.access_code,COALESCE(t.fingerprint,''),p.external_camera_enabled,COALESCE(p.external_camera_url,''),COALESCE(p.external_camera_type,''),COALESCE(p.external_camera_snapshot_url,'') FROM printers p LEFT JOIN bb_printer_trust t ON t.printer_id=p.id WHERE p.id=?`, id).Scan(&v.address, &v.model, &v.code, &v.pin, &v.external, &v.externalURL, &v.externalType, &v.snapshotURL)
	return v, e
}
func (s *Service) subscribe(v source) (*feed, chan []byte, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		ch := make(chan []byte)
		close(ch)
		return &feed{err: errors.New("Camera service is stopping")}, ch, func() {}
	}
	f := s.feeds[v.id]
	if f != nil && f.source != v {
		f.cancel()
		f = nil
	}
	ch := make(chan []byte, 1)
	if f == nil {
		ctx, cancel := context.WithCancel(context.Background())
		f = &feed{source: v, cancel: cancel, listeners: map[chan []byte]bool{}, started: time.Now()}
		s.feeds[v.id] = f
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			send := func(buf []byte) {
				s.mu.Lock()
				defer s.mu.Unlock()
				f.last = buf
				f.lastFrame = time.Now()
				for c := range f.listeners {
					select {
					case c <- buf:
					default:
					}
				}
			}
			err := s.readCamera(ctx, v, send)
			s.mu.Lock()
			defer s.mu.Unlock()
			f.err = err
			for c := range f.listeners {
				close(c)
			}
			if s.feeds[v.id] == f {
				delete(s.feeds, v.id)
			}
		}()
	}
	f.listeners[ch] = true
	if len(f.last) > 0 && time.Since(f.lastFrame) < 5*time.Second {
		ch <- f.last
	}
	closeView := func() {
		s.mu.Lock()
		delete(f.listeners, ch)
		if len(f.listeners) == 0 && s.feeds[v.id] == f {
			delete(s.feeds, v.id)
			f.cancel()
		}
		s.mu.Unlock()
	}
	return f, ch, closeView
}
func (s *Service) camera(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "camera:view") {
		return
	}
	v, e := s.device(r)
	if e != nil {
		httpx.Error(w, 404, "Printer not found")
		return
	}
	action := path.Base(r.URL.Path)
	if action == "stop" { // The departing viewer's HTTP cancellation releases its subscription.
		httpx.JSON(w, 200, object{"status": "ok"})
		return
	}
	if action == "status" {
		s.mu.Lock()
		f := s.feeds[v.id]
		out := object{"active": f != nil, "has_frames": false, "seconds_since_frame": nil, "stalled": false}
		if f != nil {
			out["has_frames"] = f.last != nil
			if !f.lastFrame.IsZero() {
				out["seconds_since_frame"] = time.Since(f.lastFrame).Seconds()
				out["stalled"] = time.Since(f.lastFrame) > 10*time.Second
			} else {
				out["stalled"] = time.Since(f.started) > 10*time.Second
			}
		}
		s.mu.Unlock()
		httpx.JSON(w, 200, out)
		return
	}
	f, ch, release := s.subscribe(v)
	defer release()
	tm := time.NewTimer(18 * time.Second)
	defer tm.Stop()
	var first []byte
	select {
	case <-r.Context().Done():
		return
	case <-tm.C:
		e = errors.New("Camera timed out before sending a frame")
	case b, ok := <-ch:
		if !ok {
			s.mu.Lock()
			e = f.err
			s.mu.Unlock()
			if e == nil {
				e = errors.New("Camera closed before sending a frame")
			}
		} else {
			first = b
		}
	}
	if action == "test" || action == "diagnose" {
		msg := "Camera frame received"
		if e != nil {
			msg = e.Error()
		}
		httpx.JSON(w, 200, object{"success": e == nil, "message": msg, "checks": []object{{"key": "camera_frame", "status": map[bool]string{true: "pass", false: "fail"}[e == nil], "message": msg}}})
		return
	}
	if e != nil {
		httpx.Error(w, 503, e.Error())
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if action == "snapshot" {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write(first)
		return
	}
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary=frame")
	write := func(b []byte) error {
		rc := http.NewResponseController(w)
		_ = rc.SetWriteDeadline(time.Now().Add(10 * time.Second))
		_, e := fmt.Fprintf(w, "--frame\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n", len(b))
		if e != nil {
			return e
		}
		if _, e = w.Write(b); e != nil {
			return e
		}
		if _, e = w.Write([]byte("\r\n")); e != nil {
			return e
		}
		return rc.Flush()
	}
	if write(first) != nil {
		return
	}
	verify := time.NewTicker(30 * time.Second)
	defer verify.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-verify.C:
			p, err := s.mediaIdentity(r, "camera")
			if err != nil || !p.Has("camera:view") || !p.AllowsPrinter(v.id) {
				return
			}
			if _, err = s.device(r); err != nil {
				return
			}
		case b, ok := <-ch:
			if !ok || write(b) != nil {
				return
			}
		}
	}
}
func (s *Service) readCamera(ctx context.Context, v source, send func([]byte)) error {
	if v.external && v.externalURL != "" {
		if v.externalType == "usb" {
			p, e := usbDevice(v.externalURL)
			if e != nil {
				return e
			}
			return ffmpegInput(ctx, []string{"-f", "v4l2", "-i", p}, send)
		}
		u, e := url.Parse(v.externalURL)
		if e != nil || u.Host == "" {
			return errors.New("External camera requires a network URL")
		}
		switch u.Scheme {
		case "http", "https":
			if strings.EqualFold(v.externalType, "snapshot") {
				return readSnapshots(ctx, u.String(), send)
			}
			req, _ := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
			client := http.Client{CheckRedirect: lanhttp.Client.CheckRedirect, Transport: &http.Transport{DialContext: lanhttp.DialContext, ResponseHeaderTimeout: 8 * time.Second, DisableKeepAlives: true}}
			res, e := client.Do(req)
			if e != nil {
				return errors.New("External camera connection failed")
			}
			defer res.Body.Close()
			if res.StatusCode/100 != 2 {
				return fmt.Errorf("External camera returned %s", res.Status)
			}
			if mt, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type")); mt == "image/jpeg" {
				b, e := io.ReadAll(io.LimitReader(res.Body, 10_000_001))
				if e != nil || !jpegFrame(b) {
					return errors.New("External camera returned an invalid JPEG")
				}
				send(b)
				res.Body.Close()
				return readSnapshots(ctx, u.String(), send)
			}
			return jpegSequence(ctx, res.Body, send)
		case "rtsp", "rtsps":
			return ffmpegStream(ctx, u.String(), send)
		default:
			return errors.New("Unsupported external camera transport")
		}
	}
	if v.pin == "" {
		return errors.New("Printer identity must be verified before opening its camera")
	}
	var err error
	for _, port := range production.CameraPorts(v.model) {
		address := net.JoinHostPort(v.address, string(port))
		if port == production.RTSPS {
			return pinnedRTSP(ctx, address, v, send)
		}
		received := false
		err = chamberCamera(ctx, address, v, func(frame []byte) {
			received = true
			send(frame)
		})
		if err == nil || received || ctx.Err() != nil {
			return err
		}
	}
	return err
}
func chamberCamera(ctx context.Context, address string, v source, send func([]byte)) error {
	raw, e := (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, "tcp", address)
	if e != nil {
		return errors.New("Printer chamber camera is unavailable")
	}
	c := tls.Client(raw, production.DeviceTLS(v.pin))
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if e = c.HandshakeContext(ctx); e != nil {
		return errors.New("Printer camera certificate verification failed")
	}
	var hello [80]byte
	binary.LittleEndian.PutUint32(hello[:4], 0x40)
	binary.LittleEndian.PutUint32(hello[4:8], 0x3000)
	copy(hello[16:48], "bblp")
	copy(hello[48:], v.code)
	if _, e = c.Write(hello[:]); e != nil {
		return errors.New("Could not authenticate to printer camera")
	}
	for ctx.Err() == nil {
		_ = c.SetReadDeadline(time.Now().Add(12 * time.Second))
		var head [16]byte
		if _, e = io.ReadFull(c, head[:]); e != nil {
			return errors.New("Chamber camera disconnected")
		}
		size := binary.LittleEndian.Uint32(head[:4])
		if size < 4 || size > 10_000_000 {
			return errors.New("Invalid chamber frame size")
		}
		b := make([]byte, size)
		if _, e = io.ReadFull(c, b); e != nil || !jpegFrame(b) {
			return errors.New("Invalid chamber JPEG")
		}
		send(b)
	}
	return ctx.Err()
}
func jpegFrame(b []byte) bool {
	return len(b) >= 4 && len(b) <= 10_000_000 && b[0] == 0xff && b[1] == 0xd8 && b[len(b)-2] == 0xff && b[len(b)-1] == 0xd9
}
func readSnapshots(ctx context.Context, address string, send func([]byte)) error {
	client := lanhttp.Client
	for ctx.Err() == nil {
		r, e := http.NewRequestWithContext(ctx, "GET", address, nil)
		if e != nil {
			return errors.New("Invalid external camera URL")
		}
		res, e := client.Do(r)
		if e != nil {
			return errors.New("External snapshot is unavailable")
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, 10_000_001))
		res.Body.Close()
		if e != nil || res.StatusCode != 200 || !jpegFrame(b) {
			return errors.New("External snapshot returned an invalid image")
		}
		send(b)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return ctx.Err()
}
func jpegSequence(ctx context.Context, source io.Reader, send func([]byte)) error {
	buf := make([]byte, 32<<10)
	pending := make([]byte, 0, 100<<10)
	var start = []byte{0xff, 0xd8}
	var end = []byte{0xff, 0xd9}
	for ctx.Err() == nil {
		n, e := source.Read(buf)
		if n > 0 {
			pending = append(pending, buf[:n]...)
			for len(pending) > 1 {
				a := bytes.Index(pending, start)
				if a < 0 {
					pending = pending[len(pending)-1:]
					break
				}
				pending = pending[a:]
				b := bytes.Index(pending[2:], end)
				if b < 0 {
					break
				}
				b += 4
				if b > 10_000_000 {
					return errors.New("Camera frame exceeds 10 MB")
				}
				frame := bytes.Clone(pending[:b])
				send(frame)
				pending = pending[b:]
			}
			if len(pending) > 10_000_000 {
				return errors.New("Camera frame exceeds 10 MB")
			}
		}
		if e != nil {
			return e
		}
	}
	return ctx.Err()
}
func ffmpegStream(ctx context.Context, address string, send func([]byte)) error {
	return ffmpegInput(ctx, []string{"-rtsp_transport", "tcp", "-protocol_whitelist", "rtsp,tcp,tls,udp,rtp", "-i", address}, send)
}
func ffmpegInput(ctx context.Context, input []string, send func([]byte)) error {
	bin, e := exec.LookPath("ffmpeg")
	if e != nil {
		return errors.New("RTSP camera streaming requires ffmpeg on the Go server")
	}
	procCtx, stop := context.WithCancel(ctx)
	defer stop()
	args := append([]string{"-nostdin", "-hide_banner", "-loglevel", "error"}, input...)
	args = append(args, "-an", "-vf", "fps=8", "-vcodec", "mjpeg", "-q:v", "6", "-f", "image2pipe", "pipe:1")
	cmd := exec.CommandContext(procCtx, bin, args...)
	stdout, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return errors.New("Could not start ffmpeg")
	}
	e = jpegSequence(procCtx, stdout, send)
	stop()
	_ = cmd.Wait()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if e != nil {
		return errors.New("RTSP session ended before another frame arrived")
	}
	return nil
}
func pinnedRTSP(ctx context.Context, camera string, v source, send func([]byte)) error {
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		return e
	}

	relayCtx, cancel := context.WithCancel(ctx)

	local := "rtsp://" + listener.Addr().String()
	remote := "rtsps://" + camera
	var relays sync.WaitGroup
	acceptDone := make(chan struct{})
	defer func() { cancel(); listener.Close(); <-acceptDone; relays.Wait() }()
	go func() {
		defer close(acceptDone)
		for {
			a, e := listener.Accept()
			if e != nil {
				return
			}
			relays.Add(1)
			go func() {
				defer relays.Done()
				defer a.Close()
				raw, e := (&net.Dialer{Timeout: 6 * time.Second}).DialContext(relayCtx, "tcp", camera)
				if e != nil {
					return
				}
				b := tls.Client(raw, production.DeviceTLS(v.pin))
				defer b.Close()
				stop := context.AfterFunc(relayCtx, func() { a.Close(); b.Close() })
				defer stop()
				if e = b.HandshakeContext(relayCtx); e != nil {
					return
				}
				done := make(chan struct{})
				go func() { defer close(done); _ = rtspRelay(a, b, local, remote); b.Close() }()
				_ = rtspRelay(b, a, remote, local)
				a.Close()
				b.Close()
				<-done
			}()
		}
	}()
	u, _ := url.Parse(local + "/streaming/live/1")
	u.User = url.UserPassword("bblp", v.code)
	return ffmpegStream(ctx, u.String(), send)
}

// Rewrite RTSP request targets and content locations, preserving Digest fields and binary RTP packets.
func rtspRelay(from io.Reader, to io.Writer, old, new string) error {
	r := bufio.NewReaderSize(from, 32<<10)
	for {
		b, e := r.Peek(1)
		if e != nil {
			return e
		}
		if b[0] == '$' {
			var header [4]byte
			if _, e = io.ReadFull(r, header[:]); e != nil {
				return e
			}
			if _, e = to.Write(header[:]); e != nil {
				return e
			}
			if _, e = io.CopyN(to, r, int64(binary.BigEndian.Uint16(header[2:]))); e != nil {
				return e
			}
			continue
		}
		var length int64
		size := 0
		for i := 0; ; i++ {
			line, e := r.ReadSlice('\n')
			if e != nil {
				return e
			}
			size += len(line)
			if size > 128<<10 {
				return errors.New("RTSP headers too large")
			}
			text := string(line)
			key, val, _ := strings.Cut(text, ":")
			if i == 0 || strings.EqualFold(key, "Content-Base") || strings.EqualFold(key, "Content-Location") {
				text = strings.ReplaceAll(text, old, new)
			}
			if strings.EqualFold(key, "Content-Length") {
				length, e = strconv.ParseInt(strings.TrimSpace(val), 10, 64)
				if e != nil || length < 0 || length > 16<<20 {
					return errors.New("Invalid RTSP content length")
				}
			}
			if _, e = io.WriteString(to, text); e != nil {
				return e
			}
			if text == "\r\n" || text == "\n" {
				break
			}
		}
		if length > 0 {
			if _, e = io.CopyN(to, r, length); e != nil {
				return e
			}
		}
	}
}
