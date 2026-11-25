// Package virtualprinter exposes selectable LAN devices for desktop slicers.
package virtualprinter

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bambastic/internal/production"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type object = map[string]any

var models = map[string]string{"BL-P001": "X1C", "BL-P002": "X1", "C13": "X1E", "N6": "X2D", "N9": "A2L", "C11": "P1P", "C12": "P1S", "N7": "P2S", "N2S": "A1", "N1": "A1 Mini", "O1D": "H2D", "O1E": "H2D Pro", "O2D": "H2D Pro", "O1C": "H2C", "O1C2": "H2C", "O1S": "H2S"}
var prefixes = map[string]string{"BL-P001": "00M00A", "BL-P002": "00M00A", "C13": "03W00A", "N6": "20P90A", "N9": "26A19A", "C11": "01S00A", "C12": "01P00A", "N7": "22E00A", "N2S": "03900A", "N1": "03000A", "O1D": "09400A", "O1E": "09400A", "O2D": "09400A", "O1C": "09400A", "O1C2": "09400A", "O1S": "09400A"}

type cfg struct {
	Position                                                  int64  `json:"-"`
	Remote                                                    string `json:",omitempty"`
	ID, Target                                                int64
	Name, Mode, Model, Code, Bind, Serial, TargetIP, Pin      string
	Enabled, Auto, SaveMapping, ForceColor, Inject, Tailscale bool
}
type Service struct {
	db        *sql.DB
	farm      *production.Service
	dir       string
	mu        sync.Mutex
	caMu      sync.Mutex
	instances map[int64]*instance
	problems  map[int64]string
	wake      chan struct{}
}

func New(db *sql.DB, farm *production.Service) (*Service, error) {
	dir := filepath.Join(farm.StorageRoot(), "virtual-printers")
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	_, e := db.Exec("CREATE TABLE IF NOT EXISTS bb_vp_titles(upload_id INTEGER PRIMARY KEY REFERENCES pending_uploads(id) ON DELETE CASCADE,title TEXT NOT NULL); CREATE TABLE IF NOT EXISTS bb_vp_receipts(vp_id INTEGER NOT NULL, filename TEXT NOT NULL, config_key TEXT NOT NULL, completed INTEGER NOT NULL, PRIMARY KEY(vp_id,filename,config_key)); CREATE TABLE IF NOT EXISTS bb_review_intake(upload_id INTEGER PRIMARY KEY, virtual_id INTEGER NOT NULL, target_id INTEGER NOT NULL DEFAULT 0); CREATE TABLE IF NOT EXISTS bb_vp_received(id INTEGER PRIMARY KEY, vp_id INTEGER NOT NULL, path TEXT NOT NULL, filename TEXT NOT NULL, source TEXT NOT NULL, config_key TEXT NOT NULL, options TEXT NOT NULL DEFAULT '{}', ready_at INTEGER NOT NULL, error TEXT); CREATE TABLE IF NOT EXISTS bb_vp_job(vp_id INTEGER NOT NULL, filename TEXT NOT NULL, queue_id INTEGER PRIMARY KEY REFERENCES print_queue(id) ON DELETE CASCADE, created INTEGER NOT NULL, config_key TEXT NOT NULL)")
	return &Service{db: db, farm: farm, dir: dir, instances: map[int64]*instance{}, problems: map[int64]string{}, wake: make(chan struct{}, 1)}, e
}
func str(x any) string {
	if x == nil {
		return ""
	}
	return fmt.Sprint(x)
}
func number(x any) int64 { n, _ := strconv.ParseInt(str(x), 10, 64); return n }
func yes(x any) bool     { return str(x) == "true" || str(x) == "1" }
func key() string        { b := make([]byte, 8); rand.Read(b); return hex.EncodeToString(b) }
func (s *Service) config(ctx context.Context) ([]cfg, error) {
	rows, e := s.db.QueryContext(ctx, `SELECT v.id,v.name,v.mode,coalesce(v.model,'BL-P001'),coalesce(nullif(p.access_code,''),v.access_code,''),coalesce(v.bind_ip,''),v.serial_suffix,coalesce(v.target_printer_id,0),coalesce(p.ip_address,''),coalesce(p.serial_number,''),coalesce(t.fingerprint,''),v.enabled,v.auto_dispatch,v.save_ams_mapping,v.queue_force_color_match,v.gcode_injection,NOT v.tailscale_disabled,v.position,coalesce(v.remote_interface_ip,'') FROM virtual_printers v LEFT JOIN printers p ON p.id=v.target_printer_id LEFT JOIN bb_printer_trust t ON t.printer_id=p.id ORDER BY v.position,v.id`)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []cfg{}
	for rows.Next() {
		var c cfg
		var serial, suffix string
		if e = rows.Scan(&c.ID, &c.Name, &c.Mode, &c.Model, &c.Code, &c.Bind, &suffix, &c.Target, &c.TargetIP, &serial, &c.Pin, &c.Enabled, &c.Auto, &c.SaveMapping, &c.ForceColor, &c.Inject, &c.Tailscale, &c.Position, &c.Remote); e != nil {
			return nil, e
		}
		c.Serial = prefixes[c.Model] + suffix
		switch c.Mode {
		case "immediate":
			c.Mode = "archive"
		case "print_queue":
			c.Mode = "queue"
		}
		if c.Mode == "proxy" {
			c.Serial = serial
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *Service) visible(p access.Principal, c cfg) bool {
	return c.Target == 0 && p.PrinterIDs == nil || c.Target > 0 && p.AllowsPrinter(c.Target)
}
func (s *Service) describe(c cfg) object {
	var pending int
	_ = s.db.QueryRow("SELECT count(*) FROM bb_vp_received WHERE vp_id=? AND error IS NULL", c.ID).Scan(&pending)
	s.mu.Lock()
	defer s.mu.Unlock()
	return object{"remote_interface_ip": c.Remote, "position": c.Position, "id": c.ID, "name": c.Name, "enabled": c.Enabled, "model": c.Model, "model_name": models[c.Model], "mode": c.Mode, "bind_ip": c.Bind, "serial": c.Serial, "target_printer_id": nullable(c.Target), "auto_dispatch": c.Auto, "save_ams_mapping": c.SaveMapping, "queue_force_color_match": c.ForceColor, "gcode_injection": c.Inject, "tailscale_disabled": !c.Tailscale, "access_code_set": c.Code != "", "status": object{"pending_files": pending, "running": s.instances[c.ID] != nil, "error": s.problems[c.ID]}}
}
func nullable(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func (s *Service) changed() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Service) Register(m *http.ServeMux) {
	s.networkRoutes(m)
	api := "/api/v1"
	m.HandleFunc("GET "+api+"/virtual-printers", s.configHTTP)
	m.HandleFunc("POST "+api+"/virtual-printers", s.configHTTP)
	for _, method := range []string{"GET", "PUT", "DELETE"} {
		m.HandleFunc(method+" "+api+"/virtual-printers/{id}", s.configHTTP)
	}
	m.HandleFunc("GET "+api+"/virtual-printers/{id}/diagnostic", s.configHTTP)
	m.HandleFunc("GET "+api+"/settings/virtual-printer/models", func(w http.ResponseWriter, r *http.Request) {
		if access.Require(w, r, "settings:read") {
			httpx.JSON(w, 200, object{"default": "BL-P001", "models": models})
		}
	})
	m.HandleFunc("GET "+api+"/virtual-printers/ca-certificate", func(w http.ResponseWriter, r *http.Request) {
		if !access.Require(w, r, "settings:read") {
			return
		}
		cert, _, e := s.authority()
		if e != nil {
			httpx.Error(w, 500, "Certificate unavailable")
			return
		}
		if r.URL.Query().Get("download") != "1" && !strings.Contains(r.Header.Get("Accept"), "application/x-pem-file") {
			info, e := caInfo(cert)
			if e != nil {
				httpx.Error(w, 500, "Certificate invalid")
				return
			}
			httpx.JSON(w, 200, info)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="bambastic-ca.crt"`)
		w.Header().Set("Content-Type", "application/x-pem-file")
		w.Write(cert)
	})
	for _, p := range []string{"/pending-uploads/{$}", "/pending-uploads/count", "/pending-uploads/{id}"} {
		m.HandleFunc("GET "+api+p, s.pending)
	}
	m.HandleFunc("DELETE "+api+"/pending-uploads/{id}", s.pending)
	m.HandleFunc("POST "+api+"/pending-uploads/{id}/archive", s.pending)
	m.HandleFunc("POST "+api+"/pending-uploads/archive-all", s.pending)
	m.HandleFunc("DELETE "+api+"/pending-uploads/discard-all", s.pending)
}
func (s *Service) configHTTP(w http.ResponseWriter, r *http.Request) {
	perm := "settings:read"
	if r.Method != "GET" {
		perm = "settings:update"
	}
	if !access.Require(w, r, perm) {
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	all, e := s.config(ctx)
	if e != nil {
		httpx.Error(w, 500, "Configuration unavailable")
		return
	}
	var c cfg
	id := number(r.PathValue("id"))
	if r.PathValue("id") != "" {
		for _, v := range all {
			if v.ID == id && s.visible(p, v) {
				c = v
				break
			}
		}
		if c.ID == 0 {
			httpx.Error(w, 404, "Virtual printer not found")
			return
		}
	}
	if r.Method == "GET" {
		if id > 0 {
			if strings.HasSuffix(r.URL.Path, "/diagnostic") {
				httpx.JSON(w, 200, s.diagnose(c))
				return
			}
			httpx.JSON(w, 200, s.describe(c))
			return
		}
		out := []object{}
		for _, c := range all {
			if s.visible(p, c) {
				out = append(out, s.describe(c))
			}
		}
		httpx.JSON(w, 200, object{"printers": out, "models": models})
		return
	}
	if r.Method == "DELETE" {
		_, e = s.db.ExecContext(ctx, "DELETE FROM virtual_printers WHERE id=?", id)
		if e != nil {
			httpx.Error(w, 409, "Cannot delete virtual printer")
			return
		}
		s.changed()
		httpx.JSON(w, 200, object{"success": true})
		return
	}
	var in object
	if !httpx.Decode(w, r, &in) {
		return
	}
	if id == 0 {
		c = cfg{Name: "Bambastic", Mode: "archive", Model: "BL-P001", Auto: true}
	}
	for name, dst := range map[string]*string{"name": &c.Name, "mode": &c.Mode, "model": &c.Model, "bind_ip": &c.Bind, "remote_interface_ip": &c.Remote, "access_code": &c.Code} {
		if x, ok := in[name]; ok {
			*dst = strings.TrimSpace(str(x))
		}
	}
	for name, dst := range map[string]*bool{"enabled": &c.Enabled, "auto_dispatch": &c.Auto, "save_ams_mapping": &c.SaveMapping, "queue_force_color_match": &c.ForceColor, "gcode_injection": &c.Inject} {
		if x, ok := in[name]; ok {
			*dst = yes(x)
		}
	}
	if x, ok := in["tailscale_disabled"]; ok {
		c.Tailscale = !yes(x)
	}
	if x, ok := in["target_printer_id"]; ok {
		c.Target = number(x)
	}
	switch c.Mode {
	case "immediate":
		c.Mode = "archive"
	case "print_queue":
		c.Mode = "queue"
	case "archive", "review", "queue", "proxy":
	default:
		httpx.Error(w, 422, "Unknown intake mode")
		return
	}
	// access_code belongs to a standalone virtual printer. Changing a physical
	// printer's saved LAN code requires an explicit target and printer permission.
	targetCode, ok := in["target_access_code"].(string)
	if in["target_access_code"] != nil && !ok {
		httpx.Error(w, 422, "Target printer LAN access code must be a string")
		return
	}
	if targetCode != "" && (len(targetCode) != 8 || number(in["target_printer_id"]) <= 0) {
		httpx.Error(w, 422, "Select a target printer and enter its eight-character LAN access code")
		return
	}
	if targetCode != "" && !access.Require(w, r, "printers:update") {
		return
	}
	var savedCode, serial, pin string
	if c.Target > 0 {
		const targetQuery = `SELECT p.ip_address,p.access_code,p.serial_number,coalesce(t.fingerprint,'')
			FROM printers p LEFT JOIN bb_printer_trust t ON t.printer_id=p.id WHERE p.id=?`
		if !p.AllowsPrinter(c.Target) || s.db.QueryRowContext(ctx, targetQuery, c.Target).Scan(&c.TargetIP, &savedCode, &serial, &pin) != nil {
			httpx.Error(w, 404, "Target printer not found")
			return
		}
		c.Code = savedCode
		if targetCode != "" {
			c.Code = targetCode
		}
	} else if p.PrinterIDs != nil {
		httpx.Error(w, 403, "A permitted target printer is required")
		return
	}
	if c.Bind == "0.0.0.0" {
		c.Bind = ""
	}
	if c.Remote != "" && (net.ParseIP(c.Remote) == nil || net.ParseIP(c.Remote).To4() == nil || net.ParseIP(c.Remote).IsUnspecified() || net.ParseIP(c.Remote).IsMulticast()) {
		httpx.Error(w, 422, "Invalid advertised IPv4 address")
		return
	}
	if c.Name == "" || len(c.Name) > 100 || strings.ContainsAny(c.Name, "\r\n") || models[c.Model] == "" || c.Mode == "proxy" && c.Target == 0 || (c.Enabled && len(c.Code) != 8) || c.Bind != "" && (net.ParseIP(c.Bind) == nil || net.ParseIP(c.Bind).To4() == nil) {
		httpx.Error(w, 422, "Check the name, model, bind address, target and eight-character access code")
		return
	}
	for _, v := range all {
		if c.Enabled && v.Enabled && v.ID != c.ID && (v.Bind == c.Bind || v.Bind == "" || c.Bind == "") {
			httpx.Error(w, 409, "Enabled virtual printers require distinct bind addresses")
			return
		}
	}
	changeTargetCode := targetCode != "" && targetCode != savedCode
	if changeTargetCode {
		target := object{"ip_address": c.TargetIP, "serial_number": serial, "access_code": targetCode}
		if err := s.farm.VerifyPrinterConnection(ctx, target, pin); err != nil {
			httpx.Error(w, 409, err.Error())
			return
		}
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		httpx.Error(w, 500, "Cannot save virtual printer")
		return
	}
	defer tx.Rollback()
	if changeTargetCode {
		// The connection was checked before opening the transaction. Only apply
		// the credential if the target still matches that check.
		result, err := tx.ExecContext(ctx, `UPDATE printers SET access_code=?,updated_at=CURRENT_TIMESTAMP
			WHERE id=? AND access_code=? AND ip_address=? AND serial_number=?
			AND EXISTS (SELECT 1 FROM bb_printer_trust WHERE printer_id=? AND fingerprint=?)`,
			targetCode, c.Target, savedCode, c.TargetIP, serial, c.Target, pin)
		if err != nil {
			httpx.Error(w, 409, "Cannot save the target printer access code")
			return
		}
		if n, err := result.RowsAffected(); err != nil || n != 1 {
			httpx.Error(w, 409, "Target printer settings changed. Review the target and try again")
			return
		}
	}
	if id == 0 {
		var result sql.Result
		result, e = tx.ExecContext(ctx, `INSERT INTO virtual_printers(name,mode,model,access_code,target_printer_id,bind_ip,enabled,auto_dispatch,save_ams_mapping,queue_force_color_match,gcode_injection,tailscale_disabled,remote_interface_ip,serial_suffix,position) SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,cast(coalesce(max(cast(serial_suffix as integer)),391800000)+1 as text),coalesce(max(position),0)+1 FROM virtual_printers`, c.Name, c.Mode, c.Model, c.Code, nullable(c.Target), c.Bind, c.Enabled, c.Auto, c.SaveMapping, c.ForceColor, c.Inject, !c.Tailscale, c.Remote)
		if e == nil {
			c.ID, e = result.LastInsertId()
		}
	} else {
		var result sql.Result
		result, e = tx.ExecContext(ctx, "UPDATE virtual_printers SET name=?,mode=?,model=?,access_code=?,target_printer_id=?,bind_ip=?,enabled=?,auto_dispatch=?,save_ams_mapping=?,queue_force_color_match=?,gcode_injection=?,tailscale_disabled=?,remote_interface_ip=?,updated_at=CURRENT_TIMESTAMP WHERE id=?", c.Name, c.Mode, c.Model, c.Code, nullable(c.Target), c.Bind, c.Enabled, c.Auto, c.SaveMapping, c.ForceColor, c.Inject, !c.Tailscale, c.Remote, id)
		if e == nil {
			if n, err := result.RowsAffected(); err != nil || n != 1 {
				httpx.Error(w, 409, "Virtual printer no longer exists")
				return
			}
		}
	}
	if e != nil || tx.Commit() != nil {
		httpx.Error(w, 409, "Cannot save virtual printer")
		return
	}
	s.changed()
	all, e = s.config(ctx)
	if e == nil {
		for _, v := range all {
			if v.ID == c.ID {
				httpx.JSON(w, 200, s.describe(v))
				return
			}
		}
	}
	httpx.Error(w, 500, "Cannot read saved configuration")
}

const pendingQuery = "SELECT u.id,u.filename,u.file_size,u.file_path,(SELECT title FROM bb_vp_titles t WHERE t.upload_id=u.id) metadata_title,u.source_ip,u.status,u.tags,u.notes,u.project_id,u.uploaded_at,coalesce(b.target_id,0) target_id FROM pending_uploads u LEFT JOIN bb_review_intake b ON b.upload_id=u.id WHERE u.status='pending'"

func (s *Service) reviewUpload(ctx context.Context, p access.Principal, k int64, archive bool, opts object) (object, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	rs, e := tx.QueryContext(ctx, pendingQuery+" AND u.id=?", k)
	if e != nil {
		return nil, e
	}
	rows, e := httpx.Rows(rs)
	if e != nil {
		return nil, e
	}
	if len(rows) != 1 || !s.visible(p, cfg{Target: number(rows[0]["target_id"])}) {
		return nil, sql.ErrNoRows
	}
	pick := rows[0]
	rel := str(pick["file_path"])
	if !filepath.IsLocal(rel) {
		return nil, errors.New("upload path unavailable")
	}
	path := filepath.Join(s.farm.StorageRoot(), rel)
	out := object{"success": true}
	if archive {
		if !p.Has("archives:create") {
			return nil, errors.New("archive creation denied")
		}
		file, e := os.Open(path)
		if e != nil {
			return nil, errors.New("uploaded file is unavailable")
		}
		aid, _, e := s.farm.Intake(ctx, tx, p, file, str(pick["filename"]), number(pick["target_id"]), number(opts["project_id"]), "", false, false, nil)
		file.Close()
		if e != nil {
			return nil, e
		}
		for _, field := range []string{"tags", "notes"} {
			if v := opts[field]; v != nil {
				if _, ok := v.(string); !ok {
					return nil, errors.New("invalid archive metadata")
				}
				if _, e = tx.ExecContext(ctx, "UPDATE print_archives SET "+field+"=? WHERE id=?", v, aid); e != nil {
					return nil, e
				}
			}
		}
		_, e = tx.ExecContext(ctx, "UPDATE pending_uploads SET status='archived',archived_id=?,archived_at=CURRENT_TIMESTAMP,tags=?,notes=?,project_id=? WHERE id=?", aid, opts["tags"], opts["notes"], nullable(number(opts["project_id"])), k)
		if e != nil {
			return nil, e
		}
		var name string
		if e = tx.QueryRowContext(ctx, "SELECT coalesce(print_name,filename) FROM print_archives WHERE id=?", aid).Scan(&name); e != nil {
			return nil, e
		}
		out = object{"id": aid, "archive_id": aid, "print_name": name, "filename": pick["filename"]}
	} else {
		if _, e = tx.ExecContext(ctx, "UPDATE pending_uploads SET status='discarded' WHERE id=?", k); e != nil {
			return nil, e
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	os.Remove(path)
	return out, nil
}
func (s *Service) pending(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := access.From(ctx)
	perm := "queue:read_all"
	if r.Method == "POST" {
		perm = "queue:create"
	} else if r.Method == "DELETE" {
		perm = "queue:delete_all"
	}
	if !(r.Method == "GET" && p.Has("queue:read_own")) && !access.Require(w, r, perm) {
		return
	}
	opts := object{}
	if r.Method == "POST" && r.ContentLength != 0 {
		if !httpx.Decode(w, r, &opts) {
			return
		}
	}
	if key := number(r.PathValue("id")); key > 0 && r.Method != "GET" {
		out, e := s.reviewUpload(ctx, p, key, r.Method == "POST", opts)
		if errors.Is(e, sql.ErrNoRows) {
			httpx.Error(w, 404, "Upload not found")
			return
		}
		if e != nil {
			httpx.Error(w, 400, e.Error())
			return
		}
		httpx.JSON(w, 200, out)
		return
	}
	rs, e := s.db.QueryContext(ctx, pendingQuery+" ORDER BY u.id DESC")
	if e != nil {
		httpx.Error(w, 500, "Storage unavailable")
		return
	}
	rows, e := httpx.Rows(rs)
	if e != nil {
		httpx.Error(w, 500, "Storage unavailable")
		return
	}
	source, _ := access.Setting(ctx, s.db, "virtual_printer_archive_name_source", "metadata")
	out := []object{}
	for _, v := range rows {
		if !s.visible(p, cfg{Target: number(v["target_id"])}) {
			continue
		}
		delete(v, "file_path")
		v["display_name"] = production.IntakeDisplayName("", str(v["filename"]), true)
		if source != "filename" && str(v["metadata_title"]) != "" {
			v["display_name"] = v["metadata_title"]
		}
		delete(v, "metadata_title")
		if r.PathValue("id") != "" && number(r.PathValue("id")) == number(v["id"]) {
			httpx.JSON(w, 200, v)
			return
		}
		out = append(out, v)
	}
	if r.PathValue("id") != "" {
		httpx.Error(w, 404, "Upload not found")
		return
	}
	if r.Method == "GET" {
		if strings.HasSuffix(r.URL.Path, "/count") {
			httpx.JSON(w, 200, object{"count": len(out)})
		} else {
			httpx.JSON(w, 200, out)
		}
		return
	}
	good, bad := 0, 0
	for _, v := range out {
		_, e = s.reviewUpload(ctx, p, number(v["id"]), r.Method == "POST", opts)
		if e != nil {
			bad++
		} else {
			good++
		}
	}
	if r.Method == "POST" {
		httpx.JSON(w, 200, object{"archived": good, "failed": bad})
	} else {
		httpx.JSON(w, 200, object{"discarded": good, "failed": bad})
	}
}
func (s *Service) ingest(ctx context.Context, c cfg, path, name, source string, opts object, staged int64) error {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	if staged != 0 {
		res, e := tx.Exec("DELETE FROM bb_vp_received WHERE id=?", staged)
		if e != nil {
			return e
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return errors.New("upload already processed")
		}
		if _, e = tx.ExecContext(ctx, "INSERT OR REPLACE INTO bb_vp_receipts VALUES(?,?,?,unixepoch())", c.ID, name, c.fingerprint()); e != nil {
			return e
		}
	}
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	if c.Mode == "review" {
		rel := filepath.Join("virtual-printers", "review-"+key()+".3mf")
		stored := filepath.Join(s.farm.StorageRoot(), rel)
		if e = os.Link(path, stored); e != nil {
			return e
		}
		ok := false
		defer func() {
			if !ok {
				os.Remove(stored)
			}
		}()
		fi, e := f.Stat()
		if e != nil {
			return e
		}
		res, e := tx.Exec("INSERT INTO pending_uploads(filename,file_path,file_size,source_ip,status) VALUES(?,?,?,?,'pending')", name, rel, fi.Size(), source)
		if e != nil {
			return e
		}
		id, _ := res.LastInsertId()
		if _, e = tx.ExecContext(ctx, "INSERT INTO bb_vp_titles VALUES(?,?)", id, production.IntakeDisplayName(path, name, false)); e != nil {
			return e
		}
		if _, e = tx.Exec("INSERT INTO bb_review_intake VALUES(?,?,?)", id, c.ID, c.Target); e != nil {
			return e
		}
		e = tx.Commit()
		ok = e == nil
		return e
	}
	if opts == nil {
		opts = object{}
	}
	if !c.SaveMapping || c.Target == 0 || c.ForceColor {
		delete(opts, "ams_mapping")
	}
	opts["gcode_injection"] = c.Inject
	opts["force_color_match"] = c.ForceColor
	p := access.Principal{Admin: true}
	if c.Target != 0 {
		p.PrinterIDs = map[int64]bool{c.Target: true}
	}
	_, jobs, e := s.farm.Intake(access.With(ctx, p), tx, p, f, name, c.Target, 0, models[c.Model], c.Mode == "queue", !c.Auto, opts)
	if e != nil {
		return e
	}
	for _, id := range jobs {
		if _, e = tx.ExecContext(ctx, "INSERT INTO bb_vp_job(vp_id,filename,queue_id,created,config_key) VALUES(?,?,?,unixepoch(),?)", c.ID, name, id, c.fingerprint()); e != nil {
			return e
		}
	}
	return tx.Commit()
}
