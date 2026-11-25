package support

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/lanhttp"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (s *Service) bugRoutes(m *http.ServeMux) {
	for _, a := range []string{"start-logging", "stop-logging", "submit"} {
		m.HandleFunc("POST /api/v1/bug-report/"+a, func(w http.ResponseWriter, r *http.Request) {
			if a == "submit" {
				s.submitReport(w, r)
				return
			}
			if !access.Require(w, r, "settings:update") {
				return
			}
			was := s.debug.Load()
			on := a == "start-logging"
			if !on {
				on, _ = strconv.ParseBool(r.URL.Query().Get("was_debug"))
			}
			tx, e := s.db.BeginTx(r.Context(), nil)
			if e == nil {
				defer tx.Rollback()
				e = access.Put(tx, "debug_logging_enabled", strconv.FormatBool(on))
				if e == nil && on && !was {
					e = access.Put(tx, "debug_logging_enabled_at", time.Now().UTC().Format(time.RFC3339))
				}
				if e == nil {
					e = tx.Commit()
				}
			}
			if e != nil {
				httpx.Error(w, 500, "Cannot change diagnostic logging")
				return
			}
			s.debug.Store(on)
			if a == "start-logging" {
				httpx.JSON(w, 200, map[string]any{"started": true, "was_debug": was})
				return
			}
			logs, _, _, e := s.logs(r.Context(), "", "", 500)
			if e != nil {
				httpx.Error(w, 500, "Cannot read diagnostic logs")
				return
			}
			var lines strings.Builder
			for i := len(logs) - 1; i >= 0; i-- {
				v := logs[i]
				fmt.Fprintf(&lines, "%v [%v] %v %v\n", v["timestamp"], v["level"], v["logger_name"], v["message"])
			}
			httpx.JSON(w, 200, map[string]string{"logs": lines.String()})
		})
	}
}

// The operator chooses a report receiver for this Bambastic distribution.
func (s *Service) submitReport(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "settings:read") {
		return
	}
	var v struct {
		Description string `json:"description"`
		Email       string `json:"email"`
		Screenshot  string `json:"screenshot_base64"`
		Support     *bool  `json:"include_support_info"`
		Debug       string `json:"debug_logs"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if json.NewDecoder(r.Body).Decode(&v) != nil || len(strings.TrimSpace(v.Description)) == 0 || len(v.Description) > 20000 || len(v.Email) > 320 || len(v.Debug) > 256<<10 {
		httpx.Error(w, 422, "Check the report description, email and attachment sizes")
		return
	}
	if v.Screenshot != "" {
		encoded := v.Screenshot
		if strings.HasPrefix(encoded, "data:") {
			_, encoded, _ = strings.Cut(encoded, ",")
		}
		blob, e := base64.StdEncoding.DecodeString(encoded)
		if e != nil || len(blob) > 5<<20 || !strings.HasPrefix(http.DetectContentType(blob), "image/") {
			httpx.Error(w, 422, "Invalid screenshot")
			return
		}
	}
	dst, e := url.Parse(os.Getenv("BAMBASTIC_REPORT_URL"))
	if e != nil || dst.Host == "" || dst.User != nil || (dst.Scheme != "https" && dst.Scheme != "http") {
		httpx.JSON(w, 200, map[string]any{"success": false, "message": "This installation has no report receiver configured. The administrator can export a support bundle."})
		return
	}
	data := map[string]any{"description": v.Description, "email": v.Email, "screenshot_base64": v.Screenshot}
	if v.Support == nil || *v.Support {
		data["support_info"] = map[string]any{"application": "Bambastic", "version": "0.1.0", "go_version": runtime.Version(), "platform": runtime.GOOS, "recent_logs": v.Debug}
	}
	b, e := json.Marshal(data)
	if e != nil {
		httpx.Error(w, 422, "Invalid report")
		return
	}
	req, e := http.NewRequestWithContext(r.Context(), "POST", dst.String(), bytes.NewReader(b))
	if e != nil {
		httpx.Error(w, 500, "Invalid report receiver")
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("BAMBASTIC_REPORT_TOKEN"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, e := lanhttp.Client.Do(req)
	if e != nil {
		httpx.Error(w, 502, "Report receiver is unavailable")
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		httpx.Error(w, 502, "Report receiver declined the submission")
		return
	}
	var out struct {
		URL    string `json:"issue_url"`
		Number int    `json:"issue_number"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out)
	answer := map[string]any{"success": true, "message": "Report submitted"}
	if u, e := url.Parse(out.URL); e == nil && u.Scheme == "https" && u.Host != "" && u.User == nil {
		answer["issue_url"] = u.String()
	}
	if out.Number > 0 {
		answer["issue_number"] = out.Number
	}
	httpx.JSON(w, 200, answer)
}
