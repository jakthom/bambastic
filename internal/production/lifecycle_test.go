package production

import (
	"archive/zip"
	"bytes"
	"context"

	"encoding/json"
	"errors"

	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"

	"strings"
	"testing"

	"bambastic/internal/access"
	"bambastic/internal/management"
	"bambastic/internal/testdb"
	_ "modernc.org/sqlite"
)

type mockHardware struct {
	uploaded string
	messages []row
	during   func()
}

func (m *mockHardware) Upload(ctx context.Context, src, name string, progress func(int64)) error {
	st, e := os.Stat(src)
	if e != nil {
		return e
	}
	m.uploaded = name
	progress(st.Size())
	if m.during != nil {
		m.during()
	}
	return nil
}
func (m *mockHardware) Publish(_ context.Context, v row) error {
	m.messages = append(m.messages, v)
	return nil
}
func (*mockHardware) Fingerprint() string { return "fixture" }
func (*mockHardware) Online() bool        { return true }
func (*mockHardware) Close()              {}

type farmTest struct {
	*testing.T
	s     *Service
	mux   *http.ServeMux
	admin access.Principal
	hw    *mockHardware
	pr    row
}

func farmFixture(t *testing.T) farmTest {
	t.Helper()
	dir := t.TempDir()
	db := testdb.Open(t)
	var e error
	if _, e = management.New(db); e != nil {
		t.Fatal(e)
	}
	s, e := New(db, dir)
	if e != nil {
		t.Fatal(e)
	}
	m := http.NewServeMux()
	s.Register(m)
	hw := &mockHardware{}
	s.open = func(_ context.Context, _ row, _ string, _ func(row)) (hardware, error) { return hw, nil }
	f := farmTest{t, s, m, access.Principal{Admin: true}, hw, nil}
	res := f.req("POST", "/printers/", row{"name": "Test", "ip_address": "127.0.0.1", "serial_number": "SIM123", "access_code": "fixture", "model": "X1C"}, f.admin, "", 200)
	var pr row
	json.Unmarshal(res, &pr)
	pr["id"] = int64(num(pr["id"]))
	f.pr = pr
	s.devices[num(pr["id"])] = &liveDevice{h: hw, printer: pr, report: row{"gcode_state": "IDLE"}}
	return f
}
func (f farmTest) req(method, path string, data any, p access.Principal, key string, want int) []byte {
	f.Helper()
	var body io.Reader
	if data != nil {
		b, _ := json.Marshal(data)
		body = bytes.NewReader(b)
	}
	r := httptest.NewRequest(method, "/api/v1"+path, body)
	r = r.WithContext(access.With(r.Context(), p))
	r.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != want {
		f.Fatalf("%s %s expected %d, got %d: %s", method, path, want, w.Code, w.Body.String())
	}
	return w.Body.Bytes()
}
func (f farmTest) upload(p access.Principal) int64 {
	f.Helper()
	var file bytes.Buffer
	z := zip.NewWriter(&file)
	for _, s := range []string{"Metadata/plate_2.gcode", "Metadata/plate_7.gcode"} {
		w, _ := z.Create(s)
		io.WriteString(w, "; simulation fixture")
	}
	w, _ := z.Create("Metadata/slice_info.config")
	io.WriteString(w, `<config><plate><metadata key="plater_id" value="2"/><metadata key="weight" value="40"/></plate></config>`)
	z.Close()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, _ := form.CreateFormFile("file", "design.3mf")
	io.Copy(part, &file)
	form.Close()
	r := httptest.NewRequest("POST", "/api/v1/archives/upload", &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r = r.WithContext(access.With(r.Context(), p))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	if rec.Code != 200 {
		f.Fatal(rec.Body.String())
	}
	var v row
	json.Unmarshal(rec.Body.Bytes(), &v)
	return num(v["id"])
}
func TestPhysicalRunAndReplay(t *testing.T) {
	f := farmFixture(t)
	ctx := context.Background()
	aid := f.upload(f.admin)
	q := row{"archive_id": aid, "printer_id": 1, "plate_id": 2, "quantity": 2}
	first := f.req("POST", "/queue/", q, f.admin, "order", 200)
	second := f.req("POST", "/queue/", q, f.admin, "order", 200)
	var a, b row
	json.Unmarshal(first, &a)
	json.Unmarshal(second, &b)
	if a["id"] != b["id"] || len(a["queue_item_ids"].([]any)) != 2 {
		t.Fatal(a, b)
	}
	q["quantity"] = 4
	f.req("POST", "/queue/", q, f.admin, "order", 409)
	v, e := f.s.claim(ctx, f.pr)
	if e != nil || v == nil {
		t.Fatalf("claim %v %v", v, e)
	}
	f.s.send(ctx, v)
	if len(f.hw.messages) != 1 || f.hw.uploaded == "" {
		t.Fatal("missing device dispatch")
	}
	view := func(state string) row {
		return row{"gcode_state": state, "gcode_file": f.hw.uploaded, "subtask_id": "22", "mc_percent": 100}
	}
	for _, st := range []string{"RUNNING", "RUNNING", "FINISH", "FINISH"} {
		if e = f.s.observe(ctx, f.pr, view(st)); e != nil {
			t.Fatal(e)
		}
	}
	var n int
	var plate int
	var grams float64
	if e = f.s.db.QueryRow("SELECT count(*),min(plate_id),sum(filament_used_grams) FROM print_log_entries WHERE status='completed'").Scan(&n, &plate, &grams); e != nil || n != 1 || plate != 2 || grams != 40 {
		t.Fatal(n, plate, grams, e)
	}
}
func TestRequiredReviewAndCancellationDuringTransfer(t *testing.T) {
	f := farmFixture(t)
	if _, e := f.s.db.Exec("INSERT INTO users(id,username,role,auth_source,is_active) VALUES(1,'student','user','local',1)"); e != nil {
		t.Fatal(e)
	}
	student := access.Principal{ID: 1, Permissions: map[string]bool{"archives:create": true, "archives:read_own": true, "archives:reprint_own": true, "queue:create": true, "queue:update_own": true, "queue:read_own": true}}
	f.s.SetUserResolver(func(context.Context, int64) (access.Principal, error) { return student, nil })
	aid := f.upload(student)
	f.req("POST", "/queue/", row{"archive_id": aid, "printer_id": 1, "plate_id": 7}, student, "", 200)
	check := func() {
		j, e := f.s.claim(context.Background(), f.pr)
		if e != nil || j != nil {
			t.Fatal("review failed", j, e)
		}
	}
	check()
	f.req("POST", "/queue/1/start", nil, student, "", 403)
	f.req("PATCH", "/queue/1", row{"manual_start": false}, student, "", 403)
	check()
	f.req("POST", "/queue/1/start", nil, f.admin, "", 200)
	v, e := f.s.claim(context.Background(), f.pr)
	if v == nil || e != nil {
		t.Fatal(v, e)
	}
	f.hw.during = func() { f.req("POST", "/queue/1/cancel", nil, f.admin, "", 200) }
	f.s.send(context.Background(), v)
	if len(f.hw.messages) != 0 {
		t.Fatal("start sent after cancellation")
	}
	if !strings.Contains(string(f.req("GET", "/queue/1", nil, student, "", 200)), "cancelled") {
		t.Fatal("expected cancellation")
	}
}
func TestUnverifiedPrinterIsNotRegistered(t *testing.T) {
	f := farmFixture(t)
	f.s.open = func(context.Context, row, string, func(row)) (hardware, error) {
		return nil, errors.New("handshake failed")
	}
	f.req("POST", "/printers/", row{"name": "Other", "serial_number": "SIM456", "ip_address": "127.0.0.2", "access_code": "wrong"}, f.admin, "", 400)
	var n int
	_ = f.s.db.QueryRow("SELECT count(*) FROM printers").Scan(&n)
	if n != 1 {
		t.Fatal(n)
	}
}
