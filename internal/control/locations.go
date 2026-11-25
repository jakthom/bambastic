package control

import (
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

var paint = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

func (s *Service) locations(api *http.ServeMux) {
	for _, base := range []string{"/api/v1/printer-locations", "/api/v1/printer-locations/{$}"} {
		for _, m := range []string{"GET", "POST", "PATCH"} {
			api.HandleFunc(m+" "+base, s.location)
		}
	}
	for _, p := range []string{"assign", "delete"} {
		api.HandleFunc("POST /api/v1/printer-locations/"+p, s.location)
	}
}
func (s *Service) location(w http.ResponseWriter, r *http.Request) {
	perm := "printers:update"
	if r.Method == "GET" {
		perm = "printers:read"
	}
	if !access.Require(w, r, perm) {
		return
	}
	p := access.From(r.Context())
	tx, e := s.DB.BeginTx(r.Context(), nil)
	if e != nil {
		httpx.Error(w, 500, "Database unavailable")
		return
	}
	defer tx.Rollback()
	query := func(q string, args ...any) ([]obj, error) {
		rows, e := tx.Query(q, args...)
		if e != nil {
			return nil, e
		}
		return httpx.Rows(rows)
	}
	printers, e := query("SELECT id,location FROM printers")
	if e != nil {
		httpx.Error(w, 500, "Cannot read printers")
		return
	}
	loc := map[int64]string{}
	counts := map[string]int{}
	hidden := map[string]bool{}
	for _, printer := range printers {
		id := printer["id"].(int64)
		name, _ := printer["location"].(string)
		loc[id] = name
		if name == "" {
			continue
		}
		if p.AllowsPrinter(id) {
			counts[name]++
		} else {
			hidden[name] = true
		}
	}
	list, e := query("SELECT id,name,icon,color FROM printer_locations")
	if e != nil {
		httpx.Error(w, 500, "Cannot read locations")
		return
	}
	byName := map[string]obj{}
	for name, count := range counts {
		byName[name] = obj{"name": name, "id": nil, "icon": nil, "color": nil, "printer_count": count}
	}
	for _, area := range list {
		name := area["name"].(string)
		if p.PrinterIDs != nil && counts[name] == 0 {
			continue
		}
		area["printer_count"] = counts[name]
		byName[name] = area
	}
	if r.Method == "GET" {
		areas := []obj{}
		for _, area := range byName {
			areas = append(areas, area)
		}
		sort.Slice(areas, func(i, j int) bool {
			return strings.ToLower(areas[i]["name"].(string)) < strings.ToLower(areas[j]["name"].(string))
		})
		httpx.JSON(w, 200, areas)
		return
	}
	var v struct {
		Name, Icon, Color, Location, NewName *string
		AltName                              *string `json:"new_name"`
		Names                                []string
		IDs                                  []int64 `json:"printer_ids"`
	}
	if !httpx.Decode(w, r, &v) {
		return
	}
	good := func(s string) bool { return strings.TrimSpace(s) == s && s != "" && len(s) <= 100 }
	if v.NewName == nil {
		v.NewName = v.AltName
	}
	if v.Name != nil && !good(*v.Name) || v.NewName != nil && !good(*v.NewName) || v.Location != nil && !good(*v.Location) || v.Color != nil && *v.Color != "" && !paint.MatchString(*v.Color) || v.Icon != nil && len(*v.Icon) > 100 {
		httpx.Error(w, 422, "Invalid location name, color or icon")
		return
	}
	allowed := func(name string) bool {
		if p.Admin || name == "" {
			return true
		}
		var n int
		e := tx.QueryRow("SELECT count(*) FROM group_locations WHERE location=?", name).Scan(&n)
		return e == nil && n == 0 && !hidden[name]
	}
	commit := func(status int, result any) {
		if e := tx.Commit(); e != nil {
			httpx.Error(w, 409, "Location change conflicts with another update")
			return
		}
		httpx.JSON(w, status, result)
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/assign"):
		name := ""
		if v.Location != nil {
			name = *v.Location
		}
		if !allowed(name) {
			httpx.Error(w, 403, "Location is used by a printer grant")
			return
		}
		ids := map[int64]bool{}
		for _, id := range v.IDs {
			old, ok := loc[id]
			if !ok || !p.AllowsPrinter(id) {
				httpx.Error(w, 404, "Printer unavailable")
				return
			}
			if !allowed(old) {
				httpx.Error(w, 403, "Location is used by a printer grant")
				return
			}
			ids[id] = true
		}
		for id := range ids {
			if _, e = tx.Exec("UPDATE printers SET location=? WHERE id=?", v.Location, id); e != nil {
				httpx.Error(w, 409, "Cannot assign printer")
				return
			}
		}
		commit(200, obj{"moved": len(ids)})
	case strings.HasSuffix(r.URL.Path, "/delete"):
		moved, deleted := 0, 0
		seen := map[string]bool{}
		for _, name := range v.Names {
			if seen[name] {
				continue
			}
			seen[name] = true
			if _, ok := byName[name]; !ok {
				continue
			}
			if !allowed(name) {
				httpx.Error(w, 403, "Cannot delete this location")
				return
			}
			res, e := tx.Exec("UPDATE printers SET location=NULL WHERE location=?", name)
			if e != nil {
				httpx.Error(w, 409, "Cannot ungroup printers")
				return
			}
			n, _ := res.RowsAffected()
			moved += int(n)
			if _, e = tx.Exec("DELETE FROM group_locations WHERE location=?", name); e == nil {
				_, e = tx.Exec("DELETE FROM printer_locations WHERE name=?", name)
			}
			if e != nil {
				httpx.Error(w, 409, "Location is in use")
				return
			}
			deleted++
		}
		commit(200, obj{"deleted": deleted, "printers_ungrouped": moved})
	default:
		if v.Name == nil {
			httpx.Error(w, 422, "Location name required")
			return
		}
		old := *v.Name
		name := old
		if v.NewName != nil {
			name = *v.NewName
		}
		if !allowed(old) || !allowed(name) {
			httpx.Error(w, 403, "Location is used by a printer grant")
			return
		}
		var existing int64
		err := tx.QueryRow("SELECT id FROM printer_locations WHERE name=?", old).Scan(&existing)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			httpx.Error(w, 500, "Cannot load location")
			return
		}
		if r.Method == "POST" && existing != 0 {
			httpx.Error(w, 409, "Location already exists")
			return
		}
		if r.Method == "PATCH" && byName[old] == nil {
			httpx.Error(w, 404, "Unknown location")
			return
		}
		var n int
		err = tx.QueryRow("SELECT count(*) FROM (SELECT name FROM printer_locations UNION SELECT location FROM printers) WHERE lower(name)=lower(?) AND name<>?", name, old).Scan(&n)
		if err != nil || n > 0 {
			httpx.Error(w, 409, "Location already exists")
			return
		}
		if existing == 0 {
			if _, e = tx.Exec("INSERT INTO printer_locations(name,icon,color) VALUES(?,?,?)", name, v.Icon, v.Color); e != nil {
				httpx.Error(w, 409, "Cannot create location")
				return
			}
		} else {
			_, e = tx.Exec("UPDATE printer_locations SET name=?,icon=coalesce(?,icon),color=coalesce(?,color),updated_at=CURRENT_TIMESTAMP WHERE id=?", name, v.Icon, v.Color, existing)
		}
		if name != old && e == nil {
			_, e = tx.Exec("UPDATE printers SET location=? WHERE location=?", name, old)
			if e == nil {
				_, e = tx.Exec("UPDATE group_locations SET location=? WHERE location=?", name, old)
			}
		}
		if e != nil {
			httpx.Error(w, 409, "Cannot update location")
			return
		}
		result, e := query("SELECT id,name,icon,color FROM printer_locations WHERE name=?", name)
		if e != nil || len(result) == 0 {
			httpx.Error(w, 500, "Cannot read updated location")
			return
		}
		result[0]["printer_count"] = counts[old]
		status := 200
		if r.Method == "POST" {
			status = 201
		}
		commit(status, result[0])
	}
}
