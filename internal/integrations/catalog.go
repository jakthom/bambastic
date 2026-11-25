package integrations

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/httpx"
)

//go:embed data/*
var catalogData embed.FS

func (s *Service) bambuTFA(ctx context.Context, region, key, code string) (string, error) {
	web := "https://bambulab.com"
	if region == "cn" || region == "china" {
		web = "https://bambulab.cn"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	jar, _ := cookiejar.New(nil)
	client := *s.client
	client.Jar = jar
	r, _ := http.NewRequestWithContext(ctx, "GET", web+"/api/csrf", nil)
	r.Header.Set("User-Agent", "Bambastic/0.1")
	res, e := client.Do(r)
	if e != nil {
		return "", errors.New("Cannot reach the Bambu sign-in service")
	}
	res.Body.Close()
	u, _ := url.Parse(web)
	csrf := ""
	for _, c := range jar.Cookies(u) {
		if c.Name == "bbl_csrf_token" {
			csrf = c.Value
		}
	}
	if csrf == "" {
		return "", errors.New("Bambu Cloud did not provide a sign-in security token")
	}
	body, _ := json.Marshal(object{"tfaKey": key, "tfaCode": code})
	r, _ = http.NewRequestWithContext(ctx, "POST", web+"/api/sign-in/tfa", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", "Bambastic/0.1")
	r.Header.Set("x-bbl-csrf-token", csrf)
	res, e = client.Do(r)
	if e != nil {
		return "", errors.New("Cannot reach the Bambu verification service")
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		return "", errors.New("Bambu Cloud declined verification")
	}
	var v object
	_ = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v)
	if tok := fallback(str(v["accessToken"]), str(v["token"])); tok != "" {
		return tok, nil
	}
	for _, c := range res.Cookies() {
		name := strings.ToLower(c.Name)
		if strings.Contains(name, "token") && !strings.Contains(name, "csrf") && c.Value != "" {
			return c.Value, nil
		}
	}
	return "", errors.New("Bambu Cloud did not issue an account token")
}
func (s *Service) catalogRoutes(m *http.ServeMux) {
	m.HandleFunc("POST /api/v1/cloud/filament-info", s.filamentInfo)
	for _, k := range []string{"builtin-filaments", "filament-id-map", "fields", "fields/{kind}", "firmware-updates"} {
		m.HandleFunc("GET /api/v1/cloud/"+k, s.catalog)
	}
}
func readCatalog(file string) (object, error) {
	b, e := catalogData.ReadFile("data/" + file + ".json")
	if e != nil {
		return nil, e
	}
	var o object
	e = json.Unmarshal(b, &o)
	return o, e
}
func (s *Service) fields(ctx context.Context, kind string) (object, error) {
	if kind == "print" {
		kind = "process"
	}
	switch kind {
	case "process", "filament", "printer":
	default:
		return nil, bad(400, "invalid_profile_type", "Unknown profile type")
	}
	o, e := readCatalog(kind + "_fields")
	if e != nil || kind != "filament" {
		return o, e
	}
	base, e := readCatalog("slicer_filament_types")
	if e != nil {
		return nil, e
	}
	engine, e := access.Setting(ctx, s.db, "preferred_slicer", "bambu_studio")
	if e != nil {
		return nil, e
	}
	types, _ := base[engine].(object)
	if options, ok := types["values"].([]any); ok {
		for _, field := range list(o["fields"]) {
			if str(field["key"]) == "filament_type" {
				field["options"] = options
			}
		}
	}
	return o, nil
}
func (s *Service) catalog(w http.ResponseWriter, r *http.Request) {
	p := access.From(r.Context())
	ctx := r.Context()
	base := path.Base(r.URL.Path)
	if strings.Contains(r.URL.Path, "/fields") {
		if !access.Require(w, r, "cloud:auth") {
			return
		}
		if kind := r.PathValue("kind"); kind != "" {
			v, e := s.fields(ctx, kind)
			respond(w, v, e)
			return
		}
		v := object{}
		for _, k := range []string{"filament", "process", "printer"} {
			f, e := s.fields(ctx, k)
			if e != nil {
				respond(w, nil, e)
				return
			}
			v[k] = f
		}
		httpx.JSON(w, 200, v)
		return
	}
	if base == "firmware-updates" {
		if e := s.cloudAccess(ctx, p, "firmware:read"); e != nil {
			respond(w, nil, e)
			return
		}
		devices, e := s.bambuRequest(ctx, p.ID, "GET", "/v1/iot-service/api/user/bind", nil)
		if e != nil {
			respond(w, nil, e)
			return
		}
		out := []object{}
		available := 0
		for _, d := range list(devices["devices"]) {
			did := str(d["dev_id"])
			o, e := s.bambuRequest(ctx, p.ID, "GET", "/v1/iot-service/api/user/device/version?device_id="+url.QueryEscape(did), nil)
			if e != nil {
				continue
			}
			if nested, ok := o["data"].(object); ok {
				o = nested
			}
			if httpx.Bool(o["update_available"]) {
				available++
			}
			out = append(out, object{"device_id": did, "device_name": d["name"], "current_version": o["current_version"], "latest_version": o["latest_version"], "update_available": o["update_available"], "release_notes": o["release_notes"]})
		}
		httpx.JSON(w, 200, object{"updates": out, "updates_available": available})
		return
	}
	if !access.Require(w, r, "filaments:read") {
		return
	}
	builtin, e := readCatalog("builtin")
	if e != nil {
		respond(w, nil, e)
		return
	}
	if base == "builtin-filaments" {
		a := []object{}
		for id, name := range builtin {
			a = append(a, object{"filament_id": id, "name": name})
		}
		sort.Slice(a, func(i, j int) bool { return str(a[i]["name"]) < str(a[j]["name"]) })
		httpx.JSON(w, 200, a)
		return
	}
	if e = s.cloudAccess(ctx, p, "filaments:read"); e == nil {
		if lists, e := s.profileList(ctx, p.ID, bambu, ""); e == nil {
			for _, f := range lists["filament"] {
				if key := str(f["filament_id"]); key != "" {
					builtin[key] = f["name"]
				}
			}
		}
	}
	httpx.JSON(w, 200, builtin)
}

func (s *Service) filamentInfo(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "filaments:read") {
		return
	}
	var ids []string
	if !httpx.Decode(w, r, &ids) {
		return
	}
	if len(ids) > 128 {
		httpx.Error(w, 422, "Too many filament identifiers")
		return
	}
	ctx := r.Context()
	p := access.From(ctx)
	builtin, e := readCatalog("builtin")
	if e != nil {
		respond(w, nil, e)
		return
	}
	cloud := s.cloudAccess(ctx, p, "filaments:read") == nil
	if cloud {
		c, e := s.readCreds(ctx, p.ID, "token")
		cloud = e == nil && c["token"] != ""
	}
	out := object{}
	for _, id := range ids {
		if _, seen := out[id]; seen {
			continue
		}
		entry := object{"name": fallback(str(builtin[id]), ""), "k": nil}
		if cloud && validID.MatchString(id) {
			o, e := s.bambuRequest(ctx, p.ID, "GET", "/v1/iot-service/api/slicer/setting/"+id, nil)
			if e == nil {
				values, _ := o["setting"].(object)
				entry["name"] = fallback(str(o["name"]), str(values["name"]))
				if k, e := strconv.ParseFloat(str(first(values["pressure_advance"])), 64); e == nil {
					entry["k"] = k
				}
			}
		}
		if entry["name"] == "" || entry["k"] == nil {
			var name, advance string
			if e := s.db.QueryRowContext(ctx, "SELECT name,COALESCE(pressure_advance,'') FROM local_presets WHERE preset_type='filament' AND (id=? OR name=? OR CASE WHEN json_valid(setting) THEN json_extract(setting,'$.filament_id') END=?) ORDER BY id LIMIT 1", id, id, id).Scan(&name, &advance); e == nil {
				if entry["name"] == "" {
					entry["name"] = name
				}
				if k, e := strconv.ParseFloat(advance, 64); e == nil && entry["k"] == nil {
					entry["k"] = k
				}
			}
		}
		out[id] = entry
	}
	httpx.JSON(w, 200, out)
}
