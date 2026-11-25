package production

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path"
	"testing"

	"bambastic/internal/access"
	"github.com/jlaffaye/ftp"
)

type mockCard map[string][]byte

func (m mockCard) Open(n string) (io.ReadCloser, error) {
	v, ok := m[n]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return io.NopCloser(bytes.NewReader(v)), nil
}
func (m mockCard) List(d string) ([]*ftp.Entry, error) {
	var out []*ftp.Entry
	for n, v := range m {
		if path.Dir(n) == d {
			out = append(out, &ftp.Entry{Name: path.Base(n), Size: uint64(len(v))})
		}
	}
	return out, nil
}
func (m mockCard) Stor(n string, r io.Reader) error { v, e := io.ReadAll(r); m[n] = v; return e }
func (m mockCard) Delete(n string) error            { delete(m, n); return nil }
func (m mockCard) Rename(from, to string) error {
	v, ok := m[from]
	if !ok {
		return errors.New("missing source")
	}
	m[to] = v
	delete(m, from)
	return nil
}
func (m mockCard) FileSize(n string) (int64, error) {
	v, ok := m[n]
	if !ok {
		return 0, sql.ErrNoRows
	}
	return int64(len(v)), nil
}
func (m mockCard) Quit() error { return nil }

func TestCardTransferAndDownloadScope(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	remote := mockCard{"/plate.gcode": []byte("G28")}
	f.s.openCard = func(_ context.Context, pr row) (card, error) {
		if val(pr["fingerprint"]) != "fixture" {
			t.Fatal("missing printer trust")
		}
		return remote, nil
	}
	f.req("GET", "/printers/1/files", nil, access.Principal{Permissions: map[string]bool{"printers:files": true}, PrinterIDs: map[int64]bool{2: true}}, "", 404)
	f.file("/printers/1/files?path=/cache", "frame.gcode", "G1", f.admin, 200)
	f.file("/printers/1/files?path=/cache", "frame.gcode", "other", f.admin, 409)
	if string(remote["/cache/frame.gcode"]) != "G1" || len(remote) != 2 {
		t.Fatal(remote)
	}
	f.req("GET", "/printers/1/files/download?path=/../plate.gcode", nil, f.admin, "", 422)
	data := f.req("GET", "/printers/1/files/download?path=/plate.gcode", nil, f.admin, "", 200)
	if string(data) != "G28" {
		t.Fatal(string(data))
	}
	auth, e := access.New(f.s.db, f.s.dir)
	if e != nil {
		t.Fatal(e)
	}
	public := http.NewServeMux()
	f.s.RegisterDownloads(public, auth)
	var job row
	json.Unmarshal(f.req("POST", "/printers/1/files/download-job", row{"paths": []string{"/plate.gcode", "/cache/frame.gcode", "/absent"}}, f.admin, "", 200), &job)
	k := val(job["job_id"])
	j, e := one(ctx, f.s.db, "SELECT * FROM bb_file_jobs WHERE job_id=?", k)
	if e != nil {
		t.Fatal(e)
	}
	f.s.prepareDownload(ctx, j)
	json.Unmarshal(f.req("GET", "/printers/1/files/download-jobs/"+k, nil, f.admin, "", 200), &job)
	if num(job["successful"]) != 2 || num(job["failed"]) != 1 || job["state"] != "ready" {
		t.Fatal(job)
	}
	url := "/api/v1/printers/1/files/dl/" + val(job["token"]) + "/printer-files.zip"
	get := func(url string, want int) []byte {
		t.Helper()
		w := httptest.NewRecorder()
		public.ServeHTTP(w, httptest.NewRequest("GET", url, nil))
		if w.Code != want {
			t.Fatalf("%d: %s", w.Code, w.Body.String())
		}
		return w.Body.Bytes()
	}
	get(url+"-different", 404)
	data = get(url, 200)
	z, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if e != nil || len(z.File) != 2 {
		t.Fatalf("zip: %v %v", z, e)
	}
	for _, entry := range z.File {
		r, e := entry.Open()
		if e != nil {
			t.Fatal(e)
		}
		data, e := io.ReadAll(r)
		r.Close()
		if e != nil || !bytes.Equal(data, remote["/"+entry.Name]) {
			t.Fatal(entry.Name, string(data), e)
		}
	}
	f.req("DELETE", "/printers/1/files/download-jobs/"+k, nil, f.admin, "", 200)
	get(url, 404)
}
