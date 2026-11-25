package production

import (
	"bambastic/internal/access"
	"context"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

var versionAnchor = regexp.MustCompile(`(?i)id=["'](?:h-|ota-)([0-9]{2})([0-9]{2})([0-9]{2})([0-9]{2})(?:-?[0-9]{8})?["']`)
var versionText = regexp.MustCompile(`\b[0-9]{2}\.[0-9]{2}\.[0-9]{2}\.[0-9]{2}\b`)
var officialHistory = map[string]string{"X1": "/en/x1/manual/X1-X1C-firmware-release-history", "X1C": "/en/x1/manual/X1-X1C-firmware-release-history", "X1E": "/en/x1/manual/X1E-firmware-release-history", "P1P": "/en/p1/manual/p1p-firmware-release-history", "P1S": "/en/p1/manual/p1p-firmware-release-history", "A1": "/en/a1/manual/a1-firmware-release-history", "A1MINI": "/en/a1-mini/manual/a1-mini-firmware-release-history", "H2D": "/en/h2d/manual/h2d-firmware-release-history", "H2C": "/en/h2c/manual/h2c-firmware-release-history", "H2S": "/en/h2s/manual/h2s-firmware-release-history", "P2S": "/en/p2s/manual/p2s-firmware-release-history", "X2D": "/en/x2d/manual/x2d-firmware-release-history", "H2DPRO": "/en/h2d-pro/manual/firmware-release-history", "H3C": "/en/h3c/manual/firmware-release-history", "A2L": "/en/a2l/manual/a2l-firmware-release-history"}

func releaseVersions(page string) []string {
	unique := map[string]bool{}
	for _, m := range versionAnchor.FindAllStringSubmatch(page, -1) {
		unique[strings.Join(m[1:], ".")] = true
	}
	if len(unique) == 0 {
		for _, v := range versionText.FindAllString(page, -1) {
			unique[v] = true
		}
	}
	out := sortedNames(unique)
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out
}
func (s *Service) firmwareInfo(ctx context.Context, pr row) row {
	k := num(pr["id"])
	current := val(s.state(k, pr)["firmware_version"])
	model := modelName(pr["model"])
	out := row{"printer_id": k, "printer_name": pr["name"], "model": pr["model"], "current_version": nullableString(current), "latest_version": nil, "update_available": false, "available_versions": []row{}}
	enabled, _ := access.Setting(ctx, s.db, "check_printer_firmware", "true")
	path := officialHistory[model]
	if !flag(enabled) || path == "" {
		return out
	}
	cached, e := one(ctx, s.db, "SELECT version FROM bb_firmware_cache WHERE model=? AND until>unixepoch()", model)
	if e != nil {
		req, e := http.NewRequestWithContext(ctx, http.MethodGet, "https://wiki.bambulab.com"+path, nil)
		if e != nil {
			return out
		}
		client := http.Client{Timeout: 4 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Host != "wiki.bambulab.com" || req.URL.Scheme != "https" || len(via) > 3 {
				return http.ErrUseLastResponse
			}
			return nil
		}}
		latest := ""
		resp, e := client.Do(req)
		if e == nil {
			if resp.StatusCode == 200 {
				data, e := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
				if e == nil && len(data) <= 2<<20 {
					versions := releaseVersions(string(data))
					if len(versions) > 0 {
						latest = versions[0]
					}
				}
			}
			resp.Body.Close()
		}
		duration := 3600
		if latest == "" {
			duration = 90
		}
		s.db.ExecContext(ctx, "INSERT INTO bb_firmware_cache VALUES(?,?,unixepoch()+?) ON CONFLICT(model) DO UPDATE SET version=excluded.version,until=excluded.until", model, latest, duration)
		cached = row{"version": latest}
	}
	latest := val(cached["version"])
	out["latest_version"] = nullableString(latest)
	if latest != "" {
		out["release_notes"] = "https://wiki.bambulab.com" + path
		out["update_available"] = versionText.FindString(current) != "" && versionText.FindString(current) < latest
	}
	return out
}
func (s *Service) firmware(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "firmware:read"); e != nil {
		return nil, e
	}
	if pathID(r) > 0 {
		pr, e := s.printerRow(r, "firmware:read")
		if e != nil {
			return nil, e
		}
		return s.withPackages(r.Context(), pr), nil
	}
	rs, e := records(r.Context(), s.db, "SELECT * FROM printers WHERE is_active ORDER BY id")
	if e != nil {
		return nil, e
	}
	out := []row{}
	n := 0
	for _, pr := range rs {
		if access.From(r.Context()).AllowsPrinter(num(pr["id"])) {
			v := s.firmwareInfo(r.Context(), pr)
			if flag(v["update_available"]) {
				n++
			}
			out = append(out, v)
		}
	}
	return row{"updates": out, "updates_available": n, "total_printers": len(out)}, nil
}
