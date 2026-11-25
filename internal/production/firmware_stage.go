package production

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"bambastic/internal/access"
)

const firmwareLimit int64 = 1 << 30

var firmwareModelKeys = map[string]string{"X1": "x1", "X1C": "x1", "X1E": "x1e", "P1S": "p1", "P1P": "p1", "A1": "a1", "A1MINI": "a1-mini", "H2DPRO": "h2d-pro", "H2C": "h2c", "H2D": "h2d", "H2S": "h2s", "H3C": "h3c", "A2L": "a2l", "X2D": "x2d", "P2S": "p2s"}
var nextScript = regexp.MustCompile(`(?is)<script\b[^>]*\bid=["']__NEXT_DATA__["'][^>]*>(.*?)</script>`)
var buildToken = regexp.MustCompile(`^[A-Za-z0-9_-]{1,96}$`)

type firmwareVersion struct {
	Version  string `json:"version"`
	URL      string `json:"url"`
	Notes    string `json:"release_notes_en"`
	Released string `json:"release_time"`
}

type firmwarePage struct {
	PrinterMap map[string]struct {
		Versions []firmwareVersion `json:"versions"`
	} `json:"printerMap"`
}

func firmwareHost(u *url.URL) bool {
	if u == nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Fragment != "" {
		return false
	}
	h := strings.ToLower(u.Hostname())
	return h == "bambulab.com" || strings.HasSuffix(h, ".bambulab.com") || h == "bambulab.cn" || strings.HasSuffix(h, ".bambulab.cn")
}

func firmwareFile(raw string) string {
	u, e := url.Parse(raw)
	if e != nil || !firmwareHost(u) {
		return ""
	}
	name := path.Base(u.Path)
	if !validFileName(name) || strings.HasPrefix(name, ".") {
		return ""
	}
	switch strings.ToLower(path.Ext(name)) {
	case ".sig", ".bin", ".zip", ".img", ".pkg":
		return name
	}
	return ""
}

func (s *Service) vendor(ctx context.Context, method, raw string) (*http.Response, error) {
	req, e := http.NewRequestWithContext(ctx, method, raw, nil)
	if e != nil || !firmwareHost(req.URL) {
		return nil, errors.New("Invalid vendor address")
	}
	client := http.Client{Transport: s.firmwareTransport, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if !firmwareHost(r.URL) || len(via) > 3 {
			return errors.New("Invalid vendor redirect")
		}
		return nil
	}}
	resp, e := client.Do(req)
	if e != nil {
		return nil, errors.New("Cannot contact the Bambu Lab firmware service")
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("Bambu Lab returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (s *Service) vendorPage(ctx context.Context, raw string) ([]byte, error) {
	resp, e := s.vendor(ctx, "GET", raw)
	if e != nil {
		return nil, e
	}
	defer resp.Body.Close()
	b, e := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if len(b) > 4<<20 {
		return nil, errors.New("Firmware catalog too large")
	}
	return b, e
}

func (s *Service) catalog(ctx context.Context, key string) []firmwareVersion {
	if key == "" {
		return nil
	}
	s.firmwareMu.Lock()
	defer s.firmwareMu.Unlock()
	var saved string
	if s.db.QueryRowContext(ctx, "SELECT versions FROM bb_firmware_catalog WHERE model=? AND until>unixepoch()", key).Scan(&saved) == nil {
		var v []firmwareVersion
		if json.Unmarshal([]byte(saved), &v) == nil {
			return v
		}
	}
	c, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	var page firmwarePage
	var build string
	_ = s.db.QueryRowContext(c, "SELECT versions FROM bb_firmware_catalog WHERE model='build' AND until>unixepoch()").Scan(&build)
	if !buildToken.MatchString(build) {
		b, e := s.vendorPage(c, "https://bambulab.com/en/support/firmware-download/all")
		if e == nil {
			if hit := nextScript.FindSubmatch(b); len(hit) == 2 {
				var root struct {
					Build string `json:"buildId"`
					Props struct {
						Page firmwarePage `json:"pageProps"`
					} `json:"props"`
				}
				if json.Unmarshal(hit[1], &root) == nil {
					page = root.Props.Page
					build = root.Build
					if buildToken.MatchString(build) {
						_, _ = s.db.ExecContext(c, "INSERT OR REPLACE INTO bb_firmware_catalog VALUES('build',?,unixepoch()+900)", build)
					}
				}
			}
		}
	}
	if _, ok := page.PrinterMap[key]; !ok && buildToken.MatchString(build) {
		b, e := s.vendorPage(c, "https://bambulab.com/_next/data/"+build+"/en/support/firmware-download/"+key+".json")
		if e == nil {
			var wrap struct {
				Page firmwarePage `json:"pageProps"`
			}
			if json.Unmarshal(b, &wrap) == nil {
				page = wrap.Page
			}
		} else {
			_, _ = s.db.ExecContext(ctx, "DELETE FROM bb_firmware_catalog WHERE model='build' AND versions=?", build)
		}
	}
	versions := []firmwareVersion{}
	for _, v := range page.PrinterMap[key].Versions {
		if v.Version == "" || versionText.FindString(v.Version) != v.Version {
			continue
		}
		if firmwareFile(v.URL) == "" {
			v.URL = ""
		}
		versions = append(versions, v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version > versions[j].Version })
	ttl := 3600
	if len(versions) == 0 {
		ttl = 90
	}
	_, _ = s.db.ExecContext(ctx, "INSERT OR REPLACE INTO bb_firmware_catalog VALUES(?,?,unixepoch()+?)", key, jstr(versions), ttl)
	return versions
}

func (s *Service) withPackages(ctx context.Context, pr row) row {
	v := s.firmwareInfo(ctx, pr)
	enabled, _ := access.Setting(ctx, s.db, "check_printer_firmware", "true")
	if !flag(enabled) {
		return v
	}
	all := s.catalog(ctx, firmwareModelKeys[modelName(pr["model"])])
	latest := val(v["latest_version"])
	if len(all) > 0 && all[0].Version > latest {
		latest = all[0].Version
		v["latest_version"] = latest
	}
	seen := map[string]bool{}
	out := []row{}
	for _, fv := range all {
		if seen[fv.Version] {
			continue
		}
		seen[fv.Version] = true
		out = append(out, row{"version": fv.Version, "file_available": fv.URL != "", "download_url": nullableString(fv.URL), "release_notes": nullableString(fv.Notes), "release_time": nullableString(fv.Released)})
		if fv.Version == latest {
			v["download_url"] = nullableString(fv.URL)
			if fv.Notes != "" {
				v["release_notes"] = fv.Notes
			}
		}
	}
	if latest != "" && !seen[latest] {
		out = append([]row{{"version": latest, "file_available": false}}, out...)
	}
	v["available_versions"] = out
	current := versionText.FindString(val(v["current_version"]))
	v["update_available"] = current != "" && latest > current
	return v
}

func (s *Service) firmwareLatest(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "firmware:read"); e != nil {
		return nil, e
	}
	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()
	keys := map[string]bool{}
	for _, key := range firmwareModelKeys {
		keys[key] = true
	}
	out := []row{}
	for _, key := range sortedNames(keys) {
		if ctx.Err() != nil {
			break
		}
		vs := s.catalog(ctx, key)
		if len(vs) > 0 {
			v := vs[0]
			out = append(out, row{"model_key": key, "version": v.Version, "download_url": v.URL, "release_notes": nullableString(v.Notes)})
		}
	}
	return out, nil
}

func (s *Service) firmwareReady(pr row, wantSize int64) (row, []string) {
	k := num(pr["id"])
	st := s.state(k, pr)
	sd := flag(st["sdcard"])
	free := int64(-1)
	s.mu.RLock()
	if dev := s.devices[k]; dev != nil && dev.report["sdcard_free_space"] != nil {
		free = num(dev.report["sdcard_free_space"])
	}
	s.mu.RUnlock()
	errs := []string{}
	if !flag(st["connected"]) {
		errs = append(errs, "Printer disconnected")
	} else if !idle(val(st["state"])) {
		errs = append(errs, "Printer is busy")
	}
	if !sd {
		errs = append(errs, "No SD card reported by printer")
	}
	space := sd && (free < 0 || free >= wantSize+(100<<20))
	if sd && !space {
		errs = append(errs, "Insufficient SD card space")
	}
	return row{"sd_card_present": sd, "sd_card_free_space": free, "space_sufficient": space, "current_version": st["firmware_version"]}, errs
}

func (s *Service) prepareFirmware(ctx context.Context, pr row, target string) (row, string, error) {
	if target != "" && versionText.FindString(target) != target {
		return nil, "", fail(422, "Invalid firmware version")
	}
	v := s.withPackages(ctx, pr)
	latest := val(v["latest_version"])
	selected := target
	if selected == "" {
		selected = latest
	}
	raw := ""
	for _, entry := range v["available_versions"].([]row) {
		if entry["version"] == selected {
			raw = val(entry["download_url"])
			break
		}
	}
	name := firmwareFile(raw)
	var size int64
	if name != "" {
		c, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		if resp, e := s.vendor(c, "HEAD", raw); e == nil {
			size = max(resp.ContentLength, 0)
			resp.Body.Close()
		}
	}
	out, errs := s.firmwareReady(pr, size)
	if name == "" {
		errs = append(errs, "Firmware file unavailable from Bambu Lab for this model and version")
	}
	if size > firmwareLimit {
		errs = append(errs, "Firmware file exceeds the size limit")
	}
	available := name != "" && (target != "" || flag(v["update_available"]))
	if name != "" && !available {
		errs = append(errs, "No newer firmware available")
	}
	out["firmware_size"] = size
	out["firmware_filename"] = nullableString(name)
	out["update_available"] = available
	out["latest_version"] = nullableString(latest)
	out["target_version"] = nullableString(selected)
	out["errors"] = errs
	out["can_proceed"] = len(errs) == 0
	return out, raw, nil
}

func (s *Service) stageFirmware(w http.ResponseWriter, r *http.Request) (any, error) {
	perm := "firmware:read"
	if r.Method == "POST" {
		perm = "firmware:update"
	}
	pr, e := s.printerRow(r, perm)
	if e != nil {
		return nil, e
	}
	k := pathID(r)
	ctx := r.Context()
	if strings.HasSuffix(r.URL.Path, "/status") {
		job, e := one(ctx, s.db, "SELECT status,progress,message,error,firmware_filename,firmware_version FROM bb_firmware_jobs WHERE printer_id=?", k)
		if errors.Is(e, sql.ErrNoRows) {
			return row{"status": "idle", "progress": 0, "message": ""}, nil
		}
		return job, e
	}
	report, raw, e := s.prepareFirmware(ctx, pr, r.URL.Query().Get("version"))
	if e != nil {
		return nil, e
	}
	if r.Method == "GET" {
		return report, nil
	}
	if !flag(report["can_proceed"]) {
		return nil, fail(400, strings.Join(report["errors"].([]string), ". "))
	}
	p := access.From(ctx)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var busy int
	if e = tx.QueryRowContext(ctx, "SELECT (SELECT count(*) FROM bb_attempts WHERE printer_id=? AND phase IN ('uploading','sent','running'))+(SELECT count(*) FROM bb_external_run WHERE printer_id=?)+(SELECT count(*) FROM bb_heater_owners WHERE printer_id=?)+(SELECT count(*) FROM bb_keep_warm WHERE printer_id=? AND released=0)", k, k, k, k).Scan(&busy); e != nil {
		return nil, e
	}
	if busy != 0 {
		return nil, fail(409, "Printer has an active job")
	}
	res, e := tx.ExecContext(ctx, `INSERT INTO bb_firmware_jobs(printer_id,model,owner,key_id,status,message,firmware_filename,firmware_version,url) VALUES(?,?,?,?,'preparing','Preparing firmware',?,?,?) ON CONFLICT(printer_id) DO UPDATE SET model=excluded.model,owner=excluded.owner,key_id=excluded.key_id,status=excluded.status,progress=0,message=excluded.message,error=NULL,firmware_filename=excluded.firmware_filename,firmware_version=excluded.firmware_version,url=excluded.url WHERE bb_firmware_jobs.status IN ('complete','error')`, k, modelName(pr["model"]), p.ID, p.KeyID, report["firmware_filename"], report["target_version"], raw)
	if e != nil {
		return nil, e
	}
	n, _ := res.RowsAffected()
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	if n == 0 {
		return row{"started": false, "message": "Firmware upload already in progress"}, nil
	}
	return row{"started": true, "message": "Preparing firmware for the printer SD card"}, nil
}

func (s *Service) firmwareAccess(ctx context.Context, j row, size int64) error {
	k := num(j["printer_id"])
	p, e := s.durableActor(ctx, num(j["owner"]), num(j["key_id"]))
	if e != nil || !p.Has("firmware:update") || !p.AllowsPrinter(k) {
		return errors.New("Firmware update permission revoked")
	}
	pr, e := one(ctx, s.db, "SELECT * FROM printers WHERE id=? AND is_active", k)
	if e != nil {
		return e
	}
	if modelName(pr["model"]) != val(j["model"]) {
		return errors.New("Printer model changed")
	}
	if _, errs := s.firmwareReady(pr, size); len(errs) > 0 {
		return errors.New(strings.Join(errs, ". "))
	}
	return nil
}

func (s *Service) transferFirmware(ctx context.Context, j row) error {
	k := num(j["printer_id"])
	raw := val(j["url"])
	name := firmwareFile(raw)
	if name == "" || name != val(j["firmware_filename"]) {
		return errors.New("Invalid firmware package")
	}
	if e := s.firmwareAccess(ctx, j, 0); e != nil {
		return e
	}
	resp, e := s.vendor(ctx, "GET", raw)
	if e != nil {
		return e
	}
	defer resp.Body.Close()
	if resp.ContentLength > firmwareLimit {
		return errors.New("Firmware file exceeds size limit")
	}
	tmp, e := os.CreateTemp(s.dir, "firmware-*")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hash := sha256.New()
	progress := func(from, span int, total int64) func(int64) {
		last := from
		return func(n int64) {
			if total <= 0 {
				return
			}
			v := from + int(min(n, total)*int64(span)/total)
			if v >= last+5 {
				_, _ = s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET progress=? WHERE printer_id=?", v, k)
				last = v
			}
		}
	}
	n, e := io.Copy(io.MultiWriter(tmp, hash), &progressReader{r: io.LimitReader(resp.Body, firmwareLimit+1), ctx: ctx, f: progress(5, 45, resp.ContentLength)})
	if e != nil || n == 0 || n > firmwareLimit || resp.ContentLength >= 0 && n != resp.ContentLength {
		return errors.New("Incomplete firmware download")
	}
	if e = s.firmwareAccess(ctx, j, n); e != nil {
		return e
	}
	if _, e = tmp.Seek(0, io.SeekStart); e != nil {
		return e
	}
	_, e = s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET status='uploading',message='Transferring firmware to SD card',progress=50 WHERE printer_id=?", k)
	if e != nil {
		return e
	}
	c, e := s.trustedCard(ctx, k)
	if e != nil {
		return e
	}
	defer c.Quit()
	destination := "/" + name
	check := func(remote string) error {
		length, e := c.FileSize(remote)
		if e != nil || length != n {
			return errors.New("Firmware verification failed")
		}
		f, e := c.Open(remote)
		if e != nil {
			return e
		}
		remoteHash := sha256.New()
		count, e := io.Copy(remoteHash, io.LimitReader(f, n+1))
		closeErr := f.Close()
		if e != nil || closeErr != nil || count != n || !bytes.Equal(remoteHash.Sum(nil), hash.Sum(nil)) {
			return errors.New("Firmware verification failed")
		}
		return nil
	}
	if _, e = c.FileSize(destination); e == nil {
		return check(destination)
	}
	partial := destination + ".bambastic.part"
	defer c.Delete(partial)
	if e = c.Stor(partial, &progressReader{r: tmp, ctx: ctx, f: progress(50, 40, n)}); e != nil {
		return errors.New("Writing firmware to the printer failed")
	}
	if e = check(partial); e != nil {
		return e
	}
	if e = s.firmwareAccess(ctx, j, n); e != nil {
		return e
	}
	if _, exists := c.FileSize(destination); exists == nil {
		return check(destination)
	}
	if e = c.Rename(partial, destination); e != nil {
		return errors.New("Publishing verified firmware on SD card failed")
	}
	return nil
}

func (s *Service) tickFirmware(ctx context.Context) {
	j, e := one(ctx, s.db, "SELECT * FROM bb_firmware_jobs WHERE status='preparing' ORDER BY printer_id LIMIT 1")
	if e != nil {
		return
	}
	k := j["printer_id"]
	res, e := s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET status='downloading',message='Downloading firmware from Bambu Lab',progress=5 WHERE printer_id=? AND status='preparing'", k)
	if e != nil {
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Minute)
	e = s.transferFirmware(c, j)
	cancel()
	if e != nil {
		_, _ = s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET status='error',message='Firmware staging failed',error=? WHERE printer_id=?", e.Error(), k)
		return
	}
	_, _ = s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET status='complete',message='Firmware is ready. On the printer, open Settings and select the SD card firmware to install.',progress=100 WHERE printer_id=?", k)
}

func (s *Service) firmwareLoop(ctx context.Context) {
	_, _ = s.db.ExecContext(ctx, "UPDATE bb_firmware_jobs SET status='error',message='Firmware staging interrupted',error='Server restarted during transfer. Start the upload again.' WHERE status IN ('downloading','uploading')")
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.tickFirmware(ctx)
		}
	}
}

func (s *Service) firmwareRoutes(rt register) {
	rt("GET", "/firmware/latest", s.firmwareLatest)
	rt("GET", "/firmware/updates/{id}/prepare", s.stageFirmware)
	rt("POST", "/firmware/updates/{id}/upload", s.stageFirmware)
	rt("GET", "/firmware/updates/{id}/upload/status", s.stageFirmware)
}
