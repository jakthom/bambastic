package management

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/spoolbuddy"
	"golang.org/x/crypto/ssh"
)

func initializeStation(db *sql.DB) error {
	_, e := db.Exec(`CREATE TABLE IF NOT EXISTS bambastic_station_diagnostics(device INTEGER NOT NULL REFERENCES spoolbuddy_devices(id) ON DELETE CASCADE, diagnostic TEXT NOT NULL, success BOOLEAN NOT NULL, output TEXT NOT NULL,exit_code INTEGER NOT NULL, timestamp DATETIME DEFAULT CURRENT_TIMESTAMP, PRIMARY KEY(device,diagnostic));
CREATE TABLE IF NOT EXISTS bambastic_station_rekey(device INTEGER PRIMARY KEY REFERENCES spoolbuddy_devices(id) ON DELETE CASCADE,next_hash TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bambastic_station_actor(device INTEGER PRIMARY KEY REFERENCES spoolbuddy_devices(id) ON DELETE CASCADE,actor_id INTEGER NOT NULL,key_id INTEGER NOT NULL);
UPDATE spoolbuddy_devices SET update_status='failed',update_message='Update interrupted by server restart; check station version' WHERE update_status='updating';`)
	return e
}
func bindStation(r request, deviceID int64) error {
	p := access.From(r.Context())
	if _, e := r.tx.Exec("INSERT OR IGNORE INTO bambastic_station_actor(device,actor_id,key_id) VALUES(?,?,?)", deviceID, p.ID, p.KeyID); e != nil {
		return e
	}
	var actor, key int64
	if e := r.tx.QueryRow("SELECT actor_id,key_id FROM bambastic_station_actor WHERE device=?", deviceID).Scan(&actor, &key); e != nil {
		return e
	}
	if actor != p.ID || key != p.KeyID {
		if p.KeyID == 0 {
			return denied()
		}
		// The new key has already authenticated at the HTTP boundary. Only the
		// exact credential explicitly supplied by the operator may take over.
		res, e := r.tx.Exec(`UPDATE bambastic_station_actor SET actor_id=?,key_id=? WHERE device=? AND EXISTS(
            SELECT 1 FROM bambastic_station_rekey n JOIN api_keys k ON k.key_hash=n.next_hash WHERE n.device=? AND k.id=? AND k.enabled)`, p.ID, p.KeyID, deviceID, deviceID, p.KeyID)
		if e != nil {
			return e
		}
		n, e := res.RowsAffected()
		if e != nil {
			return e
		}
		if n != 1 {
			return denied()
		}
		_, e = r.tx.Exec("DELETE FROM bambastic_station_rekey WHERE device=?", deviceID)
		return e
	}
	return nil
}
func stationCommand(k string) bool {
	switch k {
	case "reboot", "shutdown", "restart_daemon", "restart_browser", "apply_system_config", "run_scale_diag", "run_nfc_diag", "run_read_tag_diag":
		return true
	}
	return false
}
func diagnosticName(k string) bool { return k == "scale" || k == "nfc" || k == "read_tag" }
func stationBusy(d row) error {
	if cmd := str(d["pending_command"]); cmd != "" {
		return &problem{409, "A station command is already pending"}
	}
	return nil
}
func stationOnline(d row) bool {
	t, e := time.Parse(time.RFC3339Nano, str(d["last_seen"]))
	return e == nil && time.Since(t) < 90*time.Second
}
func (s *Service) stationPayload(dev, reply row) error {
	var payload row
	if v := str(dev["pending_system_payload"]); v != "" {
		if e := json.Unmarshal([]byte(v), &payload); e != nil {
			return e
		}
		if sealed := str(payload["api_key"]); sealed != "" {
			if s.OpenStationSecret == nil {
				return errors.New("station secret storage unavailable")
			}
			plain, e := s.OpenStationSecret(sealed)
			if e != nil {
				return e
			}
			payload["api_key"] = plain
		}
	}
	reply["pending_system_payload"] = payload
	return nil
}
func (s *Service) stationSigningKey(r request) (ssh.Signer, error) {
	const key = "bambastic_station_ssh_private_key"
	secret := setting(r.Context(), r.tx, key, "")
	if s.SealStationSecret == nil || s.OpenStationSecret == nil {
		return nil, &problem{503, "Station key storage unavailable"}
	}
	if secret != "" {
		v, e := s.OpenStationSecret(secret)
		if e != nil {
			return nil, e
		}
		seed, e := base64.StdEncoding.DecodeString(v)
		if e != nil || len(seed) != ed25519.SeedSize {
			return nil, errors.New("invalid station pairing key")
		}
		return ssh.NewSignerFromKey(ed25519.NewKeyFromSeed(seed))
	}
	_, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return nil, e
	}
	sealed := s.SealStationSecret(base64.StdEncoding.EncodeToString(priv.Seed()))
	if sealed == "" {
		return nil, errors.New("station key cannot be sealed")
	}
	if e = access.Put(r.tx, key, sealed); e != nil {
		return nil, e
	}
	return ssh.NewSignerFromKey(priv)
}
func (s *Service) stationRoutes(m *http.ServeMux) {
	const p = "spoolbuddy/devices/{device}/"
	const dg = "spoolbuddy/diagnostics/{device}/"
	s.route(m, "POST", p+"system/config", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if e = stationBusy(dev); e != nil {
			return nil, e
		}
		v, e := r.body()
		if e != nil {
			return nil, e
		}
		backend := strings.TrimRight(strings.TrimSpace(str(v["backend_url"])), "/")
		u, e := url.Parse(backend)
		if e != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, invalid("Invalid backend URL")
		}
		if _, e = r.tx.Exec("DELETE FROM bambastic_station_rekey WHERE device=?", dev["id"]); e != nil {
			return nil, e
		}
		payload := row{"backend_url": backend}
		if token := strings.TrimSpace(str(v["api_key"])); token != "" {
			if s.SealStationSecret == nil {
				return nil, &problem{503, "Station secret storage unavailable"}
			}
			digest := sha256.Sum256([]byte(token))
			if _, e = r.tx.Exec("INSERT INTO bambastic_station_rekey(device,next_hash) VALUES(?,?)", dev["id"], hex.EncodeToString(digest[:])); e != nil {
				return nil, e
			}
			payload["api_key"] = s.SealStationSecret(token)
			if payload["api_key"] == "" {
				return nil, errors.New("station credential could not be sealed")
			}
		}
		return row{"status": "queued", "message": "Station configuration queued"}, update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": "apply_system_config", "pending_system_payload": jsonText(payload)})
	})
	s.route(m, "POST", p+"system/command", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if !stationOnline(dev) {
			return nil, &problem{409, "Station is offline"}
		}
		if e = stationBusy(dev); e != nil {
			return nil, e
		}
		v, e := r.body()
		if e != nil {
			return nil, e
		}
		cmd := str(v["command"])
		switch cmd {
		case "reboot", "shutdown", "restart_daemon", "restart_browser":
		default:
			return nil, invalid("Unsupported station command")
		}
		return row{"status": "queued", "command": cmd}, update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": cmd, "pending_system_payload": nil})
	})
	s.route(m, "POST", p+"system/command-result", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if e = bindStation(r, intID(dev["id"])); e != nil {
			return nil, e
		}
		v, e := r.body()
		if e != nil {
			return nil, e
		}
		cmd := str(v["command"])
		if !stationCommand(cmd) {
			return nil, invalid("Unsupported station command")
		}
		if str(dev["pending_command"]) != "" && dev["pending_command"] != cmd {
			return nil, &problem{409, "Station command mismatch"}
		}
		if dev["pending_command"] == cmd {
			if cmd == "apply_system_config" && !truth(v["success"]) {
				if _, e = r.tx.Exec("DELETE FROM bambastic_station_rekey WHERE device=?", dev["id"]); e != nil {
					return nil, e
				}
			}
			if e = update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"pending_command": nil, "pending_system_payload": nil}); e != nil {
				return nil, e
			}
		}
		r.event(row{"type": "spoolbuddy_system_command_result", "device_id": dev["device_id"], "command": cmd, "success": truth(v["success"]), "message": str(v["message"])})
		return row{"status": "ok"}, nil
	})
	s.route(m, "POST", dg+"run", inventoryWrite, func(r request) (any, error) {
		d, e := device(r)
		if e != nil {
			return nil, e
		}
		if e = stationBusy(d); e != nil {
			return nil, e
		}
		k := r.URL.Query().Get("diagnostic")
		if !diagnosticName(k) {
			return nil, invalid("Unknown diagnostic")
		}
		if _, e = r.tx.Exec("DELETE FROM bambastic_station_diagnostics WHERE device=? AND diagnostic=?", d["id"], k); e != nil {
			return nil, e
		}
		return row{"status": "queued", "diagnostic": k, "message": "Diagnostic queued"}, update(r.tx, "spoolbuddy_devices", intID(d["id"]), row{"pending_command": "run_" + k + "_diag", "pending_system_payload": nil})
	})
	s.route(m, "GET", dg+"result", inventoryRead, func(r request) (any, error) {
		d, e := device(r)
		if e != nil {
			return nil, e
		}
		k := r.URL.Query().Get("diagnostic")
		if !diagnosticName(k) {
			return nil, invalid("Unknown diagnostic")
		}
		return one(r.tx, "SELECT diagnostic,success,output,exit_code,timestamp FROM bambastic_station_diagnostics WHERE device=? AND diagnostic=?", d["id"], k)
	})
	s.route(m, "POST", dg+"result", inventoryWrite, func(r request) (any, error) {
		d, e := device(r)
		if e != nil {
			return nil, e
		}
		if e = bindStation(r, intID(d["id"])); e != nil {
			return nil, e
		}
		v, e := r.body()
		if e != nil {
			return nil, e
		}
		k := str(v["diagnostic"])
		if !diagnosticName(k) || len(str(v["output"])) > 65536 {
			return nil, invalid("Invalid diagnostic report")
		}
		if d["pending_command"] != "run_"+k+"_diag" {
			return nil, &problem{409, "Diagnostic does not match pending command"}
		}
		if _, e = r.tx.Exec("INSERT INTO bambastic_station_diagnostics(device,diagnostic,success,output,exit_code) VALUES(?,?,?,?,?) ON CONFLICT(device,diagnostic) DO UPDATE SET success=excluded.success,output=excluded.output,exit_code=excluded.exit_code,timestamp=CURRENT_TIMESTAMP", d["id"], k, truth(v["success"]), str(v["output"]), num(v["exit_code"])); e != nil {
			return nil, e
		}
		r.event(row{"type": "spoolbuddy_diagnostic", "device_id": d["device_id"], "diagnostic": k})
		return row{"status": "ok"}, update(r.tx, "spoolbuddy_devices", intID(d["id"]), row{"pending_command": nil})
	})
	s.route(m, "GET", "spoolbuddy/ssh/public-key", "settings:read", func(r request) (any, error) {
		signer, e := s.stationSigningKey(r)
		if e != nil {
			return nil, e
		}
		return row{"public_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey()))) + " bambastic-spoolbuddy"}, nil
	})
	s.route(m, "GET", p+"update-check", inventoryRead, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		current := str(dev["firmware_version"])
		return row{"current_version": current, "latest_version": spoolbuddy.Version, "update_available": spoolbuddy.IsOlder(current)}, nil
	})
	s.route(m, "POST", p+"update", inventoryWrite, func(r request) (any, error) {
		dev, e := device(r)
		if e != nil {
			return nil, e
		}
		if !stationOnline(dev) {
			return nil, &problem{409, "Station is offline"}
		}
		if dev["update_status"] == "updating" || dev["update_status"] == "pending" {
			return row{"status": "already_updating", "message": "Update is already pending"}, nil
		}
		if _, e = s.stationSigningKey(r); e != nil {
			return nil, e
		}
		if st, e := os.Stat(s.stationBinary()); e != nil || !st.Mode().IsRegular() {
			return nil, &problem{503, "Linux station binary unavailable on the server"}
		}
		r.event(row{"type": "spoolbuddy_update", "device_id": dev["device_id"], "update_status": "pending"})
		return row{"status": "ok", "message": "Station update queued"}, update(r.tx, "spoolbuddy_devices", intID(dev["id"]), row{"update_status": "pending", "update_message": "Waiting for SSH delivery"})
	})
}
func jsonText(v any) string { b, _ := json.Marshal(v); return string(b) }
func (s *Service) stationBinary() string {
	if p := os.Getenv("BAMBASTIC_SPOOLBUDDY_BINARY"); p != "" {
		return p
	}
	return filepath.Join(s.data, "spoolbuddy", "spoolbuddy-linux-arm64")
}
