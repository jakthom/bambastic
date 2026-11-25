package integrations

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func orcaBase() string {
	return strings.TrimRight(env("ORCA_CLOUD_API_BASE", "https://api.orcaslicer.com"), "/")
}
func clientID() string { return env("ORCA_CLOUD_CLIENT_ID", "oc_app_e873d49ce7dbcc7dca8ba386") }
func (s *Service) orcaForm(ctx context.Context, path string, values url.Values) (object, int, error) {
	values.Set("client_id", clientID())
	return cloudHTTP(ctx, s.client, "POST", orcaBase()+path, "", "application/x-www-form-urlencoded", []byte(values.Encode()))
}
func (s *Service) orcaRoute(w http.ResponseWriter, r *http.Request, p access.Principal, action string) {
	ctx := r.Context()
	switch action {
	case "status":
		c, e := s.readCreds(ctx, p.ID, "otoken", "oemail", "ouser")
		respond(w, object{"connected": c["otoken"] != "", "email": c["oemail"], "user_id": c["ouser"]}, e)
		return
	case "logout":
		s.cloudMu.Lock()
		defer s.cloudMu.Unlock()
		e := s.saveCreds(ctx, p.ID, map[string]string{"otoken": "", "refresh": "", "expires": "", "oemail": "", "ouser": "", "pending": "", "interval": "", "started": ""})
		respond(w, object{"connected": false}, e)
		return
	case "device/start":
		s.cloudMu.Lock()
		defer s.cloudMu.Unlock()
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		v, code, e := s.orcaForm(ctx, "/oauth/device/code", url.Values{"scope": {"sync:read"}, "instance_label": {"Bambastic"}, "instance_url": {scheme + "://" + r.Host}})
		if e != nil || code/100 != 2 {
			if e == nil {
				e = cloudFailure(code)
			}
			respond(w, nil, e)
			return
		}
		if str(v["device_code"]) == "" || str(v["user_code"]) == "" {
			respond(w, nil, bad(502, "orca_invalid_response", "Orca Cloud returned an incomplete pairing response"))
			return
		}
		interval := intVal(v["interval"])
		if interval < 1 {
			interval = 5
		}
		seconds := intVal(v["expires_in"])
		if seconds < 1 {
			seconds = 600
		}
		e = s.saveCreds(ctx, p.ID, map[string]string{"pending": str(v["device_code"]), "interval": strconv.FormatInt(interval, 10), "started": time.Now().UTC().Format(time.RFC3339)})
		respond(w, object{"user_code": v["user_code"], "verification_uri": v["verification_uri"], "verification_uri_complete": v["verification_uri_complete"], "interval": interval, "expires_in": seconds}, e)
		return
	case "device/poll":
		s.cloudMu.Lock()
		defer s.cloudMu.Unlock()
		c, e := s.readCreds(ctx, p.ID, "pending", "started")
		if e != nil {
			respond(w, nil, e)
			return
		}
		if c["pending"] == "" || time.Since(parseStamp(c["started"])) > 15*time.Minute {
			httpx.JSON(w, 200, object{"status": "expired_token", "connected": false})
			return
		}
		v, code, e := s.orcaForm(ctx, "/oauth/token", url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}, "device_code": {c["pending"]}})
		if e != nil {
			respond(w, nil, e)
			return
		}
		if code/100 != 2 {
			status := str(v["error"])
			switch status {
			case "authorization_pending", "slow_down":
			case "expired_token", "invalid_grant", "access_denied":
				if status == "invalid_grant" {
					status = "expired_token"
				}
				_ = s.saveCreds(ctx, p.ID, map[string]string{"pending": "", "started": "", "interval": ""})
			default:
				respond(w, nil, cloudFailure(code))
				return
			}
			httpx.JSON(w, 200, object{"status": status, "connected": false})
			return
		}
		if str(v["access_token"]) == "" || str(v["refresh_token"]) == "" {
			respond(w, nil, bad(502, "orca_invalid_response", "Orca Cloud returned an incomplete token pair"))
			return
		}
		who, _, _ := cloudHTTP(ctx, s.client, "GET", orcaBase()+"/api/v1/external-apps/me", str(v["access_token"]), "", nil)
		pair := orcaPair(v)
		pair["ouser"] = str(who["user_id"])
		pair["oemail"] = str(who["email"])
		pair["pending"] = ""
		pair["interval"] = ""
		pair["started"] = ""
		e = s.saveCreds(ctx, p.ID, pair)
		respond(w, object{"status": "complete", "connected": true, "user_id": who["user_id"], "email": who["email"]}, e)
		return
	case "profiles":
		m, e := s.profileList(ctx, p.ID, orca, "")
		respond(w, m, e)
		return
	}
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		httpx.Error(w, 422, "Invalid Orca profile ID")
		return
	}
	o, e := s.orcaProfile(ctx, p.ID, id)
	respond(w, o, e)
}
func orcaPair(o object) map[string]string {
	secs := intVal(o["expires_in"])
	if secs <= 0 {
		secs = 3600
	}
	return map[string]string{"otoken": str(o["access_token"]), "refresh": str(o["refresh_token"]), "expires": time.Now().UTC().Add(time.Duration(secs) * time.Second).Format(time.RFC3339)}
}
func (s *Service) orcaToken(ctx context.Context, id int64, expiredToken string) (string, error) {
	s.cloudMu.Lock()
	defer s.cloudMu.Unlock()
	c, e := s.readCreds(ctx, id, "otoken", "refresh", "expires")
	if e != nil {
		return "", e
	}
	if c["otoken"] == "" {
		return "", bad(409, "cloud_not_authenticated", "Connect the Orca Cloud account")
	}
	expires := parseStamp(c["expires"])
	if c["otoken"] != expiredToken && (expires.IsZero() || time.Until(expires) > 60*time.Second) {
		return c["otoken"], nil
	}
	if c["refresh"] == "" {
		return "", bad(409, "cloud_expired", "Pair with Orca Cloud again")
	}
	o, code, e := s.orcaForm(ctx, "/oauth/token", url.Values{"grant_type": {"refresh_token"}, "refresh_token": {c["refresh"]}})
	if e != nil {
		return "", e
	}
	if code/100 != 2 {
		return "", cloudFailure(code)
	}
	if str(o["access_token"]) == "" || str(o["refresh_token"]) == "" {
		return "", bad(502, "orca_invalid_response", "Incomplete Orca refresh response")
	}
	if e = s.saveCreds(ctx, id, orcaPair(o)); e != nil {
		return "", e
	}
	return str(o["access_token"]), nil
}
func (s *Service) orcaProfiles(ctx context.Context, id int64) ([]object, error) {
	token, e := s.orcaToken(ctx, id, "")
	if e != nil {
		return nil, e
	}
	for n := 0; n < 2; n++ {
		o, code, e := cloudHTTP(ctx, s.client, "GET", orcaBase()+"/api/v1/external/sync/pull", token, "", nil)
		if e != nil {
			return nil, e
		}
		if code == 401 && n == 0 {
			token, e = s.orcaToken(ctx, id, token)
			if e != nil {
				return nil, e
			}
			continue
		}
		if code/100 != 2 {
			return nil, cloudFailure(code)
		}
		for _, key := range []string{"upserts", "profiles", "data", "items"} {
			if _, ok := o[key].([]any); ok {
				return list(o[key]), nil
			}
		}
		return []object{}, nil
	}
	return nil, bad(409, "cloud_expired", "Connect Orca Cloud again")
}
func (s *Service) orcaProfile(ctx context.Context, id int64, profileID string) (object, error) {
	a, e := s.orcaProfiles(ctx, id)
	if e != nil {
		return nil, e
	}
	for _, v := range a {
		if str(v["id"]) == profileID {
			c, _ := v["content"].(object)
			if c == nil {
				break
			}
			return object{"setting_id": profileID, "name": v["name"], "type": slot(str(c["type"])), "version": c["version"], "base_id": c["inherits"], "update_time": v["updated_time"], "setting": c}, nil
		}
	}
	return nil, bad(404, "cloud_not_found", "Orca profile not found")
}
