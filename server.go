package main

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
	"bambastic/internal/backup"
	"bambastic/internal/bridge"
	"bambastic/internal/cloudbackup"
	"bambastic/internal/control"
	"bambastic/internal/httpx"
	"bambastic/internal/integrations"
	"bambastic/internal/live"
	"bambastic/internal/management"
	"bambastic/internal/monitoring"
	"bambastic/internal/mqttrelay"
	"bambastic/internal/operations"
	"bambastic/internal/production"
	"bambastic/internal/releases"
	"bambastic/internal/slicing"
	"bambastic/internal/support"
	"bambastic/internal/uidiagnostics"
	"bambastic/internal/virtualprinter"
	"github.com/gofrs/flock"
	_ "modernc.org/sqlite"
)

//go:embed spec/storage.sql
var ddl string

//go:embed spec/openapi.json
var reference []byte

type application struct {
	lease        databaseLease
	database     databaseOptions
	db           *sql.DB
	handler      http.Handler
	auth         *access.Service
	farm         *production.Service
	management   *management.Service
	slicer       *slicing.Service
	diag         *support.Service
	remoteBackup *cloudbackup.Service
	localBackup  *backup.Local
	reload       chan struct{}
	relay        *mqttrelay.Service
	ops          *operations.Service
	monitor      *monitoring.Service
	virtual      *virtualprinter.Service
	lock         *flock.Flock
	catalog      *bridge.Catalog
}

func openDB(dir string) (*sql.DB, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	absolute, e := filepath.Abs(filepath.Join(dir, "bambastic.db"))
	if e != nil {
		return nil, e
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute)}
	params := u.Query()
	params.Add("_pragma", "foreign_keys(1)")
	params.Add("_pragma", "busy_timeout(10000)")
	params.Set("_txlock", "immediate")
	u.RawQuery = params.Encode()
	db, e := sql.Open("sqlite", u.String())
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(8)
	ready := false
	defer func() {
		if !ready {
			db.Close()
		}
	}()
	if _, e = db.Exec("PRAGMA journal_mode=WAL"); e != nil {
		return nil, e
	}
	tx, e := db.Begin()
	if e != nil {
		return nil, e
	}
	defer tx.Rollback()
	var n int
	if e = tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='printers'").Scan(&n); e != nil {
		return nil, e
	}
	if n == 0 {
		if _, e = tx.Exec(ddl); e != nil {
			return nil, fmt.Errorf("initialize database: %w", e)
		}
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	ready = true
	return db, nil
}
func createApp(data string) (*application, error) {
	return createConfiguredApp(context.Background(), data, databaseOptions{})
}
func createConfiguredApp(ctx context.Context, data string, options databaseOptions) (out *application, err error) {
	if err = os.MkdirAll(data, 0700); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(data, "bambastic.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("another Bambastic server owns this data directory")
	}
	defer func() {
		if err != nil {
			lock.Unlock()
		}
	}()
	root := data
	data, err = backup.Active(root)
	if err != nil {
		return nil, err
	}
	db, settings, err := options.open(ctx, root, data)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			db.Close()
			if settings.lease != nil {
				settings.lease.Close()
			}
		}
	}()
	app, err := assembleApp(root, data, db, settings)
	if err != nil {
		return nil, err
	}
	if settings.importData != nil {
		changed, e := settings.importData(ctx)
		if e != nil {
			app.slicer.Close()
			return nil, e
		}
		if changed {
			app.slicer.Close()
			app, err = assembleApp(root, data, db, settings)
			if err != nil {
				return nil, err
			}
		}
	}
	app.database = options
	app.lock = lock
	app.lease = settings.lease
	return app, nil
}
func assembleApp(root, data string, db *sql.DB, settings storageHooks) (*application, error) {
	var err error
	auth, err := access.New(db, data)
	if err != nil {
		return nil, err
	}
	mg, err := management.New(db)
	if err != nil {
		return nil, err
	}
	farm, err := production.New(db, data)
	if err != nil {
		return nil, err
	}
	catalog, err := bridge.New()
	if err != nil {
		return nil, err
	}
	api := http.NewServeMux()
	router := http.NewServeMux()
	if developmentBuild {
		if err := uidiagnostics.Register(router, root); err != nil {
			return nil, err
		}
	}
	auth.Register(router, api)
	diag, err := support.New(db)
	if err != nil {
		return nil, err
	}
	diag.Register(api)
	controls := control.New(db, data, auth)
	controls.Register(router, api)
	releases.New().Register(api)
	controls.Metrics(router, farm.InitialStatus)
	controls.System(api, farm.InitialStatus, settings.pg)
	ops, err := operations.New(db)
	if err != nil {
		return nil, err
	}
	relay := mqttrelay.New(db, auth.ReadSecret)
	relay.Initial = farm.InitialStatus
	relay.Register(api)
	ops.InventoryRelay = relay
	ops.ReadSecret = auth.ReadSecret
	ops.AccountMail = auth.SendAccountEmail
	ops.Register(api)
	mon, err := monitoring.New(db, data, auth, farm)
	if err != nil {
		return nil, err
	}
	mon.Register(router, api)
	ops.RegisterPhotos(router, data, mon.NotificationSnapshot)
	mon.SetDetectorPowerOff(ops.PowerOffForDetection)
	integration, err := integrations.New(db, data, auth, farm)
	if err != nil {
		return nil, err
	}
	integration.Register(api)
	remote, err := cloudbackup.New(db, auth)
	if err != nil {
		return nil, err
	}
	remote.CalibrationConnected = farm.CalibrationConnected
	remote.KProfiles = farm.KProfiles
	remote.RestoreKProfiles = farm.RestoreKProfiles
	remote.CloudProfiles = integration.CloudProfiles
	remote.ResolvePreset = integration.ResolvePreset
	remote.Register(api)
	mg.SealStationSecret = auth.SealSecret
	mg.OpenStationSecret = auth.OpenSecret
	mg.MaterialSlots = farm.MaterialSlots
	mg.ConfigureNativeAssignment = farm.ConfigureNativeAssignment
	farm.ResolveSlotPreset = integration.ResolvePreset
	farm.SpoolmanMaterialIdentity = mg.SpoolmanMaterialIdentity
	mg.SetProjectFiles(data, farm)
	mg.Register(api)
	reload := make(chan struct{}, 1)
	localBackup := &backup.Local{DB: db, Data: data, Root: root, Export: settings.export, PrepareRestore: settings.prepare, Reload: func() {
		select {
		case reload <- struct{}{}:
		default:
		}
	}}
	localBackup.Register(api)
	api.HandleFunc("GET /api/v1/settings/backup", backup.Serve(db, data, settings.export))
	farm.Register(api)
	farm.RegisterDownloads(router, auth)
	slicer, err := slicing.New(db, data)
	if err != nil {
		return nil, err
	}
	slicer.AttachCloud(integration)
	slicer.AttachFarm(farm)
	slicer.Register(api)
	virtual, err := virtualprinter.New(db, farm)
	if err != nil {
		return nil, err
	}
	virtual.Register(api)
	hub := live.New(auth)
	publish := func(event map[string]any) {
		hub.Publish(event)
		mon.Publish(event)
		relay.Publish(event)
		ops.Publish(event)
		diag.Domain(event)
	}
	ops.EventSink = publish
	ops.MQTTStatus = relay.PlugStatus
	farm.SetEventSink(publish)
	mg.SetEventSink(publish)
	farm.SetUserResolver(auth.User)
	hub.Initial = farm.InitialStatus
	router.Handle("GET /api/v1/ws", hub)
	router.Handle("/api/", auth.Protect(api))
	if err = auth.RegisterOAuth(router, api, catalog.HTTP(api, os.Getenv("BAMBASTIC_PUBLIC_URL"))); err != nil {
		return nil, err
	}
	router.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		if db.PingContext(r.Context()) != nil {
			httpx.Error(w, 503, "Database unavailable")
			return
		}
		httpx.JSON(w, 200, map[string]any{"status": "healthy"})
	})
	browser, err := browserHandler()
	if err != nil {
		return nil, err
	}
	schema, err := supportedSchema(api, router, browser)
	if err != nil {
		return nil, err
	}
	router.HandleFunc("GET /openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(schema)
	})
	api.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { httpx.Error(w, 404, "API route not found") })
	router.Handle("/", browser)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if origin := r.Header.Get("Origin"); origin != "" && (r.Method != "GET" && r.Method != "HEAD" || r.URL.Path == "/mcp") {
			u, e := url.Parse(origin)
			if e != nil || !strings.EqualFold(u.Host, r.Host) {
				httpx.Error(w, 403, "Cross-origin request refused")
				return
			}
		}
		diag.Request(r)
		router.ServeHTTP(w, r)
	})
	return &application{db: db, auth: auth, handler: handler, farm: farm, management: mg, ops: ops, relay: relay, monitor: mon, slicer: slicer, diag: diag, remoteBackup: remote, localBackup: localBackup, reload: reload, virtual: virtual, catalog: catalog}, nil
}
func (a *application) close() {
	a.slicer.Close()
	a.db.Close()
	if a.lease != nil {
		a.lease.Close()
	}
	if a.lock != nil {
		a.lock.Unlock()
	}
}

var parameter = regexp.MustCompile(`\{[^{}]+\}`)

// A literal endpoint such as /printers/discovered must have its own handler.
// Merely falling through to /printers/{id} cannot satisfy that contract.
func matchesRoute(method, path, registered string) bool {
	m, pattern, ok := strings.Cut(registered, " ")
	if !ok || !strings.EqualFold(method, m) {
		return false
	}
	canonical := func(p string) string {
		return strings.TrimSuffix(parameter.ReplaceAllString(strings.TrimSuffix(p, "{$}"), "{}"), "/")
	}
	if pattern == "/api/v1/archives/{id}/{sub}" && method == "delete" {
		switch canonical(path) {
		case "/api/v1/archives/tags/{}", "/api/v1/archives/{}/source", "/api/v1/archives/{}/f3d":
			return true
		}
	}
	if pattern == "/api/v1/library/folders/{id}/{sub}" && method == "get" {
		switch canonical(path) {
		case "/api/v1/library/folders/by-archive/{}", "/api/v1/library/folders/by-project/{}", "/api/v1/library/folders/{}/readme":
			return true
		}
	}
	if pattern == "/api/v1/projects/{id}/{action}" && strings.HasPrefix(path, "/api/v1/projects/{") {
		for _, action := range []string{"bom", "cover-image", "add-archives", "add-queue", "remove-archives", "create-template"} {
			if canonical(path) == "/api/v1/projects/{}/"+action {
				return true
			}
		}
	}
	if pattern == "/api/v1/smart-plugs/{id}/{action}" && canonical(path) == "/api/v1/smart-plugs/{}/status" {
		return true
	}
	return canonical(path) == canonical(pattern)
}

func supportedSchema(api, public *http.ServeMux, browser http.Handler) ([]byte, error) {
	var doc map[string]any
	if e := json.Unmarshal(reference, &doc); e != nil {
		return nil, e
	}
	components := doc["components"].(map[string]any)["schemas"].(map[string]any)
	scan := components["TagScannedRequest"].(map[string]any)["properties"].(map[string]any)
	scan["opentag3d_ndef"] = map[string]any{"type": "string", "maxLength": 510, "description": "Hex-encoded OpenTag3D NDEF record from the station"}
	for _, name := range []string{"VirtualPrinterCreate", "VirtualPrinterUpdate"} {
		properties := components[name].(map[string]any)["properties"].(map[string]any)
		properties["target_access_code"] = map[string]any{
			"type": "string", "format": "password", "writeOnly": true, "maxLength": 8,
			"description": "Optional eight-character LAN code for the explicitly selected target_printer_id. A replacement requires printers:update and a verified connection. Leave blank to reuse the saved code.",
		}
	}
	paths := doc["paths"].(map[string]any)
	for p, raw := range paths {
		methods := raw.(map[string]any)
		for method := range methods {
			req, _ := http.NewRequest(strings.ToUpper(method), parameter.ReplaceAllString(p, "1"), nil)
			_, a := api.Handler(req)
			_, b := public.Handler(req)
			if !strings.HasPrefix(p, "/api/") && (method == "get" || method == "head") {
				switch p {
				case "/", "/{full_path}", "/manifest.json", "/sw-register.js", "/sw.js":
					w := httptest.NewRecorder()
					browser.ServeHTTP(w, req)
					if w.Code == http.StatusOK {
						continue
					}
				}
			}
			if !matchesRoute(method, p, a) && !matchesRoute(method, p, b) {
				delete(methods, method)
			}
		}
		if len(methods) == 0 {
			delete(paths, p)
		}
	}
	doc["info"] = map[string]string{"title": "Bambastic", "version": "0.1.0", "description": "Registered Bambastic API routes, using the Bambuddy v1.3.2 reference data shapes."}
	return json.Marshal(doc)
}

func site(files fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/")
		if name == "" {
			name = "index.html"
		}
		if !fs.ValidPath(name) {
			http.NotFound(w, r)
			return
		}
		data, e := fs.ReadFile(files, name)
		if e != nil {
			for _, prefix := range []string{"assets/", "icons/", "fonts/", "img/", "api/", "mcp/", "oauth/", "metrics/", "health/"} {
				if strings.HasPrefix(name, prefix) {
					http.NotFound(w, r)
					return
				}
			}
			if path.Ext(name) != "" || name == "api" || name == "mcp" {
				http.NotFound(w, r)
				return
			}
			name = "index.html"
			data, e = fs.ReadFile(files, name)
			if e != nil {
				http.Error(w, "Frontend bundle unavailable", 503)
				return
			}
		}
		typ := mime.TypeByExtension(path.Ext(name))
		if name == "manifest.json" {
			typ = "application/manifest+json"
		}
		if typ != "" {
			w.Header().Set("Content-Type", typ)
		}
		if name == "sw.js" {
			w.Header().Set("Service-Worker-Allowed", "/")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method != "HEAD" {
			w.Write(data)
		}
	})
}
func (a *application) serve(ctx context.Context, addr string) error {
	parent := ctx
	srv := &http.Server{Addr: addr, Handler: a.handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	diagDone := make(chan struct{})
	go func() { defer close(diagDone); a.diag.Run(ctx) }()
	backupDone := make(chan struct{})
	go func() { defer close(backupDone); a.localBackup.Run(ctx) }()
	remoteDone := a.remoteBackup.Start(ctx)
	mgDone := a.management.Start(ctx)
	opsDone := a.ops.Start(ctx)
	relayDone := a.relay.Start(ctx)
	monDone := make(chan struct{})
	go func() { defer close(monDone); a.monitor.Run(ctx) }()
	virtualDone := make(chan struct{})
	go func() { defer close(virtualDone); a.virtual.Run(ctx) }()
	done := make(chan struct{})
	go func() { defer close(done); a.farm.Run(ctx) }()
	var lost <-chan struct{}
	if a.lease != nil {
		lost = a.lease.Done()
	}
	stop := make(chan struct{})
	go func() {
		defer close(stop)
		select {
		case <-ctx.Done():
		case <-lost:
			cancel()
		case <-a.reload:
			cancel()
		}
		c, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(c)
	}()
	err := srv.ListenAndServe()
	cancel()
	<-stop
	<-done
	<-virtualDone
	<-monDone
	<-backupDone
	<-remoteDone
	<-diagDone
	<-mgDone
	<-opsDone
	<-relayDone
	if parent.Err() == nil && lost != nil {
		select {
		case <-lost:
			return errors.New("PostgreSQL scheduler lease lost")
		default:
		}
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
