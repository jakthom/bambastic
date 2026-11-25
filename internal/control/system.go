package control

import (
	"context"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

func byteSize(n uint64) string {
	div := float64(n)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if div < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", div, unit)
		}
		div /= 1024
	}
	return ""
}
func (s *Service) storage(ctx context.Context) (obj, error) {
	sizes := map[string]uint64{}
	var total uint64
	var soft int
	err := filepath.WalkDir(s.data, func(path string, d fs.DirEntry, e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e != nil {
			soft++
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".restores" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, e := filepath.Rel(s.data, path)
		if e != nil {
			return e
		}
		name, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
		cat := "other"
		if strings.HasPrefix(name, "bambastic.db") {
			cat = "database"
		} else {
			switch name {
			case "archives", "library", "timelapses", "backups", "slices", "cache":
				cat = name
			}
		}
		stat, e := d.Info()
		if e != nil {
			soft++
			return nil
		}
		n := uint64(stat.Size())
		sizes[cat] += n
		total += n
		return nil
	})
	if err != nil {
		return nil, err
	}
	cats := []obj{}
	for key, n := range sizes {
		pct := float64(0)
		if total > 0 {
			pct = 100 * float64(n) / float64(total)
		}
		cats = append(cats, obj{"key": key, "label": key, "bytes": n, "formatted": byteSize(n), "percent_of_total": pct})
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i]["bytes"].(uint64) > cats[j]["bytes"].(uint64) })
	return obj{"total_bytes": total, "total_formatted": byteSize(total), "categories": cats, "other_breakdown": []obj{}, "scan_errors": soft, "generated_at": time.Now().UTC(), "roots": []string{s.data}, "cache": obj{"hit": false, "max_age_seconds": 0, "age_seconds": 0}}, nil
}
func (s *Service) System(api *http.ServeMux, status func(context.Context, access.Principal) ([]map[string]any, error), pg bool) {
	api.HandleFunc("GET /api/v1/system/storage-usage", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		v, e := s.storage(r.Context())
		if e != nil {
			httpx.Error(w, 500, "Storage scan failed")
			return
		}
		httpx.JSON(w, 200, v)
	})
	api.HandleFunc("GET /api/v1/system/info", func(w http.ResponseWriter, r *http.Request) {
		p := access.From(r.Context())
		if !access.Require(w, r, "settings:read") {
			return
		}
		ctx := r.Context()
		base, e := filepath.Abs(s.data)
		if e != nil {
			httpx.Error(w, 500, "Storage unavailable")
			return
		}
		db := obj{"engine": "SQLite"}
		versionQuery := "SELECT sqlite_version()"
		if pg {
			db["engine"] = "PostgreSQL"
			versionQuery = "SELECT current_setting('server_version')"
		}
		for k, q := range map[string]string{"version": versionQuery, "archives": "SELECT count(*) FROM print_archives", "archives_completed": "SELECT count(*) FROM print_archives WHERE status='completed'", "archives_failed": "SELECT count(*) FROM print_archives WHERE status='failed'", "archives_printing": "SELECT count(*) FROM print_archives WHERE status='printing'", "filaments": "SELECT count(*) FROM spool", "projects": "SELECT count(*) FROM projects", "smart_plugs": "SELECT count(*) FROM smart_plugs", "total_print_time_seconds": "SELECT coalesce(sum(duration_seconds),0) FROM print_log_entries", "total_filament_grams": "SELECT coalesce(sum(filament_used_grams),0) FROM print_log_entries"} {
			var value any
			if e = s.DB.QueryRowContext(ctx, q).Scan(&value); e != nil {
				httpx.Error(w, 500, "Database statistics unavailable")
				return
			}
			db[k] = value
		}
		secs, _ := strconv.ParseInt(fmt.Sprint(db["total_print_time_seconds"]), 10, 64)
		db["total_print_time_formatted"] = (time.Duration(secs) * time.Second).String()
		g, _ := strconv.ParseFloat(fmt.Sprint(db["total_filament_grams"]), 64)
		db["total_filament_kg"] = g / 1000
		devices, e := status(ctx, p)
		if e != nil {
			httpx.Error(w, 500, "Printer status unavailable")
			return
		}
		connected := []obj{}
		for _, v := range devices {
			if httpx.Bool(v["connected"]) {
				connected = append(connected, obj{"id": v["id"], "name": v["name"], "state": v["state"], "model": v["model"]})
			}
		}
		db["printers"] = len(devices)
		storage := obj{}
		v, e := s.storage(ctx)
		if e == nil {
			for _, c := range v["categories"].([]obj) {
				if c["key"] == "archives" || c["key"] == "database" {
					key := "archive"
					if c["key"] == "database" {
						key = "database"
					}
					storage[key+"_size_bytes"] = c["bytes"]
					storage[key+"_size_formatted"] = c["formatted"]
				}
			}
		}
		if d, e := disk.UsageWithContext(ctx, base); e == nil {
			for k, n := range map[string]uint64{"total": d.Total, "free": d.Free, "used": d.Used} {
				storage["disk_"+k+"_bytes"] = n
				storage["disk_"+k+"_formatted"] = byteSize(n)
			}
			storage["disk_percent_used"] = d.UsedPercent
		}
		memory := obj{}
		if m, e := mem.VirtualMemoryWithContext(ctx); e == nil {
			for k, n := range map[string]uint64{"total": m.Total, "used": m.Used, "available": m.Available} {
				memory[k+"_bytes"] = n
				memory[k+"_formatted"] = byteSize(n)
			}
			memory["percent_used"] = m.UsedPercent
		}
		cpuStat := obj{"count_logical": runtime.NumCPU()}
		if n, e := cpu.CountsWithContext(ctx, false); e == nil {
			cpuStat["count"] = n
		}
		if pct, e := cpu.PercentWithContext(ctx, 0, false); e == nil && len(pct) > 0 {
			cpuStat["percent"] = pct[0]
		}
		system := obj{"platform": runtime.GOOS, "architecture": runtime.GOARCH, "go_version": runtime.Version()}
		if h, e := host.InfoWithContext(ctx); e == nil {
			system["hostname"] = h.Hostname
			system["platform_release"] = h.KernelVersion
			system["platform_version"] = h.PlatformVersion
			system["boot_time"] = time.Unix(int64(h.BootTime), 0).UTC()
			system["uptime_seconds"] = h.Uptime
			system["uptime_formatted"] = (time.Duration(h.Uptime) * time.Second).String()
		}
		httpx.JSON(w, 200, obj{"app": obj{"version": "0.1.0", "base_dir": base, "archive_dir": filepath.Join(base, "archives")}, "database": db, "system": system, "storage": storage, "cpu": cpuStat, "memory": memory, "printers": obj{"total": len(devices), "connected": len(connected), "connected_list": connected}})
	})
	api.HandleFunc("GET /api/v1/settings/network-interfaces", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		ifs, e := net.Interfaces()
		if e != nil {
			httpx.Error(w, 500, "Cannot enumerate network interfaces")
			return
		}
		out := []obj{}
		for _, i := range ifs {
			addresses, e := i.Addrs()
			if e != nil {
				continue
			}
			for _, a := range addresses {
				ip, n, e := net.ParseCIDR(a.String())
				if e != nil || ip.To4() == nil {
					continue
				}
				out = append(out, obj{"name": i.Name, "ip": ip.String(), "netmask": net.IP(n.Mask).String(), "subnet": n.String(), "label": i.Name})
			}
		}
		httpx.JSON(w, 200, obj{"interfaces": out})
	})
	api.HandleFunc("POST /api/v1/settings/electricity-price", func(w http.ResponseWriter, r *http.Request) {
		p := access.From(r.Context())
		if !p.Has("settings:update") && !p.Has("settings:update_energy_cost") {
			httpx.Error(w, 403, "Energy cost update permission required")
			return
		}
		var v struct {
			Price *float64 `json:"energy_cost_per_kwh"`
		}
		if !httpx.Decode(w, r, &v) {
			return
		}
		if v.Price == nil || *v.Price < 0 {
			httpx.Error(w, 422, "Invalid electricity price")
			return
		}
		_, e := s.DB.Exec("INSERT INTO settings(key,value) VALUES('energy_cost_per_kwh',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.FormatFloat(*v.Price, 'g', -1, 64))
		if e != nil {
			httpx.Error(w, 500, "Could not save tariff")
			return
		}
		if p.KeyID != 0 {
			httpx.JSON(w, 200, obj{"energy_cost_per_kwh": *v.Price})
			return
		}
		v2, e := s.settings(r.Context())
		if e != nil {
			httpx.Error(w, 500, "Could not read settings")
			return
		}
		httpx.JSON(w, 200, v2)
	})
	for _, m := range []string{"GET", "PUT"} {
		api.HandleFunc(m+" /api/v1/settings/spoolman", s.spoolman)
	}
}
func (s *Service) spoolman(w http.ResponseWriter, r *http.Request) {
	perm := "settings:read"
	if r.Method == "PUT" {
		perm = "settings:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	fields := map[string]string{"spoolman_enabled": "false", "spoolman_url": "", "spoolman_sync_mode": "auto", "spoolman_disable_weight_sync": "false", "spoolman_report_partial_usage": "true", "auto_add_unknown_rfid": "true"}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Database unavailable")
		return
	}
	defer tx.Rollback()
	if r.Method == "PUT" {
		var v obj
		if !httpx.Decode(w, r, &v) {
			return
		}
		for k, x := range v {
			if _, ok := fields[k]; !ok {
				continue
			}
			a := fmt.Sprint(x)
			switch k {
			case "spoolman_url":
				if a != "" {
					u, e := url.Parse(a)
					if e != nil || u.Host == "" || u.User != nil || u.Scheme != "http" && u.Scheme != "https" {
						httpx.Error(w, 422, "Invalid Spoolman URL")
						return
					}
				}
			case "spoolman_sync_mode":
				if a != "auto" && a != "manual" {
					httpx.Error(w, 422, "Invalid sync mode")
					return
				}
			default:
				b, e := strconv.ParseBool(a)
				if e != nil {
					httpx.Error(w, 422, "Invalid "+k)
					return
				}
				a = strconv.FormatBool(b)
			}
			if e := access.Put(tx, k, a); e != nil {
				httpx.Error(w, 500, "Cannot update Spoolman settings")
				return
			}
		}
	}
	out := obj{}
	for k, d := range fields {
		out[k], e = access.Setting(r.Context(), tx, k, d)
		if e != nil {
			httpx.Error(w, 500, "Settings unavailable")
			return
		}
	}
	if e = tx.Commit(); e != nil {
		httpx.Error(w, 500, "Settings unavailable")
		return
	}
	httpx.JSON(w, 200, out)
}
