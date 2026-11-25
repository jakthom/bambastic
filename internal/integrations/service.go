// Package integrations connects external catalogs and cloud profiles to the local library.
package integrations

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"golang.org/x/sync/singleflight"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type object = map[string]any
type Library interface {
	ImportLibrary(context.Context, access.Principal, io.Reader, string, int64, string, string) (int64, bool, error)
}
type Service struct {
	cloudMu       sync.Mutex
	bambuMu       sync.Mutex
	bambuChecks   singleflight.Group
	bambuVerified map[[32]byte]time.Time
	db            *sql.DB
	dir           string
	auth          *access.Service
	farm          Library
	client        *http.Client
	mu            sync.Mutex
	manyToken     string
	manyConfig    config
	manyExpiry    time.Time
}
type config struct {
	URL    string `json:"url"`
	Client string `json:"client_id"`
	Secret string `json:"client_secret"`
}
type problem struct {
	status    int
	code, msg string
}

func (p problem) Error() string              { return p.msg }
func bad(status int, code, msg string) error { return problem{status, code, msg} }
func respond(w http.ResponseWriter, v any, e error) {
	if e == nil {
		httpx.JSON(w, 200, v)
		return
	}
	var p problem
	if errors.As(e, &p) {
		httpx.JSON(w, p.status, object{"detail": object{"code": p.code, "message": p.msg}})
		return
	}
	httpx.Error(w, 500, "Integration storage unavailable")
}
func New(db *sql.DB, dir string, auth *access.Service, farm Library) (*Service, error) {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = serviceDial
	client := &http.Client{Transport: transport, Timeout: 5 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 4 {
			return errors.New("too many provider redirects")
		}
		if len(via) > 0 && (req.URL.Scheme != via[0].URL.Scheme || !strings.EqualFold(req.URL.Host, via[0].URL.Host)) {
			return errors.New("provider redirect changes origin")
		}
		return nil
	}}
	return &Service{bambuVerified: map[[32]byte]time.Time{}, db: db, dir: dir, auth: auth, farm: farm, client: client}, nil
}
func serviceDial(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, e := net.SplitHostPort(address)
	if e != nil {
		return nil, e
	}
	ips, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if e != nil {
		return nil, e
	}
	dial := net.Dialer{Timeout: 10 * time.Second}
	var last error
	for _, ip := range ips {
		ip = ip.Unmap()
		if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip == netip.MustParseAddr("100.100.100.200") || ip == netip.MustParseAddr("fd00:ec2::254") {
			continue
		}
		c, e := dial.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if e == nil {
			return c, nil
		}
		last = e
	}
	if last == nil {
		last = errors.New("invalid service address")
	}
	return nil, last
}
func (s *Service) Register(api *http.ServeMux) {
	s.cloudRoutes(api)
	s.catalogRoutes(api)
	s.makerRoutes(api)
	base := "/api/v1/manyfold/"
	for _, p := range []string{"GET config", "PUT config", "DELETE config", "POST config/test", "GET status", "GET models", "GET models/{id}", "GET models/{id}/preview", "POST import"} {
		m, k, _ := strings.Cut(p, " ")
		api.HandleFunc(m+" "+base+k, s.manyfold)
	}
}
func (s *Service) conf(ctx context.Context) (config, error) {
	var c config
	var e error
	for _, v := range []struct {
		k   string
		dst *string
	}{{"manyfold_url", &c.URL}, {"manyfold_client_id", &c.Client}, {"manyfold_client_secret", &c.Secret}} {
		*v.dst, e = access.Setting(ctx, s.db, v.k, "")
		if e != nil {
			return c, e
		}
	}
	c.Secret, e = s.auth.OpenSecret(c.Secret)
	return c, e
}
func (c config) view() object {
	return object{"url": c.URL, "client_id": c.Client, "has_client_secret": c.Secret != "", "configured": c.URL != "" && c.Client != "" && c.Secret != ""}
}
func validated(c config) (config, error) {
	u, e := url.Parse(strings.TrimSpace(c.URL))
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Scheme != "http" && u.Scheme != "https" {
		return c, bad(400, "manyfold_bad_url", "Set a valid HTTP or HTTPS server address")
	}
	c.URL = strings.TrimRight(u.String(), "/")
	c.Client = strings.TrimSpace(c.Client)
	if c.Client == "" || len(c.Client) > 200 {
		return c, bad(422, "manyfold_credentials", "Supply a valid client ID")
	}
	if c.Secret == "" {
		return c, bad(400, "manyfold_secret_required", "Enter the client secret")
	}
	return c, nil
}
func (s *Service) manyAuth(ctx context.Context, c config, old string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.manyConfig == c && s.manyToken != "" && s.manyToken != old && time.Now().Before(s.manyExpiry) {
		return s.manyToken, nil
	}
	form := url.Values{"client_id": {c.Client}, "client_secret": {c.Secret}, "grant_type": {"client_credentials"}, "scope": {"public read"}}
	r, e := http.NewRequestWithContext(ctx, "POST", c.URL+"/oauth/token", strings.NewReader(form.Encode()))
	if e != nil {
		return "", bad(400, "manyfold_bad_url", "Invalid Manyfold URL")
	}
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.Header.Set("Accept", "application/json")
	res, e := s.client.Do(r)
	if e != nil {
		return "", bad(502, "manyfold_unreachable", "Cannot reach Manyfold")
	}
	defer res.Body.Close()
	if res.StatusCode == 429 {
		return "", bad(429, "manyfold_rate_limited", "Manyfold is limiting sign-ins")
	}
	if res.StatusCode/100 != 2 {
		return "", bad(502, "manyfold_credentials", "Manyfold refused the client credentials")
	}
	var data struct {
		Token   string `json:"access_token"`
		Expires int    `json:"expires_in"`
	}
	if json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&data) != nil || data.Token == "" {
		return "", bad(502, "manyfold_credentials", "Manyfold returned an invalid token")
	}
	if data.Expires <= 0 {
		data.Expires = 3600
	}
	s.manyConfig = c
	s.manyToken = data.Token
	s.manyExpiry = time.Now().Add(time.Duration(max(1, data.Expires-30)) * time.Second)
	return data.Token, nil
}
func (s *Service) manyGet(ctx context.Context, c config, endpoint, accept string) (*http.Response, error) {
	token, e := s.manyAuth(ctx, c, "")
	if e != nil {
		return nil, e
	}
	for attempt := 0; attempt < 2; attempt++ {
		r, e := http.NewRequestWithContext(ctx, "GET", c.URL+endpoint, nil)
		if e != nil {
			return nil, bad(422, "manyfold_bad_url", "Invalid model URL")
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Accept", accept)
		client := s.client
		if accept != "application/vnd.manyfold.v0+json" {
			copyClient := *s.client
			copyClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
				if len(via) > 4 || req.URL.Scheme != "http" && req.URL.Scheme != "https" || req.URL.User != nil {
					return errors.New("invalid provider redirect")
				}
				if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
					req.Header.Del("Authorization")
					req.Header.Del("Cookie")
				}
				return nil
			}
			client = &copyClient
		}
		res, e := client.Do(r)
		if e != nil {
			return nil, bad(502, "manyfold_unreachable", "Manyfold is unavailable")
		}
		if res.StatusCode/100 == 2 {
			return res, nil
		}
		res.Body.Close()
		switch res.StatusCode {
		case 401:
			if attempt == 0 {
				token, e = s.manyAuth(ctx, c, token)
				if e != nil {
					return nil, e
				}
				continue
			}
			return nil, bad(502, "manyfold_credentials", "Manyfold rejected the access token")
		case 403:
			return nil, bad(502, "manyfold_forbidden", "Manyfold denied access")
		case 404:
			return nil, bad(404, "manyfold_not_found", "Manyfold object not found")
		case 429:
			return nil, bad(429, "manyfold_rate_limited", "Manyfold is limiting requests")
		default:
			return nil, bad(502, "manyfold_unreachable", "Manyfold returned "+res.Status)
		}
	}
	return nil, bad(502, "manyfold_credentials", "Manyfold credentials unavailable")
}
func (s *Service) manyJSON(ctx context.Context, c config, p string) (object, error) {
	res, e := s.manyGet(ctx, c, p, "application/vnd.manyfold.v0+json")
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	var o object
	if json.NewDecoder(io.LimitReader(res.Body, 16<<20)).Decode(&o) != nil || o == nil {
		return nil, bad(502, "manyfold_unreachable", "Manyfold returned invalid JSON")
	}
	return o, nil
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func refID(uri, kind string) string {
	u, e := url.Parse(uri)
	if e != nil {
		return ""
	}
	s := strings.Split(u.Path, "/")
	for i := 0; i < len(s)-1; i++ {
		if s[i] == kind {
			id := strings.Split(s[i+1], ".")[0]
			if validID.MatchString(id) {
				return id
			}
		}
	}
	return ""
}
func list(v any) []object {
	a, _ := v.([]any)
	out := []object{}
	for _, i := range a {
		if o, ok := i.(object); ok {
			out = append(out, o)
		}
	}
	return out
}
func str(v any) string { x, _ := v.(string); return x }
func intVal(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}
func fallback(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
func (s *Service) models(ctx context.Context, c config, page int, q string) (object, error) {
	qv := url.Values{"page": {strconv.Itoa(page)}}
	if q != "" {
		qv.Set("q", q)
	}
	data, e := s.manyJSON(ctx, c, "/models?"+qv.Encode())
	if e != nil {
		return nil, e
	}
	models := []object{}
	for _, m := range list(data["member"]) {
		id := refID(str(m["@id"]), "models")
		if id != "" {
			models = append(models, object{"id": id, "name": fallback(str(m["name"]), id)})
		}
	}
	v, _ := data["view"].(object)
	total := len(models)
	if n, ok := data["totalItems"].(float64); ok {
		total = int(n)
	}
	return object{"total": total, "page": page, "has_next": v["next"] != nil, "has_previous": v["previous"] != nil, "models": models}, nil
}
func readRef(ctx context.Context, db *sql.DB, p access.Principal, source string) (object, error) {
	query := "SELECT id,filename,folder_id FROM library_files WHERE deleted_at IS NULL AND source_url=?"
	args := []any{source}
	if !p.Has("library:read_all") {
		if p.ID == 0 || !p.Has("library:read_own") {
			return nil, nil
		}
		query += " AND created_by_id=?"
		args = append(args, p.ID)
	}
	var id int64
	var name string
	var folder sql.NullInt64
	e := db.QueryRowContext(ctx, query+" ORDER BY id LIMIT 1", args...).Scan(&id, &name, &folder)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	var dest any
	if folder.Valid {
		dest = folder.Int64
	}
	return object{"id": id, "filename": name, "folder_id": dest}, nil
}
func (s *Service) model(ctx context.Context, c config, p access.Principal, id string) (object, string, error) {
	data, e := s.manyJSON(ctx, c, "/models/"+id)
	if e != nil {
		return nil, "", e
	}
	files := []object{}
	for _, v := range list(data["hasPart"]) {
		fid := refID(str(v["@id"]), "model_files")
		if fid == "" {
			continue
		}
		mime := str(v["encodingFormat"])
		ref, e := readRef(ctx, s.db, p, "manyfold:"+id+"/"+fid)
		if e != nil {
			return nil, "", e
		}
		files = append(files, object{"id": fid, "name": fallback(str(v["name"]), fid), "mime": mime, "importable": modelMime(mime), "library_file": ref})
	}
	prev, _ := data["preview_file"].(object)
	cover := refID(str(prev["@id"]), "model_files")
	license, _ := data["spdx:license"].(object)
	tags, _ := data["keywords"].([]any)
	if tags == nil {
		tags = []any{}
	}
	return object{"id": id, "name": fallback(str(data["name"]), id), "caption": data["caption"], "description": data["description"], "license": license["licenseId"], "tags": tags, "url": c.URL + "/models/" + id, "has_preview": cover != "", "files": files}, cover, nil
}
func modelMime(k string) bool { return k == "model/3mf" || k == "model/stl" || k == "model/step" }

type remoteFile struct {
	name, mime, raw string
	size            int64
}

func (s *Service) file(ctx context.Context, c config, model, id string) (remoteFile, error) {
	m, e := s.manyJSON(ctx, c, "/models/"+model+"/model_files/"+id)
	if e != nil {
		return remoteFile{}, e
	}
	u, e := url.Parse(str(m["contentUrl"]))
	if e != nil {
		return remoteFile{}, bad(502, "manyfold_unreachable", "Invalid file download path")
	}
	marker := "/models/" + model + "/raw/"
	_, tail, ok := strings.Cut(u.Path, marker)
	if !ok || tail == "" || strings.Contains(tail, "\\") {
		return remoteFile{}, bad(502, "manyfold_unreachable", "Manyfold supplied no usable download path")
	}
	parts := strings.Split(tail, "/")
	for i, k := range parts {
		if k == "" || k == "." || k == ".." {
			return remoteFile{}, bad(502, "manyfold_unreachable", "Invalid download path")
		}
		parts[i] = url.PathEscape(k)
	}
	return remoteFile{path.Base(tail), str(m["encodingFormat"]), marker + strings.Join(parts, "/"), intVal(m["contentSize"])}, nil
}
func (s *Service) manyfold(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	p := access.From(ctx)
	sub := strings.TrimPrefix(r.URL.Path, "/api/v1/manyfold/")
	perm := "manyfold:view"
	if strings.HasPrefix(sub, "config") {
		perm = "settings:read"
		if r.Method != "GET" {
			perm = "settings:update"
		}
	} else if sub == "import" {
		perm = "manyfold:import"
	}
	if !access.Require(w, r, perm) {
		return
	}
	c, e := s.conf(ctx)
	if e != nil {
		respond(w, nil, e)
		return
	}
	switch sub {
	case "config", "config/test":
		if r.Method == "GET" {
			httpx.JSON(w, 200, c.view())
			return
		}
		if r.Method == "DELETE" {
			_, e = s.db.ExecContext(ctx, "DELETE FROM settings WHERE key IN ('manyfold_url','manyfold_client_id','manyfold_client_secret')")
			if e != nil {
				respond(w, nil, e)
				return
			}
			w.WriteHeader(204)
			return
		}
		var v config
		if !httpx.Decode(w, r, &v) {
			return
		}
		if v.Secret == "" {
			if strings.TrimRight(v.URL, "/") != c.URL && c.Secret != "" {
				respond(w, nil, bad(400, "manyfold_secret_required", "Enter the secret for the new service address"))
				return
			}
			v.Secret = c.Secret
		}
		v, e = validated(v)
		if e != nil {
			respond(w, nil, e)
			return
		}
		if sub == "config/test" {
			m, e := s.models(ctx, v, 1, "")
			respond(w, object{"model_count": m["total"]}, e)
			return
		}
		tx, e := s.db.BeginTx(ctx, nil)
		if e != nil {
			respond(w, nil, e)
			return
		}
		defer tx.Rollback()
		for k, val := range map[string]string{"manyfold_url": v.URL, "manyfold_client_id": v.Client, "manyfold_client_secret": s.auth.SealSecret(v.Secret)} {
			if e = access.Put(tx, k, val); e != nil {
				respond(w, nil, e)
				return
			}
		}
		e = tx.Commit()
		respond(w, v.view(), e)
		return
	case "status":
		ok := c.view()["configured"] == true
		addr := ""
		if ok {
			addr = c.URL
		}
		httpx.JSON(w, 200, object{"configured": ok, "url": addr})
		return
	}
	if c.view()["configured"] != true {
		respond(w, nil, bad(409, "manyfold_not_configured", "Manyfold is not configured"))
		return
	}
	if sub == "models" {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		m, e := s.models(ctx, c, max(1, min(100000, page)), r.URL.Query().Get("q"))
		respond(w, m, e)
		return
	}
	if sub == "import" {
		var body struct {
			Model  string `json:"model_id"`
			File   string `json:"file_id"`
			Folder int64  `json:"folder_id"`
		}
		if !httpx.Decode(w, r, &body) {
			return
		}
		if !validID.MatchString(body.Model) || !validID.MatchString(body.File) {
			respond(w, nil, bad(422, "manyfold_invalid_id", "Invalid Manyfold identifier"))
			return
		}
		v, e := s.importMany(ctx, p, c, body.Model, body.File, body.Folder)
		respond(w, v, e)
		return
	}
	id := r.PathValue("id")
	if !validID.MatchString(id) {
		respond(w, nil, bad(422, "manyfold_invalid_id", "Invalid model ID"))
		return
	}
	model, cover, e := s.model(ctx, c, p, id)
	if e != nil {
		respond(w, nil, e)
		return
	}
	if !strings.HasSuffix(sub, "/preview") {
		httpx.JSON(w, 200, model)
		return
	}
	if cover == "" {
		respond(w, nil, bad(404, "manyfold_not_found", "No model preview"))
		return
	}
	f, e := s.file(ctx, c, id, cover)
	if e != nil {
		respond(w, nil, e)
		return
	}
	der := "render"
	if strings.HasPrefix(f.mime, "image/") {
		der = "preview"
	}
	ext := path.Ext(f.name)
	if !regexp.MustCompile(`^\.[A-Za-z0-9]{1,10}$`).MatchString(ext) {
		respond(w, nil, bad(404, "manyfold_not_found", "Invalid preview type"))
		return
	}
	res, e := s.manyGet(ctx, c, "/models/"+id+"/model_files/"+cover+ext+"?derivative="+der, "image/*")
	if e != nil {
		respond(w, nil, e)
		return
	}
	defer res.Body.Close()
	b, e := io.ReadAll(io.LimitReader(res.Body, 16<<20+1))
	mt := http.DetectContentType(b)
	if e != nil || len(b) > 16<<20 || mt != "image/jpeg" && mt != "image/png" && mt != "image/webp" {
		respond(w, nil, bad(502, "manyfold_unreachable", "Invalid preview image"))
		return
	}
	w.Header().Set("Content-Type", mt)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Write(b)
}
func (s *Service) importMany(ctx context.Context, p access.Principal, c config, model, id string, folder int64) (object, error) {
	source := "manyfold:" + model + "/" + id
	ref, e := readRef(ctx, s.db, p, source)
	if e != nil {
		return nil, e
	}
	if ref != nil {
		return object{"library_file_id": ref["id"], "filename": ref["filename"], "folder_id": ref["folder_id"], "was_existing": true}, nil
	}
	f, e := s.file(ctx, c, model, id)
	if e != nil {
		return nil, e
	}
	ext := strings.ToLower(path.Ext(f.name))
	if !modelMime(f.mime) || ext != ".3mf" && ext != ".stl" && ext != ".step" && ext != ".stp" {
		return nil, bad(415, "manyfold_unsupported_file", "Choose a printable model file")
	}
	if f.size > 2<<30 {
		return nil, bad(413, "manyfold_file_too_large", "Model exceeds 2 GiB")
	}
	res, e := s.manyGet(ctx, c, f.raw, "application/octet-stream")
	if e != nil {
		return nil, e
	}
	defer res.Body.Close()
	if res.ContentLength > 2<<30 {
		return nil, bad(413, "manyfold_file_too_large", "Model exceeds 2 GiB")
	}
	limited := http.MaxBytesReader(nil, res.Body, 2<<30)
	lid, existing, e := s.farm.ImportLibrary(ctx, p, limited, f.name, folder, "manyfold", source)
	if e != nil {
		return nil, bad(422, "manyfold_import_failed", "Could not store this file: "+e.Error())
	}
	var dest any
	if folder != 0 {
		dest = folder
	}
	return object{"library_file_id": lid, "filename": f.name, "folder_id": dest, "was_existing": existing}, nil
}
