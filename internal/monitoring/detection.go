package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strings"
	"time"
)

const gadgetCreate = "https://gadget-pv1-oeapi.octoeverywhere.com/api/gadget/v1/createcontext"

type detectorSettings struct {
	kind, host, key, callback, level, action string
	enabled                                  bool
	interval                                 int
	printers                                 []int64
}

func (c detectorSettings) fingerprint() string {
	raw, _ := json.Marshal([]any{c.kind, c.host, c.key, c.callback, c.level, c.action, c.enabled, c.interval, c.printers})
	return fmt.Sprintf("%x", sha256.Sum256(raw))
}
func (c detectorSettings) allows(id int64) bool {
	if c.printers == nil {
		return true
	}
	for _, i := range c.printers {
		if id == i {
			return true
		}
	}
	return false
}
func (s *Service) detectorSettings(ctx context.Context, kind string) (detectorSettings, error) {
	c := detectorSettings{kind: kind, level: "medium", action: "notify", interval: 10}
	rs, err := s.db.QueryContext(ctx, `SELECT key,value FROM settings WHERE key LIKE 'obico_%' OR key LIKE 'octoeverywhere_%' OR key IN ('bambuddy_internal_url','external_url')`)
	if err != nil {
		return c, err
	}
	kv := map[string]string{}
	for rs.Next() {
		var k, v string
		if err = rs.Scan(&k, &v); err != nil {
			rs.Close()
			return c, err
		}
		kv[k] = v
	}
	err = rs.Err()
	rs.Close()
	if err != nil {
		return c, err
	}
	enabled := func(k string) bool { return strings.EqualFold(kv[k+"_enabled"], "true") }
	c.enabled = enabled(kind) && !(enabled("obico") && enabled("octoeverywhere"))
	if list := kv[kind+"_enabled_printers"]; list != "" {
		if json.Unmarshal([]byte(list), &c.printers) != nil || c.printers == nil {
			c.printers = []int64{}
		}
	}
	if v := kv[kind+"_action"]; v != "" {
		c.action = v
	}
	lvlKey := "obico_sensitivity"
	secretKey := "obico_ml_token"
	if kind == "octoeverywhere" {
		c.interval = 20
		lvlKey = "octoeverywhere_confidence"
		secretKey = "octoeverywhere_api_key"
	}
	if n := int(num(kv[kind+"_poll_interval"])); n >= 5 && n <= 3600 {
		c.interval = n
	}
	if v := kv[lvlKey]; v != "" {
		c.level = v
	}
	c.key, err = s.auth.OpenSecret(kv[secretKey])
	c.host = strings.TrimRight(strings.TrimSpace(kv["obico_ml_url"]), "/")
	c.callback = strings.TrimRight(strings.TrimSpace(kv["bambuddy_internal_url"]), "/")
	if c.callback == "" {
		c.callback = strings.TrimRight(strings.TrimSpace(kv["external_url"]), "/")
	}
	return c, err
}

type cachedFrame struct {
	jpeg    []byte
	expires time.Time
}
type detectorService struct {
	powerOff func(context.Context, int64, func() bool) error
	client   *http.Client
	running  bool
	status   map[string]map[int64]object
	history  map[string][]object
	frames   map[string]cachedFrame
}

func (s *Service) registerDetection(public, api *http.ServeMux) {
	s.detectors.client = lanhttp.Client
	s.detectors.status = map[string]map[int64]object{}
	s.detectors.history = map[string][]object{}
	s.detectors.frames = map[string]cachedFrame{}
	for _, k := range []string{"obico", "octoeverywhere"} {
		for _, action := range []string{"status", "printer-status", "test-connection"} {
			method := "GET"
			if action == "test-connection" {
				method = "POST"
			}
			api.HandleFunc(method+" /api/v1/"+k+"/"+action, func(w http.ResponseWriter, r *http.Request) { s.detectionHTTP(w, r, k, action) })
		}
	}
	route := "GET /api/v1/obico/cached-frame/{nonce}"
	h := func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		key := r.PathValue("nonce")
		frame, ok := s.detectors.frames[key]
		delete(s.detectors.frames, key)
		s.mu.Unlock()
		if !ok || time.Now().After(frame.expires) {
			httpx.Error(w, 404, "Frame expired")
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(frame.jpeg)
	}
	public.HandleFunc(route, h)
	if public != api {
		api.HandleFunc(route, h)
	}
}
func validBase(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func gadgetURL(raw string) bool {
	u, e := url.Parse(raw)
	return e == nil && u.Scheme == "https" && strings.HasSuffix(strings.ToLower(u.Hostname()), ".octoeverywhere.com") && (u.Port() == "" || u.Port() == "443") && u.User == nil && u.Fragment == ""
}

type remoteResult struct {
	status  int
	body    []byte
	backoff time.Duration
}

func (s *Service) mlRequest(ctx context.Context, method, target, header, key, mime string, b []byte) (remoteResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(b))
	if e != nil {
		return remoteResult{}, errors.New("Invalid detection URL")
	}
	if key != "" {
		req.Header.Set(header, key)
	}
	if mime != "" {
		req.Header.Set("Content-Type", mime)
	}
	res, e := s.detectors.client.Do(req)
	if e != nil {
		return remoteResult{}, errors.New("Detection service unavailable")
	}
	defer res.Body.Close()
	body, e := io.ReadAll(io.LimitReader(res.Body, 1<<20+1))
	r := remoteResult{status: res.StatusCode, body: body}
	if n := num(res.Header.Get("Retry-After")); n > 0 {
		r.backoff = time.Duration(min(n, 86400)) * time.Second
	}
	if e != nil || len(body) > 1<<20 {
		return r, errors.New("Invalid detection response")
	}
	return r, nil
}
func (s *Service) gadgetContext(ctx context.Context, key, level string) (string, remoteResult, error) {
	l := map[string]int{"lowest": 1, "low": 2, "medium": 3, "high": 4, "highest": 5}[level]
	if l == 0 {
		l = 3
	}
	if key == "" {
		return "", remoteResult{}, errors.New("Gadget API key required")
	}
	b, _ := json.Marshal(object{"WarningConfidenceLevel": l, "PauseConfidenceLevel": l})
	r, e := s.mlRequest(ctx, "POST", gadgetCreate, "X-API-Key", key, "application/json", b)
	if e != nil {
		return "", r, e
	}
	var o struct{ ContextId, ProcessRequestUrl, FallbackProcessRequestUrl string }
	if r.status/100 != 2 || json.Unmarshal(r.body, &o) != nil || o.ContextId == "" || !gadgetURL(o.ProcessRequestUrl) || !gadgetURL(o.FallbackProcessRequestUrl) {
		return "", r, fmt.Errorf("Gadget rejected the context request (HTTP %d)", r.status)
	}
	return o.ProcessRequestUrl, r, nil
}
func (s *Service) detectionHTTP(w http.ResponseWriter, r *http.Request, kind, action string) {
	perm := map[string]string{"status": "settings:read", "printer-status": "printers:read", "test-connection": "settings:update"}[action]
	if !access.Require(w, r, perm) {
		return
	}
	p := access.From(r.Context())
	c, e := s.detectorSettings(r.Context(), kind)
	if e != nil {
		httpx.Error(w, 503, "Detection configuration unavailable")
		return
	}
	if action == "test-connection" {
		var input struct {
			URL        string
			Token      *string
			APIKey     *string `json:"api_key"`
			Confidence string
		}
		if !httpx.Decode(w, r, &input) {
			return
		}
		out := object{"ok": false, "error": nil, "status_code": nil, "error_code": nil}
		if kind == "octoeverywhere" {
			if input.APIKey != nil && *input.APIKey != "********" {
				c.key = *input.APIKey
			}
			if input.Confidence != "" {
				c.level = input.Confidence
			}
			_, res, err := s.gadgetContext(r.Context(), c.key, c.level)
			if res.status != 0 {
				out["status_code"] = res.status
			}
			if err != nil {
				out["error"] = err.Error()
			} else {
				out["ok"] = true
			}
		} else {
			out["body"] = nil
			out["auth_ok"] = nil
			target := strings.TrimRight(strings.TrimSpace(input.URL), "/")
			if !validBase(target) {
				httpx.Error(w, 422, "A valid HTTP service URL is required")
				return
			}
			if input.Token != nil && *input.Token != "********" {
				c.key = *input.Token
			} else if c.host != target && c.key != "" {
				httpx.Error(w, 400, "Enter the token again after changing the URL")
				return
			}
			bearer := ""
			if c.key != "" {
				bearer = "Bearer " + c.key
			}
			res, err := s.mlRequest(r.Context(), "GET", target+"/hc/", "Authorization", bearer, "", nil)
			if err != nil {
				out["error"] = err.Error()
			} else {
				out["status_code"] = res.status
				out["body"] = strings.TrimSpace(string(res.body[:min(len(res.body), 512)]))
				if res.status == 200 && strings.EqualFold(strings.TrimSpace(string(res.body)), "ok") {
					probe, err := s.mlRequest(r.Context(), "GET", target+"/p/", "Authorization", bearer, "", nil)
					if err != nil {
						out["error"] = err.Error()
					} else {
						ok := probe.status != 401 && probe.status != 403 && probe.status < 500
						out["auth_ok"] = ok
						out["ok"] = ok
						if !ok {
							out["status_code"] = probe.status
							out["error"] = "ML service rejected the authentication probe"
						}
					}
				}
			}
		}
		httpx.JSON(w, 200, out)
		return
	}
	states := map[int64]object{}
	history := []object{}
	var last any
	s.mu.Lock()
	running := s.detectors.running
	for id, st := range s.detectors.status[kind] {
		if p.AllowsPrinter(id) {
			v := object{}
			for k, val := range st {
				v[k] = val
			}
			if v["error"] != nil {
				if !p.Has("settings:read") {
					v["error"] = nil
				} else {
					last = v["error"]
				}
			}
			states[id] = v
		}
	}
	for _, evt := range s.detectors.history[kind] {
		if p.AllowsPrinter(num(asString(evt["printer_id"]))) {
			history = append(history, evt)
		}
	}
	s.mu.Unlock()
	var allowed []int64
	if c.printers != nil {
		allowed = []int64{}
		for _, id := range c.printers {
			if p.AllowsPrinter(id) {
				allowed = append(allowed, id)
			}
		}
		sort.Slice(allowed, func(i, j int) bool { return allowed[i] < allowed[j] })
	}
	out := object{"enabled": c.enabled, "monitored_printers": allowed, "per_printer": states, "last_error": last, "last_error_code": nil}
	if action == "status" {
		out["history"] = history
		out["is_running"] = c.enabled && running
		out["poll_interval"] = c.interval
		out["action"] = c.action
		if kind == "obico" {
			m := sensitivity(c.level)
			out["ml_url"] = c.host
			out["sensitivity"] = c.level
			out["thresholds"] = object{"low": .38 * m, "high": .78 * m, "pause": .78 * m * 1.75}
			out["external_url_configured"] = c.callback != ""
		} else {
			out["confidence"] = c.level
			out["api_key_configured"] = c.key != ""
			out["notifications"] = s.covered(r.Context(), p, c)
		}
	}
	httpx.JSON(w, 200, out)
}
func (s *Service) covered(ctx context.Context, p access.Principal, c detectorSettings) object {
	cover := map[int64]bool{}
	overall := false
	missing := []int64{}
	rows, e := s.db.QueryContext(ctx, "SELECT printer_id FROM notification_providers WHERE enabled AND on_ai_failure_detection")
	if e == nil {
		for rows.Next() {
			var id sql.NullInt64
			if rows.Scan(&id) == nil {
				if !id.Valid {
					overall = true
				} else if p.AllowsPrinter(id.Int64) {
					cover[id.Int64] = true
				}
			}
		}
		rows.Close()
	}
	if !overall {
		rows, e = s.db.QueryContext(ctx, "SELECT id FROM printers WHERE is_active ORDER BY id")
		if e == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil && !cover[id] && p.AllowsPrinter(id) && c.allows(id) {
					missing = append(missing, id)
				}
			}
			rows.Close()
		}
	}
	return object{"configured": overall || len(cover) > 0, "uncovered_printers": missing}
}

type rolling struct {
	vals   []float64
	cursor int
	sum    float64
}

func (s *rolling) add(x float64, max int) float64 {
	if len(s.vals) < max {
		s.vals = append(s.vals, x)
	} else {
		s.sum -= s.vals[s.cursor]
		s.vals[s.cursor] = x
		s.cursor = (s.cursor + 1) % max
	}
	s.sum += x
	return s.sum / float64(len(s.vals))
}

type inspection struct {
	run                         int64
	task, name, config, process string
	next                        time.Time
	count                       int
	ewm                         float64
	short                       rolling
	long                        *rolling
	fired, warned               bool
	lastNotice                  time.Time
}

func sensitivity(s string) float64 {
	switch s {
	case "low":
		return 1.25
	case "high":
		return .75
	}
	return 1
}
func (i *inspection) classify(conf float64, level string) (float64, string) {
	i.count++
	if i.count == 1 {
		i.ewm = conf
	} else {
		i.ewm = (2*conf + 11*i.ewm) / 13
	}
	short := i.short.add(conf, 310)
	long := i.long.add(conf, 7200)
	score := i.ewm - long
	m := sensitivity(level)
	fail := func(f float64) bool {
		v := score / f
		return i.count > 30 && v >= .38*m && (v > .78*m || v > (short-long)*3.8*m)
	}
	if fail(1.75) {
		return score, "failure"
	}
	if fail(1) {
		return score, "warning"
	}
	return score, "safe"
}

func (s *Service) runDetection(ctx context.Context) {
	s.mu.Lock()
	s.detectors.running = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.detectors.running = false; clear(s.detectors.frames); s.mu.Unlock() }()
	jobs := map[string]map[int64]*inspection{"obico": {}, "octoeverywhere": {}}
	baseline := map[int64]*rolling{}
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	for ctx.Err() == nil {
		for _, kind := range []string{"obico", "octoeverywhere"} {
			c, err := s.detectorSettings(ctx, kind)
			if err != nil {
				continue
			}
			s.inspectFarm(ctx, c, jobs[kind], baseline)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
func (s *Service) inspectFarm(ctx context.Context, c detectorSettings, jobs map[int64]*inspection, baseline map[int64]*rolling) {
	states := []map[string]any{}
	if c.enabled && s.farm != nil {
		var err error
		states, err = s.farm.InitialStatus(ctx, access.Principal{Admin: true})
		if err != nil {
			return
		}
	}
	active := map[int64]bool{}
	for _, v := range states {
		id := num(asString(v["id"]))
		state := strings.ToUpper(asString(v["state"]))
		if id == 0 || !c.allows(id) || v["connected"] != true || state != "RUNNING" && state != "PAUSE" && state != "PAUSED" {
			continue
		}
		active[id] = true
		task, name := asString(v["subtask_id"]), asString(v["subtask_name"])
		finger := c.fingerprint()
		i := jobs[id]
		if i == nil || i.task != task || i.name != name || i.config != finger {
			if baseline[id] == nil {
				baseline[id] = &rolling{}
			}
			i = &inspection{run: s.activePrint(ctx, id, task), task: task, name: name, config: finger, long: baseline[id]}
			jobs[id] = i
			s.storeVerdict(c.kind, id, object{"class": "unknown", "frame_count": 0, "error": nil}, nil)
		}
		if state != "RUNNING" || time.Now().Before(i.next) {
			continue
		}
		i.next = time.Now().Add(time.Duration(c.interval) * time.Second)
		st, evt, err := s.inspect(ctx, c, id, i)
		// A response may arrive after the operator disables detection or starts a
		// new job. Invalidate both the verdict and potential action in that case.
		if !s.currentInspection(ctx, c, id, i) {
			continue
		}
		if err != nil {
			st = object{"class": "error", "frame_count": i.count, "error": err.Error(), "error_code": nil}
		} else if act, ok := s.farm.(interface {
			OnDetection(context.Context, int64, string, string, string, string, string) error
		}); ok {
			verdict := asString(st["class"])
			mode := ""
			if verdict == "failure" && !i.fired {
				mode = c.action
				i.fired = true
			} else if (verdict == "warning" || verdict == "failure") && (!i.warned || c.kind == "octoeverywhere" && time.Since(i.lastNotice) >= 10*time.Minute) {
				mode = "notify"
			}
			if mode != "" {
				i.warned = true
				i.lastNotice = time.Now()
				if err = act.OnDetection(ctx, id, i.task, i.name, mode, c.kind, verdict); err != nil {
					st["error"] = err.Error()
				}
				if err == nil && mode == "pause_and_off" {
					if offErr := s.detectorPower(ctx, c, id, i); offErr != nil {
						st["error"] = offErr.Error()
					}
				}
				if evt != nil {
					evt["action"] = mode
				}
			}
		}
		s.storeVerdict(c.kind, id, st, evt)
	}
	for id := range jobs {
		if !active[id] {
			delete(jobs, id)
			s.mu.Lock()
			delete(s.detectors.status[c.kind], id)
			s.mu.Unlock()
		}
	}
}
func (s *Service) storeVerdict(kind string, id int64, st, evt object) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.detectors.status[kind] == nil {
		s.detectors.status[kind] = map[int64]object{}
	}
	s.detectors.status[kind][id] = st
	if evt != nil {
		old := s.detectors.history[kind]
		s.detectors.history[kind] = append([]object{evt}, old[:min(199, len(old))]...)
	}
}
func (s *Service) currentInspection(ctx context.Context, c detectorSettings, id int64, i *inspection) bool {
	latest, err := s.detectorSettings(ctx, c.kind)
	if err != nil || latest.fingerprint() != i.config || !latest.enabled {
		return false
	}
	states, e := s.farm.InitialStatus(ctx, access.Principal{Admin: true})
	if e != nil {
		return false
	}
	for _, v := range states {
		if num(asString(v["id"])) == id {
			return v["connected"] == true && strings.EqualFold(asString(v["state"]), "RUNNING") && asString(v["subtask_id"]) == i.task && asString(v["subtask_name"]) == i.name
		}
	}
	return false
}
func (s *Service) firstFrame(ctx context.Context, v source) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	_, ch, release := s.subscribe(v)
	defer release()
	select {
	case <-ctx.Done():
		return nil, errors.New("Camera frame unavailable")
	case b, ok := <-ch:
		if !ok || len(b) == 0 {
			return nil, errors.New("Camera did not return a frame")
		}
		return b, nil
	}
}
func (s *Service) inspect(ctx context.Context, c detectorSettings, id int64, i *inspection) (object, object, error) {
	if c.kind == "obico" && (!validBase(c.host) || !validBase(c.callback)) {
		return nil, nil, errors.New("Configure an ML URL and a callback URL reachable by Obico")
	}
	if c.kind == "octoeverywhere" && c.key == "" {
		return nil, nil, errors.New("Gadget API key required")
	}
	r, _ := http.NewRequestWithContext(access.With(ctx, access.Principal{Admin: true}), "GET", "/", nil)
	r.SetPathValue("id", fmt.Sprint(id))
	src, e := s.device(r)
	if e != nil {
		return nil, nil, errors.New("Printer camera unavailable")
	}
	frame, e := s.firstFrame(ctx, src)
	if e != nil {
		return nil, nil, e
	}
	if !s.currentInspection(ctx, c, id, i) {
		return nil, nil, errors.New("Inspection configuration changed")
	}
	st := object{"class": "unknown", "error": nil, "error_code": nil}
	evt := object{"printer_id": id, "task_name": i.name, "timestamp": time.Now().UTC()}
	if c.kind == "obico" {
		key, _ := tokenSecret()
		s.mu.Lock()
		s.detectors.frames[key] = cachedFrame{frame, time.Now().Add(30 * time.Second)}
		s.mu.Unlock()
		defer func() { s.mu.Lock(); delete(s.detectors.frames, key); s.mu.Unlock() }()
		bearer := ""
		if c.key != "" {
			bearer = "Bearer " + c.key
		}
		res, err := s.mlRequest(ctx, "GET", c.host+"/p/?"+url.Values{"img": {c.callback + "/api/v1/obico/cached-frame/" + key}}.Encode(), "Authorization", bearer, "", nil)
		if err != nil {
			return nil, nil, err
		}
		if res.status/100 != 2 {
			return nil, nil, fmt.Errorf("Obico inspection failed (HTTP %d)", res.status)
		}
		var payload struct {
			Detections [][]json.RawMessage `json:"detections"`
		}
		if json.Unmarshal(res.body, &payload) != nil || payload.Detections == nil {
			return nil, nil, errors.New("Obico returned an invalid assessment")
		}
		var total float64
		for _, det := range payload.Detections {
			if len(det) < 2 {
				continue
			}
			var score float64
			if json.Unmarshal(det[1], &score) == nil && !math.IsNaN(score) && !math.IsInf(score, 0) {
				total += score
			}
		}
		score, verdict := i.classify(total, c.level)
		st["class"] = verdict
		st["score"] = math.Max(0, score)
		evt["current_p"] = total
		evt["score"] = st["score"]
		evt["class"] = verdict
		evt["detections"] = len(payload.Detections)
		if len(payload.Detections) == 0 && verdict == "safe" {
			evt = nil
		}
	} else {
		frame, e = gadgetFrame(frame)
		if e != nil {
			return nil, nil, e
		}
		if i.process == "" {
			u, r, e := s.gadgetContext(ctx, c.key, c.level)
			if e != nil {
				i.backoff(r)
				return nil, nil, e
			}
			i.process = u
		}
		if !s.currentInspection(ctx, c, id, i) {
			return nil, nil, errors.New("Inspection configuration changed")
		}
		var body bytes.Buffer
		m := multipart.NewWriter(&body)
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", `form-data; name="image"; filename="snapshot.jpg"`)
		h.Set("Content-Type", "image/jpeg")
		part, e := m.CreatePart(h)
		if e != nil {
			return nil, nil, e
		}
		_, _ = part.Write(frame)
		_ = m.Close()
		res, e := s.mlRequest(ctx, "POST", i.process, "X-API-Key", c.key, m.FormDataContentType(), body.Bytes())
		if e != nil {
			return nil, nil, e
		}
		if res.status/100 != 2 {
			i.backoff(res)
			var problem struct{ ErrorType, ErrorDetails string }
			if json.Unmarshal(res.body, &problem) == nil && problem.ErrorType == "OE_INTERNAL_ERROR" && problem.ErrorDetails == "Failed to get context object." {
				i.process = ""
			}
			return nil, nil, fmt.Errorf("Gadget inspection failed (HTTP %d)", res.status)
		}
		var v struct {
			PrintQuality                     int
			WarningSuggested, PauseSuggested *bool
			FasterInspectionSuggested        bool
			NextProcessIntervalSec           struct{ Minimum, Recommended int }
		}
		if json.Unmarshal(res.body, &v) != nil || v.NextProcessIntervalSec.Minimum < 1 || v.NextProcessIntervalSec.Minimum > 86400 {
			return nil, nil, errors.New("Gadget returned an invalid processing interval")
		}
		interval := c.interval
		if v.FasterInspectionSuggested && v.NextProcessIntervalSec.Recommended > 0 {
			interval = min(interval, v.NextProcessIntervalSec.Recommended)
		}
		i.next = time.Now().Add(time.Duration(max(interval, v.NextProcessIntervalSec.Minimum)) * time.Second)
		if v.PrintQuality < 1 || v.PrintQuality > 10 || v.WarningSuggested == nil || v.PauseSuggested == nil {
			return nil, nil, errors.New("Gadget returned an invalid assessment")
		}
		verdict := "safe"
		if *v.PauseSuggested {
			verdict = "failure"
		} else if *v.WarningSuggested {
			verdict = "warning"
		}
		i.count++
		st["print_quality"] = v.PrintQuality
		st["class"] = verdict
		evt["print_quality"] = v.PrintQuality
		evt["class"] = verdict
		if verdict == "safe" {
			evt = nil
		}
	}
	st["frame_count"] = i.count
	return st, evt, nil
}
func (i *inspection) backoff(r remoteResult) {
	wait := r.backoff
	if r.status == 401 || r.status == 403 {
		wait = max(wait, 15*time.Minute)
	} else if r.status == 429 {
		wait = max(wait, time.Minute)
	}
	if next := time.Now().Add(wait); next.After(i.next) {
		i.next = next
	}
}

func (s *Service) SetDetectorPowerOff(f func(context.Context, int64, func() bool) error) {
	s.detectors.powerOff = f
}
func (s *Service) activePrint(ctx context.Context, id int64, task string) int64 {
	if task == "" || task == "0" {
		return 0
	}
	var run int64
	_ = s.db.QueryRowContext(ctx, "SELECT id FROM print_log_entries WHERE printer_id=? AND subtask_id=? AND completed_at IS NULL ORDER BY id DESC LIMIT 1", id, task).Scan(&run)
	return run
}
func (s *Service) detectorPower(ctx context.Context, c detectorSettings, id int64, i *inspection) error {
	if s.detectors.powerOff == nil {
		return errors.New("No printer power controller is available")
	}
	guard := func() bool {
		if i.run == 0 || s.activePrint(ctx, id, i.task) != i.run {
			return false
		}
		current, e := s.detectorSettings(ctx, c.kind)
		if e != nil || !current.enabled || current.action != "pause_and_off" || current.fingerprint() != i.config || !current.allows(id) {
			return false
		}
		states, e := s.farm.InitialStatus(ctx, access.Principal{Admin: true})
		if e != nil {
			return false
		}
		for _, st := range states {
			if num(asString(st["id"])) != id {
				continue
			}
			state := strings.ToUpper(asString(st["state"]))
			return st["connected"] == true && (state == "RUNNING" || state == "PAUSE" || state == "PAUSED") && asString(st["subtask_id"]) == i.task && asString(st["subtask_name"]) == i.name
		}
		return false
	}
	return s.detectors.powerOff(ctx, id, guard)
}
func gadgetFrame(b []byte) ([]byte, error) {
	if len(b) <= 6<<20 {
		return b, nil
	}
	cfg, e := jpeg.DecodeConfig(bytes.NewReader(b))
	if e != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 64_000_000 {
		return nil, errors.New("Cannot process oversized camera image")
	}
	img, e := jpeg.Decode(bytes.NewReader(b))
	if e != nil {
		return nil, errors.New("Cannot decode camera image")
	}
	small := scaled(img, 2048)
	for _, quality := range []int{90, 80, 70} {
		var out bytes.Buffer
		if e = jpeg.Encode(&out, small, &jpeg.Options{Quality: quality}); e == nil && out.Len() <= 6<<20 {
			return out.Bytes(), nil
		}
	}
	return nil, errors.New("Gadget image remains too large")
}
