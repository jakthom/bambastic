package integrations

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
)

const makerAPI = "https://api.bambulab.com/v1/design-service"

var modelURLPattern = regexp.MustCompile(`(?:^|/)models/([1-9][0-9]*)(?:[-/]|$)`)
var profileFragment = regexp.MustCompile(`^profileId[-=]([1-9][0-9]*)$`)

func makerURL(raw string) (int64, int64, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.Scheme != "https" && u.Scheme != "http" || u.User != nil || u.Port() != "" {
		return 0, 0, bad(400, "invalid_url", "Invalid MakerWorld model URL")
	}
	host := strings.ToLower(u.Hostname())
	if host != "makerworld.com" && !strings.HasSuffix(host, ".makerworld.com") {
		return 0, 0, bad(400, "invalid_url", "Choose a MakerWorld model URL")
	}
	match := modelURLPattern.FindStringSubmatch(u.Path)
	if match == nil {
		return 0, 0, bad(400, "invalid_url", "The link does not identify a model")
	}
	model, _ := strconv.ParseInt(match[1], 10, 64)
	if model <= 0 {
		return 0, 0, bad(400, "invalid_url", "Invalid model ID")
	}
	var profile int64
	if f := profileFragment.FindStringSubmatch(u.Fragment); f != nil {
		profile, _ = strconv.ParseInt(f[1], 10, 64)
	}
	return model, profile, nil
}
func modelURL(model, profile int64) string {
	base := fmt.Sprintf("https://makerworld.com/models/%d", model)
	if profile > 0 {
		base += fmt.Sprintf("#profileId-%d", profile)
	}
	return base
}
func (s *Service) makerRoutes(m *http.ServeMux) {
	for _, a := range []string{"GET status", "POST resolve", "POST import", "GET recent-imports", "GET thumbnail"} {
		method, p, _ := strings.Cut(a, " ")
		m.HandleFunc(method+" /api/v1/makerworld/"+p, s.maker)
	}
}
func (s *Service) makerGet(ctx context.Context, addr, token string) (object, error) {
	v, code, e := cloudHTTP(ctx, s.client, "GET", addr, token, "", nil)
	if e != nil {
		return nil, e
	}
	if code/100 != 2 {
		switch code {
		case 401, 403:
			return nil, bad(403, "auth_required", "The MakerWorld account cannot access this model")
		case 404:
			return nil, bad(404, "not_found", "MakerWorld model or profile not found")
		case 429:
			return nil, bad(429, "rate_limited", "Try MakerWorld again later")
		default:
			return nil, bad(502, "upstream_unavailable", "MakerWorld is unavailable")
		}
	}
	return v, nil
}
func (s *Service) makerRefs(ctx context.Context, p access.Principal, model int64, limit int) ([]object, error) {
	q := "SELECT id AS library_file_id,filename,folder_id,thumbnail_path,source_url,created_at FROM library_files WHERE source_type='makerworld' AND deleted_at IS NULL"
	args := []any{}
	if !p.Has("library:read_all") {
		if p.ID == 0 || !p.Has("library:read_own") {
			return []object{}, nil
		}
		q += " AND created_by_id=?"
		args = append(args, p.ID)
	}
	if model > 0 {
		q += " AND (source_url=? OR source_url LIKE ?)"
		args = append(args, modelURL(model, 0), modelURL(model, 0)+"#profileId-%")
	}
	q += " ORDER BY created_at DESC,id DESC LIMIT ?"
	args = append(args, limit)
	rows, e := s.db.QueryContext(ctx, q, args...)
	if e != nil {
		return nil, e
	}
	return httpx.Rows(rows)
}
func safeCDN(address string, signed bool) (*url.URL, bool) {
	u, e := url.Parse(address)
	if e != nil || u.User != nil || u.Scheme != "https" || u.Host == "" || u.Port() != "" {
		return nil, false
	}
	host := strings.ToLower(u.Hostname())
	return u, host == "makerworld.bblmw.com" || host == "public-cdn.bblmw.com" || signed && strings.HasSuffix(host, ".amazonaws.com")
}
func (s *Service) assetResponse(ctx context.Context, addr string, signed bool) (*http.Response, error) {
	if _, ok := safeCDN(addr, signed); !ok {
		return nil, bad(400, "invalid_url", "Unrecognized MakerWorld asset host")
	}
	client := *s.client
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if _, ok := safeCDN(r.URL.String(), signed); !ok || len(via) > 4 {
			return fmt.Errorf("invalid MakerWorld asset redirect")
		}
		return nil
	}
	r, e := http.NewRequestWithContext(ctx, "GET", addr, nil)
	if e != nil {
		return nil, bad(400, "invalid_url", "Invalid asset URL")
	}
	r.Header.Set("User-Agent", "Bambastic/0.1")
	res, e := client.Do(r)
	if e != nil {
		return nil, bad(502, "download_failed", "Could not retrieve the MakerWorld asset")
	}
	if res.StatusCode/100 != 2 {
		res.Body.Close()
		return nil, bad(502, "download_failed", "MakerWorld asset request failed")
	}
	return res, nil
}
func (s *Service) maker(w http.ResponseWriter, r *http.Request) {
	action := path.Base(r.URL.Path)
	ctx := r.Context()
	p := access.From(ctx)
	perm := "makerworld:view"
	if action == "import" {
		perm = "makerworld:import"
	}
	if !access.Require(w, r, perm) {
		return
	}
	if action == "recent-imports" {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 {
			limit = 20
		}
		v, e := s.makerRefs(ctx, p, 0, min(limit, 100))
		respond(w, v, e)
		return
	}
	if action == "thumbnail" {
		res, e := s.assetResponse(ctx, r.URL.Query().Get("url"), false)
		if e != nil {
			respond(w, nil, e)
			return
		}
		defer res.Body.Close()
		b, e := io.ReadAll(io.LimitReader(res.Body, 8<<20+1))
		mt := http.DetectContentType(b)
		if e != nil || len(b) > 8<<20 || mt != "image/jpeg" && mt != "image/png" && mt != "image/webp" {
			respond(w, nil, bad(502, "download_failed", "Invalid thumbnail image"))
			return
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Write(b)
		return
	}
	token := ""
	invalid := false
	// A shared API key can browse public metadata. Private credentials are confined to explicitly opted-in keys.
	if e := s.cloudAccess(ctx, p, perm); e == nil {
		c, err := s.readCreds(ctx, p.ID, "token", "expired")
		if err != nil {
			respond(w, nil, err)
			return
		}
		token = c["token"]
		invalid = c["expired"] != ""
	}
	if action == "status" {
		httpx.JSON(w, 200, object{"has_cloud_token": token != "", "can_download": token != "" && !invalid, "sign_in_expired": token != "" && invalid})
		return
	}
	var req struct {
		URL      string `json:"url"`
		Model    int64  `json:"model_id"`
		Profile  int64  `json:"profile_id"`
		Instance int64  `json:"instance_id"`
		Folder   int64  `json:"folder_id"`
		Source   string `json:"source_type"`
	}
	if !httpx.Decode(w, r, &req) {
		return
	}
	if action == "resolve" {
		var e error
		req.Model, req.Profile, e = makerURL(req.URL)
		if e != nil {
			respond(w, nil, e)
			return
		}
	} else if req.Model <= 0 || req.Source != "" && req.Source != "makerworld" {
		httpx.Error(w, 422, "Invalid MakerWorld model")
		return
	}
	base := fmt.Sprintf("%s/design/%d", makerAPI, req.Model)
	design, e := s.makerGet(ctx, base, "")
	if e != nil {
		respond(w, nil, e)
		return
	}
	instances, e := s.makerGet(ctx, base+"/instances", "")
	if e != nil {
		respond(w, nil, e)
		return
	}
	hits := list(instances["hits"])
	local := list(design["instances"])
	byID := map[int64]object{}
	for _, v := range local {
		byID[intVal(v["id"])] = v
	}
	if action == "resolve" {
		for _, h := range hits {
			v := byID[intVal(h["id"])]
			ext, _ := v["extention"].(object)
			info, _ := ext["modelInfo"].(object)
			for _, k := range []string{"compatibility", "otherCompatibility"} {
				if info[k] != nil {
					h[k] = info[k]
				}
			}
		}
		refs, e := s.makerRefs(ctx, p, req.Model, 50000)
		ids := []any{}
		for _, q := range refs {
			ids = append(ids, q["library_file_id"])
		}
		var profile any
		if req.Profile > 0 {
			profile = req.Profile
		}
		respond(w, object{"model_id": req.Model, "profile_id": profile, "design": design, "instances": hits, "already_imported_library_ids": ids}, e)
		return
	}
	if token == "" || invalid {
		respond(w, nil, bad(403, "auth_required", "Connect Bambu Cloud to download a MakerWorld profile"))
		return
	}
	var instance, profile int64
	for _, v := range append(local, hits...) {
		iid, pid := intVal(v["id"]), intVal(v["profileId"])
		if pid <= 0 || iid <= 0 {
			continue
		}
		if req.Profile > 0 && pid != req.Profile || req.Instance > 0 && iid != req.Instance {
			continue
		}
		instance, profile = iid, pid
		break
	}
	if profile == 0 {
		respond(w, nil, bad(404, "not_found", "No matching print profile on this model"))
		return
	}
	src := modelURL(req.Model, profile)
	ref, e := readRef(ctx, s.db, p, src)
	if e != nil {
		respond(w, nil, e)
		return
	}
	if ref != nil {
		httpx.JSON(w, 200, object{"library_file_id": ref["id"], "filename": ref["filename"], "folder_id": ref["folder_id"], "profile_id": profile, "was_existing": true})
		return
	}
	manifest, e := s.makerGet(ctx, fmt.Sprintf("%s/instance/%d/f3mf?type=download", makerAPI, instance), token)
	if e != nil {
		respond(w, nil, e)
		return
	}
	if str(manifest["url"]) == "" && str(design["modelId"]) != "" {
		manifest, e = s.makerGet(ctx, fmt.Sprintf("https://api.bambulab.com/v1/iot-service/api/user/profile/%d?model_id=%s", profile, url.QueryEscape(str(design["modelId"]))), token)
		if e != nil {
			respond(w, nil, e)
			return
		}
	}
	res, e := s.assetResponse(ctx, str(manifest["url"]), true)
	if e != nil {
		respond(w, nil, e)
		return
	}
	defer res.Body.Close()
	if res.ContentLength > 2<<30 {
		httpx.Error(w, 413, "Model exceeds 2 GiB")
		return
	}
	name := path.Base(strings.ReplaceAll(fallback(str(manifest["name"]), fmt.Sprintf("model-%d.3mf", req.Model)), "\\", "/"))
	if !strings.HasSuffix(strings.ToLower(name), ".3mf") {
		name += ".3mf"
	}
	id, reused, e := s.farm.ImportLibrary(ctx, p, http.MaxBytesReader(nil, res.Body, 2<<30), name, req.Folder, "makerworld", src)
	if e != nil {
		respond(w, nil, bad(422, "import_failed", "Could not add the model to the library"))
		return
	}
	var folder sql.NullInt64
	_ = s.db.QueryRowContext(ctx, "SELECT folder_id FROM library_files WHERE id=?", id).Scan(&folder)
	var dest any
	if folder.Valid {
		dest = folder.Int64
	}
	httpx.JSON(w, 200, object{"library_file_id": id, "filename": name, "folder_id": dest, "profile_id": profile, "was_existing": reused})
}
