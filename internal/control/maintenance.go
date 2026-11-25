package control

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"database/sql"
	_ "embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed maintenance.json
var defaultMaintenance []byte

func (s *Service) registerMaintenance(api *http.ServeMux) {
	s.maintenanceLogRoutes(api)
	reg := func(method, path, perm string, fn func(*http.Request) (any, error)) {
		api.HandleFunc(method+" /api/v1/maintenance/"+path, func(w http.ResponseWriter, r *http.Request) {
			if !access.Require(w, r, "maintenance:"+perm) {
				return
			}
			v, e := fn(r)
			if e != nil {
				switch {
				case errors.Is(e, sql.ErrNoRows):
					httpx.Error(w, 404, "Maintenance record not found")
				case errors.Is(e, errInput):
					httpx.Error(w, 422, e.Error())
				default:
					httpx.Error(w, 500, "Maintenance operation failed")
				}
				return
			}
			httpx.JSON(w, 200, v)
		})
	}
	reg("GET", "overview", "read", s.maintenanceOverview)
	reg("GET", "summary", "read", s.maintenanceOverview)
	reg("GET", "printers/{printer}", "read", s.maintenanceOverview)
	reg("GET", "types", "read", s.maintenanceTypes)
	reg("POST", "types", "create", s.maintenanceTypes)
	reg("PATCH", "types/{id}", "update", s.maintenanceTypes)
	reg("DELETE", "types/{id}", "delete", s.maintenanceTypes)
	reg("POST", "types/restore-defaults", "update", s.maintenanceTypes)
	reg("PATCH", "items/{item}", "update", s.maintenanceItem)
	reg("DELETE", "items/{item}", "delete", s.maintenanceItem)
	reg("POST", "items/{item}/perform", "update", s.maintenanceItem)
	reg("GET", "items/{item}/history", "read", s.maintenanceItem)
	reg("GET", "printers/{printer}/log", "read", s.maintenanceLog)
	reg("GET", "log", "read", s.maintenanceLog)
	reg("POST", "printers/{printer}/assign/{type}", "create", s.assignMaintenance)
	reg("PATCH", "printers/{printer}/hours", "update", s.printerHours)
}

var errInput = errors.New("invalid maintenance request")

func query(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, stmt string, args ...any) ([]obj, error) {
	rows, e := q.Query(stmt, args...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(rows)
}
func seedTypes(tx *sql.Tx) error {
	var items []struct {
		Name, Description, Icon string
		Hours                   float64 `json:"default_interval_hours"`
	}
	if e := json.Unmarshal(defaultMaintenance, &items); e != nil {
		return e
	}
	for _, v := range items {
		if _, e := tx.Exec(`INSERT INTO maintenance_types(name,description,icon,default_interval_hours,interval_type,is_system,is_deleted) SELECT ?,?,?,?,'hours',1,0 WHERE NOT EXISTS(SELECT 1 FROM maintenance_types WHERE name=?)`, v.Name, v.Description, v.Icon, v.Hours, v.Name); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) maintenanceTypes(r *http.Request) (any, error) {
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = seedTypes(tx); e != nil {
		return nil, e
	}
	id := r.PathValue("id")
	switch {
	case r.Method == "DELETE":
		res, e := tx.Exec("UPDATE maintenance_types SET is_deleted=1 WHERE id=?", id)
		if e != nil {
			return nil, e
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil, sql.ErrNoRows
		}
	case strings.HasSuffix(r.URL.Path, "/restore-defaults"):
		res, e := tx.Exec("UPDATE maintenance_types SET is_deleted=0 WHERE is_system AND is_deleted")
		if e != nil {
			return nil, e
		}
		n, _ := res.RowsAffected()
		return obj{"restored": n}, tx.Commit()
	case r.Method != "GET":
		var v struct {
			Name, Description, Icon *string
			Hours                   *float64 `json:"default_interval_hours"`
			Kind                    *string  `json:"interval_type"`
			Wiki                    *string  `json:"wiki_url"`
		}
		if e = json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<18)).Decode(&v); e != nil || v.Hours != nil && *v.Hours < 1 || v.Kind != nil && *v.Kind != "hours" && *v.Kind != "days" {
			return nil, errInput
		}
		if r.Method == "POST" {
			if v.Name == nil || strings.TrimSpace(*v.Name) == "" {
				return nil, errInput
			}
			res, e := tx.Exec("INSERT INTO maintenance_types(name,description,icon,default_interval_hours,interval_type,wiki_url,is_system,is_deleted) VALUES(?,?,?,COALESCE(?,100),COALESCE(?,'hours'),?,0,0)", v.Name, v.Description, v.Icon, v.Hours, v.Kind, v.Wiki)
			if e != nil {
				return nil, e
			}
			i, _ := res.LastInsertId()
			id = strconv.FormatInt(i, 10)
		} else {
			_, e = tx.Exec("UPDATE maintenance_types SET name=COALESCE(?,name),description=COALESCE(?,description),icon=COALESCE(?,icon),default_interval_hours=COALESCE(?,default_interval_hours),interval_type=COALESCE(?,interval_type),wiki_url=COALESCE(?,wiki_url) WHERE id=?", v.Name, v.Description, v.Icon, v.Hours, v.Kind, v.Wiki, id)
			if e != nil {
				return nil, e
			}
		}
	}
	if r.Method == "DELETE" {
		return obj{"status": "deleted"}, tx.Commit()
	}
	st := "SELECT * FROM maintenance_types WHERE NOT is_deleted"
	args := []any{}
	if id != "" {
		st += " AND id=?"
		args = append(args, id)
	}
	list, e := query(tx, st+" ORDER BY name", args...)
	if e != nil {
		return nil, e
	}
	if id != "" {
		if len(list) == 0 {
			return nil, sql.ErrNoRows
		}
		return list[0], tx.Commit()
	}
	return list, tx.Commit()
}

const itemStatus = `SELECT i.id,p.id AS printer_id,p.name AS printer_name,p.model AS printer_model,t.id AS maintenance_type_id,t.name AS maintenance_type_name,t.icon AS maintenance_type_icon,t.wiki_url AS maintenance_type_wiki_url,i.enabled,COALESCE(i.custom_interval_hours,t.default_interval_hours) AS interval_hours,COALESCE(i.custom_interval_type,t.interval_type) AS interval_type,COALESCE(p.runtime_seconds,0)/3600.0+COALESCE(p.print_hours_offset,0.0) AS current_hours,i.last_performed_at,i.last_performed_hours FROM printer_maintenance i JOIN printers p ON p.id=i.printer_id JOIN maintenance_types t ON t.id=i.maintenance_type_id WHERE NOT t.is_deleted`

func numeric(v any) float64 { f, _ := strconv.ParseFloat(fmt.Sprint(v), 64); return f }
func evaluate(v obj) {
	interval := numeric(v["interval_hours"])
	hours := numeric(v["current_hours"]) - numeric(v["last_performed_hours"])
	left := interval - hours
	v["hours_since_maintenance"] = hours
	v["hours_until_due"] = left
	v["days_since_maintenance"] = nil
	v["days_until_due"] = nil
	days := interval + 1
	if t, ok := v["last_performed_at"].(time.Time); ok {
		days = time.Since(t).Hours() / 24
		v["days_since_maintenance"] = days
	}
	if v["interval_type"] == "days" {
		v["days_since_maintenance"] = days
		left = interval - days
		v["days_until_due"] = left
		v["hours_until_due"] = 0
	}
	v["is_due"] = left <= 0
	v["is_warning"] = left > 0 && left <= interval*.1
}
func (s *Service) maintenanceOverview(r *http.Request) (any, error) {
	p := access.From(r.Context())
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	if e = seedTypes(tx); e != nil {
		return nil, e
	}
	list, e := query(tx, "SELECT id AS printer_id,name AS printer_name,model AS printer_model,COALESCE(runtime_seconds,0)/3600.0+COALESCE(print_hours_offset,0) AS total_print_hours FROM printers WHERE is_active ORDER BY name")
	if e != nil {
		return nil, e
	}
	out := []obj{}
	issues := []obj{}
	due, warn := 0, 0
	for _, v := range list {
		id := v["printer_id"].(int64)
		if !p.AllowsPrinter(id) || r.PathValue("printer") != "" && r.PathValue("printer") != strconv.FormatInt(id, 10) {
			continue
		}
		_, e = tx.Exec("INSERT OR IGNORE INTO printer_maintenance(printer_id,maintenance_type_id,enabled,last_performed_hours) SELECT ?,id,1,0 FROM maintenance_types WHERE is_system AND NOT is_deleted", id)
		if e != nil {
			return nil, e
		}
		items, e := query(tx, itemStatus+" AND p.id=? ORDER BY t.name", id)
		if e != nil {
			return nil, e
		}
		d, w := 0, 0
		for _, i := range items {
			evaluate(i)
			if httpx.Bool(i["enabled"]) {
				if i["is_due"] == true {
					d++
				} else if i["is_warning"] == true {
					w++
				}
			}
		}
		v["maintenance_items"] = items
		v["due_count"] = d
		v["warning_count"] = w
		out = append(out, v)
		if d+w > 0 {
			issues = append(issues, obj{"printer_id": id, "printer_name": v["printer_name"], "due_count": d, "warning_count": w})
		}
		due += d
		warn += w
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	if r.PathValue("printer") != "" {
		if len(out) == 0 {
			return nil, sql.ErrNoRows
		}
		return out[0], nil
	}
	if strings.HasSuffix(r.URL.Path, "/summary") {
		return obj{"total_due": due, "total_warning": warn, "printers_with_issues": issues}, nil
	}
	return out, nil
}
func (s *Service) maintenanceItem(r *http.Request) (any, error) {
	p := access.From(r.Context())
	id := r.PathValue("item")
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rows, e := query(tx, itemStatus+" AND i.id=?", id)
	if e != nil {
		return nil, e
	}
	if len(rows) != 1 || !p.AllowsPrinter(rows[0]["printer_id"].(int64)) {
		return nil, sql.ErrNoRows
	}
	v := rows[0]
	if r.Method == "GET" {
		return query(tx, "SELECT * FROM maintenance_history WHERE printer_maintenance_id=? ORDER BY performed_at DESC,id DESC", id)
	}
	if r.Method == "DELETE" {
		_, e = tx.Exec("UPDATE printer_maintenance SET enabled=0 WHERE id=?", id)
		if e != nil {
			return nil, e
		}
		return obj{"status": "removed"}, tx.Commit()
	}
	if r.Method == "POST" {
		var in struct{ Notes string }
		if json.NewDecoder(http.MaxBytesReader(nil, r.Body, 32<<10)).Decode(&in) != nil || len(in.Notes) > 10000 {
			return nil, errInput
		}
		notes := strings.TrimSpace(in.Notes)
		res, e := tx.Exec("INSERT INTO maintenance_history(printer_maintenance_id,hours_at_maintenance,notes) VALUES(?,?,?)", id, v["current_hours"], notes)
		if e != nil {
			return nil, e
		}
		history, _ := res.LastInsertId()
		var uid, user any
		if p.ID != 0 {
			uid = p.ID
			user = p.Username
		}
		_, e = tx.Exec("INSERT INTO maintenance_log_entries(printer_id,printer_name,maintenance_type_id,maintenance_type_name,hours_at_maintenance,notes,performed_by_id,performed_by_username,history_id) VALUES(?,?,?,?,?,?,?,?,?)", v["printer_id"], v["printer_name"], v["maintenance_type_id"], v["maintenance_type_name"], v["current_hours"], notes, uid, user, history)
		if e != nil {
			return nil, e
		}
		_, e = tx.Exec("UPDATE printer_maintenance SET last_performed_at=CURRENT_TIMESTAMP,last_performed_hours=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", v["current_hours"], id)
		if e != nil {
			return nil, e
		}
	} else {
		var in struct {
			Hours   *float64 `json:"custom_interval_hours"`
			Kind    *string  `json:"custom_interval_type"`
			Enabled *bool
		}
		if json.NewDecoder(r.Body).Decode(&in) != nil || in.Hours != nil && *in.Hours < 1 || in.Kind != nil && *in.Kind != "days" && *in.Kind != "hours" {
			return nil, errInput
		}
		_, e = tx.Exec("UPDATE printer_maintenance SET custom_interval_hours=COALESCE(?,custom_interval_hours),custom_interval_type=COALESCE(?,custom_interval_type),enabled=COALESCE(?,enabled),updated_at=CURRENT_TIMESTAMP WHERE id=?", in.Hours, in.Kind, in.Enabled, id)
		if e != nil {
			return nil, e
		}
	}
	rows, e = query(tx, itemStatus+" AND i.id=?", id)
	if e != nil {
		return nil, e
	}
	evaluate(rows[0])
	return rows[0], tx.Commit()
}
func (s *Service) maintenanceLog(r *http.Request) (any, error) {
	p := access.From(r.Context())
	rows, e := query(s.DB, "SELECT * FROM maintenance_log_entries ORDER BY performed_at DESC,id DESC")
	if e != nil {
		return nil, e
	}
	out := []obj{}
	for _, v := range rows {
		id, ok := v["printer_id"].(int64)
		if (!ok && !p.Admin) || ok && !p.AllowsPrinter(id) || r.PathValue("printer") != "" && fmt.Sprint(id) != r.PathValue("printer") {
			continue
		}
		out = append(out, v)
	}
	limit := 100
	offset := 0
	if n, e := strconv.Atoi(r.URL.Query().Get("limit")); e == nil && n > 0 && n <= 1000 {
		limit = n
	}
	if n, e := strconv.Atoi(r.URL.Query().Get("offset")); e == nil && n > 0 {
		offset = n
	}
	total := len(out)
	start := min(offset, total)
	return obj{"items": out[start:min(start+limit, total)], "total": total}, nil
}
func (s *Service) assignMaintenance(r *http.Request) (any, error) {
	id, _ := strconv.ParseInt(r.PathValue("printer"), 10, 64)
	if !access.From(r.Context()).AllowsPrinter(id) {
		return nil, sql.ErrNoRows
	}
	_, e := s.DB.Exec("INSERT INTO printer_maintenance(printer_id,maintenance_type_id,enabled,last_performed_hours) VALUES(?,?,1,0) ON CONFLICT(printer_id,maintenance_type_id) DO UPDATE SET enabled=1", id, r.PathValue("type"))
	if e != nil {
		return nil, errInput
	}
	rows, e := query(s.DB, itemStatus+" AND p.id=? AND t.id=?", id, r.PathValue("type"))
	if e != nil || len(rows) == 0 {
		return nil, sql.ErrNoRows
	}
	evaluate(rows[0])
	return rows[0], nil
}
func (s *Service) printerHours(r *http.Request) (any, error) {
	id, _ := strconv.ParseInt(r.PathValue("printer"), 10, 64)
	if !access.From(r.Context()).AllowsPrinter(id) {
		return nil, sql.ErrNoRows
	}
	h, e := strconv.ParseFloat(r.URL.Query().Get("total_hours"), 64)
	if e != nil || h < 0 {
		return nil, errInput
	}
	res, e := s.DB.Exec("UPDATE printers SET print_hours_offset=?-COALESCE(runtime_seconds,0)/3600.0 WHERE id=?", h, id)
	if e != nil {
		return nil, e
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, sql.ErrNoRows
	}
	return obj{"printer_id": id, "total_hours": h}, nil
}

func (s *Service) maintenanceLogRoutes(api *http.ServeMux) {
	for _, base := range []string{"/api/v1/maintenance/log", "/api/v1/maintenance/printers/{printer}/log"} {
		api.HandleFunc("DELETE "+base, s.purgeLog)
		api.HandleFunc("GET "+base+"/export", s.exportLog)
	}
	api.HandleFunc("DELETE /api/v1/maintenance/log/{entry}", s.purgeLog)
}
func scopedEntry(v obj, r *http.Request) bool {
	p := access.From(r.Context())
	id, ok := v["printer_id"].(int64)
	if !ok && p.PrinterIDs != nil || ok && !p.AllowsPrinter(id) {
		return false
	}
	return r.PathValue("printer") == "" || fmt.Sprint(id) == r.PathValue("printer")
}
func (s *Service) exportLog(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "maintenance:read") {
		return
	}
	rows, e := query(s.DB, "SELECT * FROM maintenance_log_entries ORDER BY performed_at DESC,id DESC")
	if e != nil {
		httpx.Error(w, 500, "Maintenance logs unavailable")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="maintenance.csv"`)
	out := csv.NewWriter(w)
	defer out.Flush()
	keys := []string{"performed_at", "printer_name", "maintenance_type_name", "hours_at_maintenance", "notes", "performed_by_username"}
	out.Write(keys)
	for _, v := range rows {
		if !scopedEntry(v, r) {
			continue
		}
		record := make([]string, len(keys))
		for i, k := range keys {
			if v[k] != nil {
				raw := fmt.Sprint(v[k])
				if strings.HasPrefix(raw, "=") || strings.HasPrefix(raw, "+") || strings.HasPrefix(raw, "@") || strings.HasPrefix(raw, "-") {
					raw = "'" + raw
				}
				record[i] = raw
			}
		}
		if out.Write(record) != nil {
			return
		}
	}
}
func (s *Service) purgeLog(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "maintenance:delete") {
		return
	}
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Maintenance database unavailable")
		return
	}
	defer tx.Rollback()
	stmt := "SELECT id,printer_id,history_id FROM maintenance_log_entries"
	var arg any
	if id := r.PathValue("entry"); id != "" {
		stmt += " WHERE id=?"
		arg = id
	} else {
		days, e := strconv.Atoi(r.URL.Query().Get("older_than_days"))
		if e != nil || days < 1 || days > 36500 {
			httpx.Error(w, 422, "older_than_days must be from 1 to 36500")
			return
		}
		stmt += " WHERE performed_at<datetime('now',?)"
		arg = fmt.Sprintf("-%d days", days)
	}
	rows, e := query(tx, stmt, arg)
	if e != nil {
		httpx.Error(w, 500, "Cannot read maintenance history")
		return
	}
	removed := 0
	for _, v := range rows {
		if !scopedEntry(v, r) {
			continue
		}
		if _, e = tx.Exec("DELETE FROM maintenance_log_entries WHERE id=?", v["id"]); e == nil && v["history_id"] != nil {
			_, e = tx.Exec("DELETE FROM maintenance_history WHERE id=?", v["history_id"])
		}
		if e != nil {
			httpx.Error(w, 409, "Cannot remove log entry")
			return
		}
		removed++
	}
	if r.PathValue("entry") != "" && removed == 0 {
		httpx.Error(w, 404, "Unknown maintenance entry")
		return
	}
	if e = tx.Commit(); e != nil {
		httpx.Error(w, 500, "Cannot save changes")
		return
	}
	httpx.JSON(w, 200, obj{"status": "deleted", "deleted": removed, "id": r.PathValue("entry")})
}
