// Package spoolbuddy runs the Raspberry Pi NFC and scale station.
package spoolbuddy

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Config struct {
	Backend, APIKey, DeviceID, Hostname, IPAddress string
	Heartbeat, Sample, WeightReport, TagPoll       time.Duration
	ConfigPath                                     string
}

func Environment() (Config, error) {
	c := Config{Backend: strings.TrimRight(os.Getenv("SPOOLBUDDY_BACKEND_URL"), "/"), APIKey: os.Getenv("SPOOLBUDDY_API_KEY"), DeviceID: os.Getenv("SPOOLBUDDY_DEVICE_ID"), Hostname: os.Getenv("SPOOLBUDDY_HOSTNAME"), Heartbeat: 10 * time.Second, Sample: 100 * time.Millisecond, WeightReport: time.Second, TagPoll: 300 * time.Millisecond}
	c.ConfigPath = os.Getenv("SPOOLBUDDY_CONFIG_FILE")
	if c.ConfigPath == "" {
		c.ConfigPath = "/var/lib/spoolbuddy/config.json"
	}
	if b, err := os.ReadFile(c.ConfigPath); err == nil {
		var v savedConfig
		if json.Unmarshal(b, &v) != nil {
			return c, errors.New("invalid station config file")
		}
		c.Backend, c.APIKey = v.Backend, v.Key
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, err
	}
	if c.Hostname == "" {
		c.Hostname, _ = os.Hostname()
	}
	ifs, _ := net.Interfaces()
	sort.Slice(ifs, func(i, j int) bool { return ifs[i].Name < ifs[j].Name })
	for _, v := range ifs {
		if v.Flags&net.FlagLoopback != 0 || len(v.HardwareAddr) == 0 {
			continue
		}
		if c.DeviceID == "" {
			c.DeviceID = "sb-" + hex.EncodeToString(v.HardwareAddr)
		}
		if c.IPAddress == "" {
			aa, _ := v.Addrs()
			for _, a := range aa {
				ip, _, e := net.ParseCIDR(a.String())
				if e == nil && ip.To4() != nil {
					c.IPAddress = ip.String()
					break
				}
			}
		}
	}
	if c.DeviceID == "" {
		c.DeviceID = "sb-" + c.Hostname
	}
	if c.IPAddress == "" {
		c.IPAddress = "127.0.0.1"
	}
	u, e := url.Parse(c.Backend)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.RawQuery != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return c, errors.New("set SPOOLBUDDY_BACKEND_URL to an HTTP(S) server URL")
	}
	if c.DeviceID == "" || len(c.DeviceID) > 50 || strings.ContainsAny(c.DeviceID, "/\\?#") || c.DeviceID == "." || c.DeviceID == ".." {
		return c, errors.New("invalid SPOOLBUDDY_DEVICE_ID")
	}
	return c, nil
}

type Scale interface {
	Raw() (int, bool, error)
	Close() error
}
type Tag struct {
	UID string
	SAK byte
}
type NFC interface {
	ReadTag() (*Tag, error)
	Close() error
}
type Hardware struct {
	OpenScale func() (Scale, error)
	OpenNFC   func() (NFC, error)
}
type message struct {
	path string
	body map[string]any
}
type calibration struct {
	tare   int
	factor float64
}
type writeTask struct {
	key          string
	spool        int
	short, large []byte
}
type tagWriter interface {
	WriteTag(uid string, short, large []byte) (string, error)
}
type Station struct {
	systemAction           func(context.Context, string) error
	lastTag                string
	write                  *writeTask
	finishedWrite          string
	Config                 Config
	Hardware               Hardware
	client                 *http.Client
	mu                     sync.Mutex
	scaleOK, nfcOK, hasRaw bool
	raw                    int
	cal                    calibration
	queue                  []message
	latest                 *message
	wake                   chan struct{}
	started                time.Time
	backlight              string
	maxBright              int
}

func New(c Config, h Hardware) *Station {
	if c.Heartbeat <= 0 {
		c.Heartbeat = 10 * time.Second
	}
	if c.Sample <= 0 {
		c.Sample = 100 * time.Millisecond
	}
	if c.WeightReport <= 0 {
		c.WeightReport = time.Second
	}
	if c.TagPoll <= 0 {
		c.TagPoll = 300 * time.Millisecond
	}
	s := &Station{Config: c, Hardware: h, systemAction: runSystemAction, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, cal: calibration{factor: 1}, wake: make(chan struct{}, 1)}
	devices, _ := filepath.Glob("/sys/class/backlight/*")
	if len(devices) > 0 {
		v, e := os.ReadFile(filepath.Join(devices[0], "max_brightness"))
		n, err := strconv.Atoi(strings.TrimSpace(string(v)))
		if e == nil && err == nil && n > 0 {
			s.backlight = devices[0]
			s.maxBright = n
		}
	}
	return s
}
func (s *Station) post(ctx context.Context, m message) (map[string]any, error) {
	b, e := json.Marshal(m.body)
	if e != nil {
		return nil, e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", s.Config.Backend+"/api/v1/spoolbuddy/"+m.path, bytes.NewReader(b))
	if e != nil {
		return nil, e
	}
	req.Header.Set("Content-Type", "application/json")
	if s.Config.APIKey != "" {
		req.Header.Set("X-API-Key", s.Config.APIKey)
	}
	res, e := s.client.Do(req)
	if e != nil {
		return nil, errors.New("server connection failed")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, fmt.Errorf("station endpoint %s returned HTTP %d", m.path, res.StatusCode)
	}
	var data map[string]any
	if e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&data); e != nil {
		return nil, e
	}
	return data, nil
}
func (s *Station) push(m message) {
	s.mu.Lock()
	if m.path == "scale/reading" {
		s.latest = &m
	} else {
		if len(s.queue) == 100 {
			s.queue = s.queue[1:]
		}
		s.queue = append(s.queue, m)
	}
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Station) flush(ctx context.Context) error {
	for {
		s.mu.Lock()
		isTag := len(s.queue) > 0
		var m message
		if isTag {
			m = s.queue[0]
		} else if s.latest != nil {
			m = *s.latest
			s.latest = nil
		} else {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		if _, e := s.post(ctx, m); e != nil {
			if !isTag {
				s.mu.Lock()
				if s.latest == nil {
					s.latest = &m
				}
				s.mu.Unlock()
			}
			return e
		}
		if isTag {
			s.mu.Lock()
			if len(s.queue) > 0 && s.queue[0].path == m.path && s.queue[0].body["tag_uid"] == m.body["tag_uid"] {
				s.queue = s.queue[1:]
			}
			s.mu.Unlock()
		}
	}
}
func (s *Station) registration() message {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Config
	return message{"devices/register", map[string]any{"device_id": c.DeviceID, "hostname": c.Hostname, "ip_address": c.IPAddress, "firmware_version": Version, "has_nfc": s.nfcOK, "has_scale": s.scaleOK, "tare_offset": s.cal.tare, "calibration_factor": s.cal.factor, "backend_url": c.Backend, "has_backlight": s.backlight != "", "nfc_reader_type": "PN5180", "nfc_connection": "SPI"}}
}
func (s *Station) heartbeat() message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return message{"devices/" + url.PathEscape(s.Config.DeviceID) + "/heartbeat", map[string]any{"nfc_ok": s.nfcOK, "scale_ok": s.scaleOK, "uptime_s": int(time.Since(s.started).Seconds()), "firmware_version": Version, "ip_address": s.Config.IPAddress, "backend_url": s.Config.Backend, "system_stats": map[string]any{"arch": runtime.GOARCH, "cpu_count": runtime.NumCPU()}}}
}
func (s *Station) apply(ctx context.Context, d map[string]any) error {
	s.mu.Lock()
	if t, ok := d["tare_offset"].(float64); ok {
		s.cal.tare = int(t)
	}
	if f, ok := d["calibration_factor"].(float64); ok && f != 0 && !math.IsInf(f, 0) && !math.IsNaN(f) {
		s.cal.factor = f
	}
	if d["pending_command"] == "write_tag" {
		v, _ := d["pending_write_payload"].(map[string]any)
		short, _ := hex.DecodeString(fmt.Sprint(v["ndef_data_hex"]))
		large, _ := hex.DecodeString(fmt.Sprint(v["ndef_v2_hex"]))
		spool, _ := v["spool_id"].(float64)
		key, _ := v["write_id"].(string)
		if key == "" {
			key = fmt.Sprintf("%d/%x", int(spool), short)
		}
		if spool > 0 && len(short) > 0 && len(short) <= 144 && len(large) <= 888 && s.finishedWrite != key {
			s.write = &writeTask{key, int(spool), short, large}
		}
	} else if _, ok := d["pending_command"]; ok {
		s.write = nil
		s.finishedWrite = ""
	}
	raw, seen := s.raw, s.hasRaw
	s.mu.Unlock()
	if d["pending_command"] == "tare" && seen {
		s.push(message{"devices/" + url.PathEscape(s.Config.DeviceID) + "/calibration/set-tare", map[string]any{"tare_offset": raw}})
		s.mu.Lock()
		s.cal.tare = raw
		s.mu.Unlock()
	}
	if brightness, ok := d["display_brightness"].(float64); ok && s.backlight != "" && brightness >= 0 && brightness <= 100 {
		target := []byte(strconv.Itoa(int(brightness * float64(s.maxBright) / 100)))
		path := filepath.Join(s.backlight, "brightness")
		if current, e := os.ReadFile(path); e == nil && strings.TrimSpace(string(current)) != string(target) {
			if e = os.WriteFile(path, target, 0); e != nil {
				log.Printf("backlight brightness: %v", e)
			}
		}
	}
	return s.applySystem(ctx, d)
}
func (s *Station) Run(ctx context.Context) error {
	s.started = time.Now()
	sensors, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); s.poll(sensors) }()
	defer func() { cancel(); wg.Wait() }()
	registered := false
	backoff := time.Second
	for ctx.Err() == nil {
		var e error
		if !registered {
			var d map[string]any
			d, e = s.post(ctx, s.registration())
			if e == nil {
				registered = true
				e = s.apply(ctx, d)
			}
		}
		if e == nil {
			var d map[string]any
			d, e = s.post(ctx, s.heartbeat())
			if e == nil {
				e = s.apply(ctx, d)
			}
		}
		if e == nil {
			e = s.flush(ctx)
		}
		if e != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("%v; reconnecting in %s", e, backoff)
			registered = false
			if !pause(ctx, backoff) {
				break
			}
			backoff = min(30*time.Second, backoff*2)
			continue
		}
		backoff = time.Second
		timer := time.NewTimer(s.Config.Heartbeat)
	wait:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil
			case <-timer.C:
				break wait
			case <-s.wake:
				if e = s.flush(ctx); e != nil {
					timer.Stop()
					registered = false
					break wait
				}
			}
		}
	}
	return nil
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

type sample struct {
	at time.Time
	g  float64
}

func (s *Station) poll(ctx context.Context) {
	var sc Scale
	var nfc NFC
	var again, nextTag, nextReport time.Time
	var current string
	misses := 0
	var samples []sample
	var prev calibration
	defer func() {
		if sc != nil {
			sc.Close()
		}
		if nfc != nil {
			nfc.Close()
		}
	}()
	for ctx.Err() == nil {
		now := time.Now()
		if !now.Before(again) {
			again = now.Add(10 * time.Second)
			if sc == nil && s.Hardware.OpenScale != nil {
				sc, _ = s.Hardware.OpenScale()
			}
			if nfc == nil && s.Hardware.OpenNFC != nil {
				nfc, _ = s.Hardware.OpenNFC()
			}
			s.mu.Lock()
			s.scaleOK = sc != nil
			s.nfcOK = nfc != nil
			s.mu.Unlock()
		}
		if sc != nil {
			raw, ready, e := sc.Raw()
			if e != nil {
				sc.Close()
				sc = nil
				s.mu.Lock()
				s.scaleOK = false
				s.hasRaw = false
				s.mu.Unlock()
				samples = nil
			} else if ready {
				s.mu.Lock()
				cal := s.cal
				s.raw = raw
				s.hasRaw = true
				s.mu.Unlock()
				if cal != prev {
					samples = nil
					prev = cal
				}
				grams := float64(raw-cal.tare) * cal.factor
				samples = append(samples, sample{now, grams})
				if len(samples) > 20 {
					samples = samples[1:]
				}
				if !now.Before(nextReport) {
					nextReport = now.Add(s.Config.WeightReport)
					total := 0.
					lo, hi := math.Inf(1), math.Inf(-1)
					n := 0
					for _, v := range samples {
						total += v.g
						if now.Sub(v.at) <= time.Second {
							lo = math.Min(lo, v.g)
							hi = math.Max(hi, v.g)
							n++
						}
					}
					s.push(message{"scale/reading", map[string]any{"device_id": s.Config.DeviceID, "weight_grams": math.Round(total/float64(len(samples))*10) / 10, "stable": n >= 5 && hi-lo < 2, "raw_adc": raw}})
				}
			}
		}
		if nfc != nil && !now.Before(nextTag) {
			nextTag = now.Add(s.Config.TagPoll)
			t, e := nfc.ReadTag()
			if e != nil {
				nfc.Close()
				nfc = nil
				s.mu.Lock()
				s.nfcOK = false
				s.mu.Unlock()
			} else if t == nil {
				misses++
				if misses >= 3 && current != "" {
					s.push(message{"nfc/tag-removed", map[string]any{"device_id": s.Config.DeviceID, "tag_uid": current}})
					current = ""
					s.mu.Lock()
					s.lastTag = ""
					s.mu.Unlock()
				}
			} else {
				misses = 0
				if current != t.UID {
					if current != "" {
						s.push(message{"nfc/tag-removed", map[string]any{"device_id": s.Config.DeviceID, "tag_uid": current}})
					}
					current = t.UID
					s.mu.Lock()
					s.lastTag = t.UID
					s.mu.Unlock()
					event := map[string]any{"device_id": s.Config.DeviceID, "tag_uid": current, "sak": int(t.SAK)}
					if reader, ok := nfc.(tagMetadataReader); ok {
						for k, v := range reader.TagMetadata(t) {
							event[k] = v
						}
					}
					s.push(message{"nfc/tag-scanned", event})
				}
				s.mu.Lock()
				task := s.write
				s.write = nil
				if task != nil {
					s.finishedWrite = task.key
				}
				s.mu.Unlock()
				if task != nil {
					success := false
					msg := "The presented tag is not writable"
					if t.SAK == 0 || t.SAK == 4 {
						if w, ok := nfc.(tagWriter); ok {
							result, e := w.WriteTag(t.UID, task.short, task.large)
							if e != nil {
								msg = e.Error()
							} else {
								success = true
								msg = result
							}
						}
					}
					body := map[string]any{"device_id": s.Config.DeviceID, "spool_id": task.spool, "tag_uid": t.UID, "success": success, "message": msg}
					if !strings.Contains(task.key, "/") {
						body["write_id"] = task.key
					}
					s.push(message{"nfc/write-result", body})
				}

			}
		}
		if !pause(ctx, s.Config.Sample) {
			break
		}
	}
}
