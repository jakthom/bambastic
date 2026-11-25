package slicing

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const orcaBase = "https://raw.githubusercontent.com/SoftFever/OrcaSlicer/main/resources/profiles/BBL"

func (s *Service) baseProfile(ctx context.Context, name, kind string, force bool) (object, error) {
	if len(name) > 300 || name == "" || strings.ContainsAny(name, "/\\\n\r\x00") || name == "." || name == ".." {
		return nil, errors.New("Invalid base profile name")
	}
	var raw string
	var fresh bool
	if !force {
		if e := s.db.QueryRowContext(ctx, "SELECT setting,unixepoch(fetched_at)>unixepoch('now','-7 days') FROM orca_base_profiles WHERE name=?", name).Scan(&raw, &fresh); e == nil && fresh {
			var cached object
			if json.Unmarshal([]byte(raw), &cached) == nil {
				return cached, nil
			}
		}
	}
	dir := map[string]string{"printer": "machine", "machine": "machine", "process": "process", "filament": "filament"}[kind]
	if dir == "" {
		return nil, errors.New("Unknown preset type")
	}
	dirs := []string{dir}
	if dir != "filament" {
		dirs = append(dirs, "filament")
	}
	c, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	for _, d := range dirs {
		var profile object
		code, e := s.jsonCall(c, "GET", orcaBase+"/"+d+"/"+url.PathEscape(name)+".json", nil, &profile)
		if e != nil {
			if code == 404 {
				continue
			}
			break
		}
		if profile == nil {
			break
		}
		b, _ := json.Marshal(profile)
		_, e = s.db.ExecContext(ctx, "INSERT INTO orca_base_profiles(name,profile_type,setting,fetched_at) VALUES(?,?,?,CURRENT_TIMESTAMP) ON CONFLICT(name) DO UPDATE SET profile_type=excluded.profile_type,setting=excluded.setting,fetched_at=CURRENT_TIMESTAMP", name, kind, string(b))
		return profile, e
	}
	if !force && raw != "" {
		var existing object
		if json.Unmarshal([]byte(raw), &existing) == nil {
			return existing, nil
		}
	}
	return nil, errors.New("Base profile unavailable")
}
func (s *Service) inheritBase(ctx context.Context, child object, kind string, seen map[string]bool) object {
	ref := asText(child["inherits"])
	if ref == "" || seen[ref] || len(seen) >= 10 {
		return child
	}
	seen[ref] = true
	base, e := s.baseProfile(ctx, ref, kind, false)
	if e != nil {
		return child
	}
	out := s.inheritBase(ctx, base, kind, seen)
	for k, v := range child {
		out[k] = v
	}
	return out
}
func (s *Service) cacheHTTP(w http.ResponseWriter, r *http.Request) {
	read := r.Method == "GET"
	perm := "settings:update"
	if read {
		perm = "settings:read"
	}
	if !access.Require(w, r, perm) {
		return
	}
	ctx := r.Context()
	if read {
		var n, fresh int
		if e := s.db.QueryRowContext(ctx, "SELECT count(*),coalesce(sum(unixepoch(fetched_at)>=unixepoch('now','-7 days')),0) FROM orca_base_profiles").Scan(&n, &fresh); e != nil {
			presetError(w, e)
			return
		}
		httpx.JSON(w, 200, object{"total": n, "fresh": fresh, "stale": n - fresh, "ttl_days": 7})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/refresh") {
		rs, e := rows(s.db, ctx, "SELECT name,profile_type FROM orca_base_profiles ORDER BY name")
		if e != nil {
			presetError(w, e)
			return
		}
		ok := 0
		for _, v := range rs {
			if _, e = s.baseProfile(ctx, asText(v["name"]), asText(v["profile_type"]), true); e == nil {
				ok++
			}
			if ctx.Err() != nil {
				return
			}
		}
		httpx.JSON(w, 200, object{"total": len(rs), "refreshed": ok, "failed": len(rs) - ok})
		return
	}
	presets, e := rows(s.db, ctx, "SELECT id,preset_type,setting FROM local_presets")
	if e != nil {
		presetError(w, e)
		return
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		presetError(w, e)
		return
	}
	defer tx.Rollback()
	updates := int64(0)
	for _, v := range presets {
		var profile object
		if json.Unmarshal([]byte(asText(v["setting"])), &profile) != nil {
			continue
		}
		kind := classify(profile, "")
		if kind != "" && kind != asText(v["preset_type"]) {
			res, e := tx.ExecContext(ctx, "UPDATE local_presets SET preset_type=?,updated_at=CURRENT_TIMESTAMP WHERE id=? AND setting=? AND preset_type=?", kind, v["id"], v["setting"], v["preset_type"])
			if e != nil {
				presetError(w, e)
				return
			}
			n, _ := res.RowsAffected()
			updates += n
		}
	}
	if e = tx.Commit(); e != nil {
		presetError(w, e)
		return
	}
	httpx.JSON(w, 200, object{"total": len(presets), "reclassified": updates})
}
