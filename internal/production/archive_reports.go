package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

func (s *Service) archiveReports(rt register) {
	rt("GET", "/archives/last-per-printer", s.lastPerPrinter)
	rt("GET", "/archives/analysis/failures", s.failureReport(false))
	rt("GET", "/archives/stats/export", s.failureReport(true))
	rt("GET", "/archives/export", s.exportArchive)
	rt("GET", "/archives/no-3mf-warning", s.missingFilesWarning)
}
func archiveReader(p access.Principal) bool {
	return p.Has("archives:read_all") || p.Has("archives:read_own")
}
func (s *Service) lastPerPrinter(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !archiveReader(p) {
		return nil, fail(403, "Archive read permission required")
	}
	// Rank each printer's visible print history before loading full archive metadata.
	ctx := r.Context()
	owner := ""
	var args []any
	if !p.Has("archives:read_all") {
		owner = " AND a.created_by_id=?"
		args = append(args, p.ID)
	}
	candidates, e := records(ctx, s.db, `SELECT * FROM (SELECT a.id AS aid,l.*,row_number() OVER (PARTITION BY l.printer_id ORDER BY coalesce(l.started_at,l.created_at) DESC,l.id DESC) AS sequence FROM print_log_entries l JOIN print_archives a ON a.id=l.archive_id WHERE a.deleted_at IS NULL AND l.printer_id IS NOT NULL`+owner+`) WHERE sequence=1`, args...)
	if e != nil {
		return nil, e
	}
	latest := map[int64]row{}
	for _, run := range candidates {
		pid := num(run["printer_id"])
		if !p.AllowsPrinter(pid) {
			continue
		}
		v, e := s.getAsset(ctx, s.db, p, "archives", num(run["aid"]), "read")
		if e != nil {
			continue
		}
		decodeAsset(v)
		v["run_id"] = run["id"]
		for _, k := range []string{"printer_id", "printer_name", "status", "started_at", "completed_at", "plate_id", "failure_reason", "user_verdict", "user_verdict_source", "user_verdict_at", "timelapse_path", "photos", "confirm_requested", "cost", "filament_used_grams", "energy_kwh", "energy_cost", "print_name"} {
			v[k] = run[k]
		}
		latest[pid] = v
	}
	fallback, e := records(ctx, s.db, `SELECT * FROM (SELECT a.*,row_number() OVER (PARTITION BY a.printer_id ORDER BY coalesce(a.started_at,a.created_at) DESC,a.id DESC) AS sequence FROM print_archives a WHERE a.deleted_at IS NULL AND a.printer_id IS NOT NULL AND NOT EXISTS(SELECT 1 FROM print_log_entries l WHERE l.archive_id=a.id)`+owner+`) WHERE sequence=1`, args...)
	if e != nil {
		return nil, e
	}
	for _, v := range fallback {
		pid := num(v["printer_id"])
		if p.AllowsPrinter(pid) && latest[pid] == nil {
			s.cleanCard(v)
			latest[pid] = v
		}
	}
	out := make([]row, 0, len(latest))
	for _, v := range latest {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return num(out[i]["printer_id"]) < num(out[j]["printer_id"]) })
	return out, nil
}
func (s *Service) cleanCard(v row) { delete(v, "sequence"); decodeAsset(v) }
func (s *Service) missingFilesWarning(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !archiveReader(p) {
		return nil, fail(403, "Archive read permission required")
	}
	records, e := records(r.Context(), s.db, "SELECT * FROM print_archives WHERE deleted_at IS NULL AND created_at>=datetime('now','-30 days') AND extra_data IS NOT NULL")
	if e != nil {
		return nil, e
	}
	visible := false
	var reason any
	for _, v := range records {
		if s.accessible(r.Context(), s.db, p, "archives", v, "read") {
			var extra row
			_ = json.Unmarshal([]byte(val(v["extra_data"])), &extra)
			if flag(extra["no_3mf_available"]) {
				visible = true
				if why := extra["no_3mf_reason"]; why != nil {
					reason = why
					break
				}
			}
		}
	}
	return row{"has_fallback": visible, "reason": reason}, nil
}

type reportWindow struct {
	from, until time.Time
	zone        *time.Location
	days        int
}

func reportingWindow(r *http.Request, defaultDays int) (reportWindow, error) {
	q := r.URL.Query()
	zone := time.Local
	var e error
	if z := q.Get("tz"); z != "" {
		zone, e = time.LoadLocation(z)
		if e != nil {
			return reportWindow{}, fail(400, "Invalid time zone")
		}
	}
	v := reportWindow{zone: zone, days: defaultDays}
	if raw := q.Get("days"); raw != "" {
		n, e := strconv.Atoi(raw)
		if e != nil || n < 1 || n > 36500 {
			return v, fail(400, "Invalid period")
		}
		v.days = n
	}
	for _, field := range []string{"date_from", "date_to"} {
		raw := q.Get(field)
		if raw == "" {
			continue
		}
		t, e := time.ParseInLocation("2006-01-02", raw, zone)
		if e != nil {
			t, e = time.Parse(time.RFC3339, raw)
		} else if field == "date_to" {
			t = t.AddDate(0, 0, 1)
		}
		if e != nil {
			return v, fail(400, "Invalid date")
		}
		if field == "date_from" {
			v.from = t
		} else {
			v.until = t
		}
	}
	if v.from.IsZero() && v.until.IsZero() && v.days > 0 {
		v.from = time.Now().AddDate(0, 0, -v.days)
	}
	if !v.until.IsZero() && !v.from.IsZero() {
		if !v.from.Before(v.until) {
			return v, fail(400, "Invalid date range")
		}
		v.days = int(math.Ceil(v.until.Sub(v.from).Hours() / 24))
	}
	return v, nil
}
func reportDate(v any) time.Time {
	if t, ok := v.(time.Time); ok {
		return t
	}
	for _, f := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05.999999999", "2006-01-02T15:04:05.999999999"} {
		if t, e := time.Parse(f, val(v)); e == nil {
			return t
		}
	}
	return time.Time{}
}
func (v reportWindow) accepts(t time.Time) bool {
	return (v.from.IsZero() || !t.Before(v.from)) && (v.until.IsZero() || t.Before(v.until))
}
func ratio(a, b int) float64 {
	if b == 0 {
		return 0
	}
	return math.Round(float64(a)*1000/float64(b)) / 10
}
func label(v any) string {
	if s := val(v); s != "" {
		return s
	}
	return "Unknown"
}
func (s *Service) failureReport(export bool) endpoint {
	return func(w http.ResponseWriter, r *http.Request) (any, error) {
		ctx := r.Context()
		p := access.From(ctx)
		if export && !p.Has("stats:read") || !export && !archiveReader(p) {
			return nil, fail(403, "Report permission required")
		}
		q := r.URL.Query()
		user := q.Get("created_by_id")
		if user != "" && !p.Has("stats:filter_by_user") {
			return nil, fail(403, "User filtering permission required")
		}
		days := 30
		if export {
			days = 30
		}
		if !export && q.Get("date_from") != "" {
			days = 0
		}
		window, e := reportingWindow(r, days)
		if e != nil {
			return nil, e
		}
		runs, e := records(ctx, s.db, "SELECT l.*,a.project_id,p.name current_printer FROM print_log_entries l LEFT JOIN print_archives a ON a.id=l.archive_id LEFT JOIN printers p ON p.id=l.printer_id ORDER BY l.created_at DESC,l.id DESC")
		if e != nil {
			return nil, e
		}
		byReason, byMaterial, byPrinter, rejects := map[string]int{}, map[string]int{}, map[string]int{}, map[string]int{}
		byHour := map[int]int{}
		for h := 0; h < 24; h++ {
			byHour[h] = 0
		}
		recent := []row{}
		total, success, failed, rejected := 0, 0, 0, 0
		weeks := max(1, min(5215, window.days/7))
		now := time.Now().UTC()
		trend := make([]row, weeks)
		counts := make([][3]int, weeks)
		for i := range trend {
			trend[i] = row{"week_start": now.AddDate(0, 0, -7*(weeks-i)).Format("2006-01-02")}
		}
		for _, v := range runs {
			pid, uid := num(v["printer_id"]), num(v["created_by_id"])
			if !scope(p, pid) || q.Get("printer_id") != "" && pid != num(q.Get("printer_id")) || q.Get("project_id") != "" && num(v["project_id"]) != num(q.Get("project_id")) {
				continue
			}
			if !export && !p.Has("archives:read_all") && (uid == 0 || uid != p.ID) {
				continue
			}
			if user != "" && (user == "-1" && uid != 0 || user != "-1" && uid != num(user)) {
				continue
			}
			dt := reportDate(v["created_at"])
			status := val(v["status"])
			bad := status == "failed" || status == "aborted"
			good := status == "completed"
			age := now.Sub(dt)
			if age >= 0 && age < time.Duration(weeks*7)*24*time.Hour {
				idx := weeks - 1 - int(age/(7*24*time.Hour))
				counts[idx][0]++
				if bad {
					counts[idx][1]++
				}
				if good {
					counts[idx][2]++
				}
			}
			if !window.accepts(dt) {
				continue
			}
			total++
			if good {
				success++
				if val(v["user_verdict"]) == "reject" {
					rejected++
					rejects[label(v["failure_reason"])]++
				}
			}
			if bad {
				failed++
				byReason[label(v["failure_reason"])]++
				byMaterial[label(v["filament_type"])]++
				name := v["current_printer"]
				if name == nil {
					name = v["printer_name"]
				}
				byPrinter[label(name)]++
				byHour[dt.In(window.zone).Hour()]++
				if len(recent) < 10 {
					recent = append(recent, row{"id": v["archive_id"], "run_id": v["id"], "print_name": v["print_name"], "failure_reason": v["failure_reason"], "filament_type": v["filament_type"], "printer_id": v["printer_id"], "created_at": v["created_at"]})
				}
			}
		}
		for i, c := range counts {
			trend[i]["total_prints"] = c[0]
			trend[i]["failed_prints"] = c[1]
			trend[i]["failure_rate"] = ratio(c[1], c[1]+c[2])
		}
		result := row{"period_days": window.days, "total_prints": total, "failed_prints": failed, "failure_rate": ratio(failed, success+failed), "rejected_prints": rejected, "yield_rate": ratio(success-rejected, success+failed), "rejects_by_reason": rejects, "failures_by_reason": byReason, "failures_by_filament": byMaterial, "failures_by_printer": byPrinter, "failures_by_hour": byHour, "recent_failures": recent, "trend": trend}
		if !export {
			return result, nil
		}
		data := [][]any{{"Metric", "Value"}, {"Period (days)", window.days}, {"Total Prints", total}, {"Failed Prints", failed}, {"Failure Rate (%)", result["failure_rate"]}}
		for _, group := range []struct {
			name   string
			values map[string]int
		}{{"Failures by Reason", byReason}, {"Failures by Filament", byMaterial}, {"Failures by Printer", byPrinter}} {
			data = append(data, []any{""}, []any{group.name, ""})
			keys := []string{}
			for k := range group.values {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				data = append(data, []any{k, group.values[k]})
			}
		}
		data = append(data, []any{""}, []any{"Weekly Trend"}, []any{"Week", "Total", "Failed", "Rate (%)"})
		for _, v := range trend {
			data = append(data, []any{v["week_start"], v["total_prints"], v["failed_prints"], v["failure_rate"]})
		}
		return nil, writeReport(w, q.Get("format"), "stats", data)
	}
}

var defaultExport = strings.Fields("id run_id print_name filename status quantity printer_id printer_name plate_id project_name filament_type filament_used_grams print_time_seconds layer_height nozzle_diameter bed_temperature nozzle_temperature total_layers cost designer tags notes failure_reason user_verdict duration_seconds started_at completed_at created_at")
var exportLabels = map[string]string{"id": "ID", "run_id": "Print ID", "print_name": "Print Name", "filename": "Filename", "status": "Status", "quantity": "Items Printed", "printer_id": "Printer ID", "printer_name": "Printer", "plate_id": "Plate", "project_name": "Project", "filament_type": "Filament Type", "filament_used_grams": "Filament (g)", "print_time_seconds": "Print Time (s)", "layer_height": "Layer Height (mm)", "nozzle_diameter": "Nozzle (mm)", "bed_temperature": "Bed Temp (°C)", "nozzle_temperature": "Nozzle Temp (°C)", "total_layers": "Total Layers", "cost": "Cost", "energy_kwh": "Energy (kWh)", "energy_cost": "Energy Cost", "wear_cost": "Wear Cost", "designer": "Designer", "tags": "Tags", "notes": "Notes", "failure_reason": "Failure Reason", "user_verdict": "Outcome", "duration_seconds": "Duration (s)", "started_at": "Started At", "completed_at": "Completed At", "created_at": "Created At"}

func (s *Service) exportArchive(w http.ResponseWriter, r *http.Request) (any, error) {
	p := access.From(r.Context())
	if !archiveReader(p) {
		return nil, fail(403, "Archive read permission required")
	}
	q := r.URL.Query()
	window, e := reportingWindow(r, 0)
	if e != nil {
		return nil, e
	}
	fields := defaultExport
	if raw := q.Get("fields"); raw != "" {
		fields = strings.Split(raw, ",")
		for i, k := range fields {
			fields[i] = strings.TrimSpace(k)
			if _, ok := exportLabels[fields[i]]; !ok {
				return nil, fail(400, "Unknown export field")
			}
		}
	}
	heads := []any{}
	for _, k := range fields {
		heads = append(heads, exportLabels[k])
	}
	data := [][]any{heads}
	archives, e := records(r.Context(), s.db, "SELECT a.*,pr.name printer_name,pj.name project_name FROM print_archives a LEFT JOIN printers pr ON pr.id=a.printer_id LEFT JOIN projects pj ON pj.id=a.project_id WHERE a.deleted_at IS NULL ORDER BY a.created_at DESC,a.id DESC")
	if e != nil {
		return nil, e
	}
	for _, a := range archives {
		if !owned(p, "archives", "read", num(a["created_by_id"])) || q.Get("project_id") != "" && num(q.Get("project_id")) != num(a["project_id"]) {
			continue
		}
		runs, e := records(r.Context(), s.db, "SELECT * FROM print_log_entries WHERE archive_id=? ORDER BY created_at DESC,id DESC", a["id"])
		if e != nil {
			return nil, e
		}
		if len(runs) == 0 {
			runs = []row{nil}
		}
		for _, run := range runs {
			v := row{}
			for k, x := range a {
				v[k] = x
			}
			if run != nil {
				for _, k := range strings.Fields("printer_id printer_name plate_id status user_verdict duration_seconds energy_kwh energy_cost wear_cost filament_type filament_used_grams cost quantity started_at completed_at created_at failure_reason notes") {
					v[k] = run[k]
				}
				v["run_id"] = run["id"]
				if run["print_name"] != nil {
					v["print_name"] = run["print_name"]
				}
			}
			pid := num(v["printer_id"])
			if !scope(p, pid) || q.Get("printer_id") != "" && num(q.Get("printer_id")) != pid || q.Get("status") != "" && q.Get("status") != val(v["status"]) || !window.accepts(reportDate(v["created_at"])) {
				continue
			}
			line := []any{}
			for _, k := range fields {
				line = append(line, v[k])
			}
			data = append(data, line)
		}
	}
	return nil, writeReport(w, q.Get("format"), "archives", data)
}
func cellText(v any) string {
	if t, ok := v.(time.Time); ok {
		return t.Format(time.RFC3339)
	}
	return val(v)
}
func writeReport(w http.ResponseWriter, format, name string, rows [][]any) error {
	if format == "" {
		format = "csv"
	}
	var b bytes.Buffer
	switch format {
	case "csv":
		writer := csv.NewWriter(&b)
		for _, line := range rows {
			str := make([]string, len(line))
			for i, v := range line {
				str[i] = cellText(v)
				if _, ok := v.(string); ok {
					trim := strings.TrimSpace(str[i])
					if trim != "" && strings.ContainsAny(string([]rune(trim)[0]), "=+-@＝＋－＠") {
						str[i] = "'" + str[i]
					}
				}
			}
			if e := writer.Write(str); e != nil {
				return e
			}
		}
		writer.Flush()
		if e := writer.Error(); e != nil {
			return e
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	case "xlsx":
		if e := xlsx(&b, rows); e != nil {
			return e
		}
		w.Header().Set("Content-Type", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet")
	default:
		return fail(400, "Format must be csv or xlsx")
	}
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": fmt.Sprintf("%s_export_%s.%s", name, time.Now().Format("20060102_150405"), format)}))
	_, e := w.Write(b.Bytes())
	return e
}
func xlsx(b *bytes.Buffer, rows [][]any) error {
	z := zip.NewWriter(b)
	files := map[string]string{
		"[Content_Types].xml":        `<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/><Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/></Types>`,
		"_rels/.rels":                `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/></Relationships>`,
		"xl/workbook.xml":            `<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><sheets><sheet name="Report" sheetId="1" r:id="rId1"/></sheets></workbook>`,
		"xl/_rels/workbook.xml.rels": `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/></Relationships>`}
	for k, v := range files {
		f, e := z.Create(k)
		if e != nil {
			return e
		}
		if _, e = f.Write([]byte(v)); e != nil {
			return e
		}
	}
	f, e := z.Create("xl/worksheets/sheet1.xml")
	if e != nil {
		return e
	}
	fmt.Fprint(f, `<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>`)
	for i, line := range rows {
		fmt.Fprintf(f, `<row r="%d">`, i+1)
		for j, v := range line {
			letters := ""
			for n := j + 1; n > 0; n = (n - 1) / 26 {
				letters = string(rune('A'+(n-1)%26)) + letters
			}
			id := fmt.Sprintf("%s%d", letters, i+1)
			switch v.(type) {
			case float64, int, int64, float32:
				fmt.Fprintf(f, `<c r="%s"><v>%s</v></c>`, id, cellText(v))
			default:
				fmt.Fprintf(f, `<c r="%s" t="inlineStr"><is><t xml:space="preserve">`, id)
				if e := xml.EscapeText(f, []byte(cellText(v))); e != nil {
					return e
				}
				fmt.Fprint(f, "</t></is></c>")
			}
		}
		fmt.Fprint(f, "</row>")
	}
	fmt.Fprint(f, "</sheetData></worksheet>")
	return z.Close()
}
