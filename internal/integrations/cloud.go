package integrations

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const orca = "orca_cloud"
const bambu = "cloud"

var fields = map[string]string{"token": "cloud_token", "email": "cloud_email", "region": "cloud_region", "expired": "cloud_token_invalid_at", "otoken": "orca_cloud_token", "refresh": "orca_cloud_refresh_token", "expires": "orca_cloud_expires_at", "oemail": "orca_cloud_email", "ouser": "orca_cloud_user_id", "pending": "orca_cloud_pending_verifier", "interval": "orca_cloud_pending_state", "started": "orca_cloud_pending_at"}

func globalKey(k string) string {
	if k == "expired" {
		return "bambu_cloud_token_invalid_at"
	}
	if k == "token" || k == "region" || k == "email" {
		return "bambu_" + fields[k]
	}
	return fields[k]
}
func isSecret(k string) bool {
	return k == "token" || k == "otoken" || k == "refresh" || k == "pending"
}
func (s *Service) cloudAccess(ctx context.Context, p access.Principal, perm string) error {
	if !p.Has(perm) {
		return bad(403, "permission_denied", "Insufficient permission")
	}
	if p.KeyID != 0 {
		var allowed bool
		var owner sql.NullInt64
		if e := s.db.QueryRowContext(ctx, "SELECT can_access_cloud,user_id FROM api_keys WHERE id=? AND enabled", p.KeyID).Scan(&allowed, &owner); e != nil || !allowed || !owner.Valid || owner.Int64 != p.ID || p.ID == 0 {
			return bad(403, "cloud_identity_required", "A key with cloud access and an active owner is required")
		}
	}
	if p.ID == 0 {
		enabled, e := s.auth.Enabled(ctx)
		if e != nil {
			return e
		}
		if enabled {
			return bad(403, "cloud_identity_required", "Cloud access requires a signed-in user")
		}
	}
	return nil
}
func (s *Service) readCreds(ctx context.Context, id int64, keys ...string) (map[string]string, error) {
	out := map[string]string{}
	for _, k := range keys {
		col := fields[k]
		if col == "" {
			return nil, errors.New("invalid credential field")
		}
		var v string
		var e error
		if id == 0 {
			v, e = access.Setting(ctx, s.db, globalKey(k), "")
		} else {
			var value sql.NullString
			e = s.db.QueryRowContext(ctx, "SELECT "+col+" FROM users WHERE id=? AND is_active", id).Scan(&value)
			v = value.String
		}
		if e != nil {
			return nil, e
		}
		if isSecret(k) {
			v, e = s.auth.OpenSecret(v)
			if e != nil {
				return nil, bad(409, "cloud_reconfigure", "Reconnect the cloud account")
			}
		}
		out[k] = v
	}
	return out, nil
}
func (s *Service) saveCreds(ctx context.Context, id int64, values map[string]string) error {
	if _, rotating := values["token"]; rotating {
		s.bambuMu.Lock()
		defer s.bambuMu.Unlock()
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for k, v := range values {
		col := fields[k]
		if col == "" {
			return errors.New("invalid token field")
		}
		if isSecret(k) && v != "" {
			v = s.auth.SealSecret(v)
		}
		if id == 0 {
			if v == "" {
				_, e = tx.ExecContext(ctx, "DELETE FROM settings WHERE key=?", globalKey(k))
			} else {
				e = access.Put(tx, globalKey(k), v)
			}
		} else {
			var res sql.Result
			var value any
			if v != "" {
				value = v
			}
			res, e = tx.ExecContext(ctx, "UPDATE users SET "+col+"=? WHERE id=? AND is_active", value, id)
			if e == nil {
				n, _ := res.RowsAffected()
				if n == 0 {
					return errors.New("account no longer active")
				}
			}
		}
		if e != nil {
			return e
		}
	}
	return tx.Commit()
}
func cloudHTTP(ctx context.Context, client *http.Client, method, address, token, contentType string, payload []byte) (object, int, error) {
	r, e := http.NewRequestWithContext(ctx, method, address, bytes.NewReader(payload))
	if e != nil {
		return nil, 0, e
	}
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "Bambastic/0.1")
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	cctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	res, e := client.Do(r.WithContext(cctx))
	if e != nil {
		return nil, 0, bad(502, "cloud_unreachable", "The cloud service is unavailable")
	}
	defer res.Body.Close()
	if res.StatusCode == 204 {
		return object{}, 204, nil
	}
	var raw any
	if e = json.NewDecoder(io.LimitReader(res.Body, 32<<20)).Decode(&raw); e != nil {
		if res.StatusCode == 403 || res.StatusCode == 429 {
			return nil, res.StatusCode, bad(502, "cloud_refused", "Cloud service declined the request; sign in or try again later")
		}
		return nil, res.StatusCode, bad(502, "cloud_unreachable", "Cloud service returned an invalid response")
	}
	if a, ok := raw.([]any); ok {
		return object{"items": a}, res.StatusCode, nil
	}
	o, _ := raw.(object)
	if o == nil {
		o = object{}
	}
	return o, res.StatusCode, nil
}
func cloudFailure(code int) error {
	switch code {
	case 401, 403:
		return bad(409, "cloud_expired", "Cloud sign-in expired or access was denied")
	case 404:
		return bad(404, "cloud_not_found", "Profile or device not found")
	case 429:
		return bad(429, "cloud_rate_limited", "Cloud service is limiting requests")
	default:
		return bad(502, "cloud_failed", "Cloud service returned HTTP "+strconv.Itoa(code))
	}
}
func (s *Service) bambuBase(region string) string {
	if region == "china" || region == "cn" {
		return "https://api.bambulab.cn"
	}
	return "https://api.bambulab.com"
}
func expiredBambuToken(code int, response object) bool {
	return code == http.StatusUnauthorized && (response["code"] == float64(4) || strings.Contains(strings.ToLower(str(response["error"])+" "+str(response["message"])), "please login"))
}

// A slow response to an old token cannot expire a replacement login.
func (s *Service) recordBambuExpiry(ctx context.Context, id int64, token string) {
	s.bambuMu.Lock()
	defer s.bambuMu.Unlock()
	c, e := s.readCreds(ctx, id, "token", "expired")
	if e == nil && c["token"] == token && c["expired"] == "" {
		_ = s.saveCreds(ctx, id, map[string]string{"expired": time.Now().UTC().Format(time.RFC3339)})
	}
}
func (s *Service) bambuStatus(ctx context.Context, id int64) (object, error) {
	c, e := s.readCreds(ctx, id, "token", "email", "region", "expired")
	if e != nil {
		return nil, e
	}
	unknown := object{"is_authenticated": false, "sign_in_expired": c["expired"] != "", "region": nil, "email": nil}
	token := c["token"]
	if token == "" || c["expired"] != "" {
		return unknown, nil
	}
	region := fallback(c["region"], "global")
	digest := sha256.Sum256([]byte(region + "\x00" + token))
	s.bambuMu.Lock()
	fresh := time.Now().Before(s.bambuVerified[digest])
	s.bambuMu.Unlock()
	if !fresh {
		work := s.bambuChecks.DoChan(hex.EncodeToString(digest[:]), func() (any, error) {
			result, code, err := cloudHTTP(ctx, s.client, "GET", s.bambuBase(region)+"/v1/design-user-service/my/preference", token, "", nil)
			if err == nil && code == http.StatusOK {
				s.bambuMu.Lock()
				now := time.Now()
				for key, until := range s.bambuVerified {
					if !now.Before(until) {
						delete(s.bambuVerified, key)
					}
				}
				s.bambuVerified[digest] = now.Add(5 * time.Minute)
				s.bambuMu.Unlock()
			}
			return expiredBambuToken(code, result), nil
		})
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case verdict := <-work:
			if verdict.Val == true {
				s.recordBambuExpiry(ctx, id, token)
				unknown["sign_in_expired"] = true
				return unknown, nil
			}
		}
	}
	return object{"is_authenticated": true, "sign_in_expired": false, "region": region, "email": c["email"]}, nil
}
func (s *Service) bambuRequest(ctx context.Context, id int64, method, endpoint string, req object) (object, error) {
	c, e := s.readCreds(ctx, id, "token", "region")
	if e != nil {
		return nil, e
	}
	if c["token"] == "" {
		return nil, bad(401, "cloud_not_authenticated", "Not authenticated")
	}
	var body []byte
	if req != nil {
		body, _ = json.Marshal(req)
	}
	o, code, e := cloudHTTP(ctx, s.client, method, s.bambuBase(c["region"])+endpoint, c["token"], "application/json", body)
	if e != nil {
		return nil, e
	}
	if code/100 != 2 {
		if expiredBambuToken(code, o) {
			s.recordBambuExpiry(ctx, id, c["token"])
			return nil, bad(401, "cloud_expired", "Authentication expired")
		}
		return nil, bad(502, "cloud_failed", "Bambu Cloud returned HTTP "+strconv.Itoa(code))
	}
	return o, nil
}
func (s *Service) bambuLogin(w http.ResponseWriter, r *http.Request, p access.Principal, action string) {
	var v object
	if !httpx.Decode(w, r, &v) {
		return
	}
	ctx := r.Context()
	region := fallback(str(v["region"]), "global")
	if region != "china" && region != "global" && region != "cn" {
		httpx.Error(w, 422, "Choose a Bambu Cloud region")
		return
	}
	email := str(v["email"])
	token := strings.TrimSpace(str(v["access_token"]))
	if action == "token" {
		if token == "" || strings.IndexFunc(token, func(r rune) bool { return r < 33 || r > 126 }) >= 0 {
			httpx.Error(w, 401, "Invalid token")
			return
		}
		o, code, e := cloudHTTP(ctx, s.client, "GET", s.bambuBase(region)+"/v1/design-user-service/my/preference", token, "", nil)
		if e != nil || code/100 != 2 {
			if code == 401 || code == 403 {
				httpx.Error(w, 401, "Invalid token")
			} else {
				httpx.Error(w, 502, "Bambu Cloud could not validate the token")
			}
			return
		}
		email = fallback(str(o["email"]), "token-auth")
	} else if action == "verify" && str(v["tfa_key"]) != "" {
		var err error
		token, err = s.bambuTFA(ctx, region, str(v["tfa_key"]), str(v["code"]))
		if err != nil {
			httpx.JSON(w, 200, object{"success": false, "needs_verification": true, "verification_type": "tfa", "tfa_key": v["tfa_key"], "message": err.Error()})
			return
		}
	} else {
		input := object{"account": email}
		endpoint := s.bambuBase(region) + "/v1/user-service/user/login"
		if action == "login" {
			input["password"] = v["password"]
		} else {
			input["code"] = v["code"]
		}
		b, _ := json.Marshal(input)
		o, code, e := cloudHTTP(ctx, s.client, "POST", endpoint, "", "application/json", b)
		if e != nil || code >= 500 {
			httpx.Error(w, 502, "Bambu Cloud sign-in is unavailable")
			return
		}
		if code/100 != 2 {
			httpx.Error(w, 401, "Bambu Cloud declined the sign-in request")
			return
		}
		token = str(o["accessToken"])
		if token == "" {
			kind := str(o["loginType"])
			verify := kind == "verifyCode" || kind == "tfa" || str(o["tfaKey"]) != ""
			mode := "email"
			if kind == "tfa" || str(o["tfaKey"]) != "" {
				mode = "tfa"
			}
			httpx.JSON(w, 200, object{"success": false, "needs_verification": verify, "verification_type": mode, "tfa_key": o["tfaKey"], "message": "Complete Bambu Cloud verification"})
			return
		}
	}
	e := s.saveCreds(ctx, p.ID, map[string]string{"token": token, "email": email, "region": region, "expired": ""})
	if e != nil {
		respond(w, nil, e)
		return
	}
	if action == "token" {
		httpx.JSON(w, 200, object{"is_authenticated": true, "email": email, "region": region, "sign_in_expired": false})
		return
	}
	httpx.JSON(w, 200, object{"success": true, "needs_verification": false, "message": "Signed in"})
}
func (s *Service) cloudRoutes(api *http.ServeMux) {
	for _, endpoint := range []string{"GET cloud/status", "POST cloud/login", "POST cloud/verify", "POST cloud/token", "POST cloud/logout", "GET cloud/settings", "POST cloud/settings", "GET cloud/settings/{id}", "PUT cloud/settings/{id}", "DELETE cloud/settings/{id}", "GET cloud/devices", "GET cloud/filaments", "GET orca-cloud/status", "POST orca-cloud/device/start", "POST orca-cloud/device/poll", "POST orca-cloud/logout", "GET orca-cloud/profiles", "GET orca-cloud/profiles/{id}"} {
		m, path, _ := strings.Cut(endpoint, " ")
		api.HandleFunc(m+" /api/v1/"+path, s.cloud)
	}
}
func (s *Service) cloud(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	ctx := r.Context()
	resource := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	provider, action, _ := strings.Cut(resource, "/")
	isOrca := provider == "orca-cloud"
	perm := "cloud:auth"
	if isOrca {
		perm = "orca_cloud:auth"
	} else if action == "devices" {
		perm = "printers:read"
	} else if action == "filaments" {
		perm = "filaments:read"
	}
	if e := s.cloudAccess(ctx, p, perm); e != nil {
		respond(w, nil, e)
		return
	}
	if isOrca {
		s.orcaRoute(w, r, p, action)
		return
	}
	switch action {
	case "status":
		status, e := s.bambuStatus(ctx, p.ID)
		respond(w, status, e)
		return
	case "login", "verify", "token":
		s.bambuLogin(w, r, p, action)
		return
	case "logout":
		e := s.saveCreds(ctx, p.ID, map[string]string{"token": "", "email": "", "region": "", "expired": ""})
		respond(w, object{"success": true}, e)
		return
	case "devices":
		o, e := s.bambuRequest(ctx, p.ID, "GET", "/v1/iot-service/api/user/bind", nil)
		respond(w, list(o["devices"]), e)
		return
	}
	if r.Method == "GET" && (action == "settings" || action == "filaments") {
		data, e := s.profileList(ctx, p.ID, bambu, r.URL.Query().Get("version"))
		if action == "filaments" {
			respond(w, data["filament"], e)
		} else {
			respond(w, data, e)
		}
		return
	}
	id := r.PathValue("id")
	endpoint := "/v1/iot-service/api/slicer/setting"
	if id != "" && !validID.MatchString(id) {
		httpx.Error(w, 422, "Invalid profile ID")
		return
	}
	if id != "" {
		endpoint += "/" + id
	}
	var payload object
	if r.Method == "PUT" || r.Method == "POST" {
		if !httpx.Decode(w, r, &payload) {
			return
		}
		if r.Method == "PUT" {
			payload["setting_id"] = id
		}
	}
	// Bambu stores create and update at the collection endpoint.
	method := r.Method
	if method == "PUT" {
		method = "POST"
		endpoint = "/v1/iot-service/api/slicer/setting"
	}
	o, e := s.bambuRequest(ctx, p.ID, method, endpoint, payload)
	if r.Method == "DELETE" && e == nil {
		o = object{"success": true, "message": "Profile deleted"}
	}
	respond(w, o, e)
}
func parseStamp(v string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02 15:04:05.999999", "2006-01-02 15:04:05"} {
		if stamp, e := time.Parse(layout, v); e == nil {
			return stamp
		}
	}
	return time.Time{}
}
func profileGroups() map[string][]object {
	return map[string][]object{"printer": {}, "process": {}, "filament": {}}
}
func slot(s string) string {
	switch s {
	case "machine", "printer":
		return "printer"
	case "process", "print":
		return "process"
	case "filament":
		return s
	}
	return ""
}
func (s *Service) profileList(ctx context.Context, id int64, provider, version string) (map[string][]object, error) {
	out := profileGroups()
	if provider == orca {
		rows, e := s.orcaProfiles(ctx, id)
		if e != nil {
			return out, e
		}
		for _, v := range rows {
			c, _ := v["content"].(object)
			k := slot(str(c["type"]))
			if k == "" || v["id"] == nil {
				continue
			}
			pid := fmt.Sprint(v["id"])
			out[k] = append(out[k], object{"setting_id": pid, "name": fallback(str(v["name"]), pid), "type": k, "version": c["version"], "user_id": c["user_id"], "updated_time": v["updated_time"], "is_custom": true, "inherits": c["inherits"], "filament_type": first(c["filament_type"]), "filament_colour": first(c["default_filament_colour"])})
		}
		return out, nil
	}
	o, e := s.bambuRequest(ctx, id, "GET", "/v1/iot-service/api/slicer/setting?version="+url.QueryEscape(fallback(version, "02.00.00.00")), nil)
	if e != nil {
		return out, e
	}
	for _, key := range []string{"printer", "print", "filament"} {
		k := slot(key)
		section, _ := o[key].(object)
		for _, tier := range []string{"private", "public"} {
			for _, row := range list(section[tier]) {
				v := object{}
				for _, k := range []string{"name", "version", "user_id", "updated_time", "filament_id", "base_id", "inherits", "filament_type", "filament_colour"} {
					v[k] = row[k]
				}
				v["type"] = k
				v["setting_id"] = row["setting_id"]
				if v["setting_id"] == nil {
					v["setting_id"] = row["id"]
				}
				v["is_custom"] = tier == "private"
				out[k] = append(out[k], v)
			}
		}
	}
	return out, nil
}
func first(v any) any {
	if a, ok := v.([]any); ok {
		if len(a) > 0 {
			return a[0]
		}
		return nil
	}
	return v
}
func (s *Service) CloudProfiles(ctx context.Context, p access.Principal, source string) (map[string][]object, string) {
	perm := "cloud:auth"
	key := "token"
	if source == orca {
		perm = "orca_cloud:auth"
		key = "otoken"
	}
	if s.cloudAccess(ctx, p, perm) != nil {
		return profileGroups(), "not_authenticated"
	}
	c, e := s.readCreds(ctx, p.ID, key)
	if e != nil || c[key] == "" {
		return profileGroups(), "not_authenticated"
	}
	groups, e := s.profileList(ctx, p.ID, source, "")
	if e != nil {
		var err problem
		if errors.As(e, &err) && err.code == "cloud_expired" {
			return profileGroups(), "expired"
		}
		return profileGroups(), "unreachable"
	}
	for _, a := range groups {
		for _, v := range a {
			v["id"] = v["setting_id"]
			v["source"] = source
		}
	}
	return groups, "ok"
}
func (s *Service) ResolvePreset(ctx context.Context, p access.Principal, source, id, k string) (object, error) {
	perm := "cloud:auth"
	if source == orca {
		perm = "orca_cloud:auth"
	}
	if e := s.cloudAccess(ctx, p, perm); e != nil {
		return nil, e
	}
	if !validID.MatchString(id) {
		return nil, bad(422, "cloud_invalid_id", "Invalid cloud preset ID")
	}
	var o object
	var e error
	if source == orca {
		o, e = s.orcaProfile(ctx, p.ID, id)
	} else {
		o, e = s.bambuRequest(ctx, p.ID, "GET", "/v1/iot-service/api/slicer/setting/"+id, nil)
	}
	if e != nil {
		return nil, e
	}
	settings, _ := o["setting"].(object)
	if settings == nil {
		return nil, bad(422, "cloud_invalid_profile", "Cloud profile contains no settings")
	}
	if ty := slot(str(settings["type"])); ty != "" && ty != k {
		return nil, bad(422, "cloud_wrong_profile", "Preset type does not match the selected slot")
	}
	return settings, nil
}
func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
