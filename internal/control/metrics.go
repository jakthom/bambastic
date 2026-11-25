package control

import (
	"context"
	"crypto/subtle"
	"fmt"
	"math"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

func (s *Service) Metrics(router *http.ServeMux, snapshot func(context.Context, access.Principal) ([]map[string]any, error)) {
	router.HandleFunc("GET /api/v1/metrics", func(w http.ResponseWriter, r *http.Request) {
		enabled, e := access.Setting(r.Context(), s.DB, "prometheus_enabled", "false")
		if e != nil {
			httpx.Error(w, 503, "Configuration unavailable")
			return
		}
		if enabled != "true" {
			httpx.Error(w, 404, "Prometheus metrics are disabled")
			return
		}
		token, e := s.auth.ReadSecret(r.Context(), "prometheus_token")
		if e != nil {
			httpx.Error(w, 503, "Metric credential unavailable")
			return
		}
		if token != "" {
			sent, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || subtle.ConstantTimeCompare([]byte(sent), []byte(token)) != 1 {
				httpx.Error(w, 401, "Invalid metrics credential")
				return
			}
		}
		fleet, e := snapshot(r.Context(), access.Principal{Admin: true})
		if e != nil {
			httpx.Error(w, 503, "Telemetry unavailable")
			return
		}
		counts := map[string]int{}
		rows, e := s.DB.QueryContext(r.Context(), "SELECT status,count(*) FROM print_queue GROUP BY status")
		if e != nil {
			httpx.Error(w, 503, "Queue unavailable")
			return
		}
		for rows.Next() {
			var name string
			var n int
			if e = rows.Scan(&name, &n); e != nil {
				break
			}
			counts[name] = n
		}
		rows.Close()
		if e != nil {
			httpx.Error(w, 503, "Queue unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# TYPE bambuddy_build_info gauge\nbambuddy_build_info{version=%q,platform=%q,architecture=%q,go_version=%q} 1\n", "0.1.0", runtime.GOOS, runtime.GOARCH, runtime.Version())
		metrics := map[string][]string{}
		connected := 0
		for _, p := range fleet {
			labels := fmt.Sprintf("{printer_id=%q,printer_name=%q}", fmt.Sprint(p["printer_id"]), fmt.Sprint(p["name"]))
			emit := func(metric string, value any, scale float64) {
				n, e := strconv.ParseFloat(fmt.Sprint(value), 64)
				if e == nil && !math.IsNaN(n) && !math.IsInf(n, 0) {
					metrics[metric] = append(metrics[metric], labels+" "+strconv.FormatFloat(n*scale, 'f', -1, 64))
				}
			}
			if httpx.Bool(p["connected"]) {
				connected++
				emit("printer_connected", 1, 1)
			} else {
				emit("printer_connected", 0, 1)
				continue
			}
			emit("print_progress_percent", p["progress"], 1)
			emit("print_remaining_seconds", p["remaining_time"], 60)
			emit("print_layer_current", p["layer_num"], 1)
			emit("print_layer_total", p["total_layers"], 1)
			if t, ok := p["temperatures"].(map[string]any); ok {
				for k, v := range map[string]string{"bed": "bed_temp_celsius", "bed_target": "bed_target_celsius", "nozzle": "nozzle_temp_celsius", "nozzle_target": "nozzle_target_celsius", "chamber": "chamber_temp_celsius"} {
					emit(v, t[k], 1)
				}
			}
		}
		metrics["printers_total"] = []string{fmt.Sprintf(" %d", len(fleet))}
		metrics["printers_connected"] = []string{fmt.Sprintf(" %d", connected)}
		for state, n := range counts {
			metrics["queue_items"] = append(metrics["queue_items"], fmt.Sprintf("{status=%q} %d", state, n))
		}
		names := []string{}
		for m := range metrics {
			names = append(names, m)
		}
		sort.Strings(names)
		for _, m := range names {
			fmt.Fprintf(w, "# TYPE bambuddy_%s gauge\n", m)
			sort.Strings(metrics[m])
			for _, v := range metrics[m] {
				fmt.Fprintf(w, "bambuddy_%s%s\n", m, v)
			}
		}
	})
}
