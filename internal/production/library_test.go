package production

import (
	"bambastic/internal/access"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"testing"
)

func (f farmTest) file(path, name, contents string, p access.Principal, want int) row {
	f.Helper()
	var b bytes.Buffer
	form := multipart.NewWriter(&b)
	part, e := form.CreateFormFile("file", name)
	if e != nil {
		f.Fatal(e)
	}
	io.WriteString(part, contents)
	form.Close()
	r := httptest.NewRequest("POST", "/api/v1"+path, &b)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r = r.WithContext(access.With(r.Context(), p))
	w := httptest.NewRecorder()
	f.mux.ServeHTTP(w, r)
	if w.Code != want {
		f.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	var v row
	json.Unmarshal(w.Body.Bytes(), &v)
	return v
}
func TestManagedFilesVersionsAndRecoverableDeletion(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	f.req("POST", "/library/folders", row{"name": "Models"}, p, "", 200)
	f.req("POST", "/library/folders", row{"name": "Brackets", "parent_id": 1}, p, "", 200)
	f.req("PUT", "/library/folders/1", row{"parent_id": 2}, p, "", 409)
	v := f.file("/library/files", "README.md", "original", p, 200)
	if num(v["id"]) != 1 {
		t.Fatal(v)
	}
	f.req("POST", "/library/files/move", row{"file_ids": []int{1}, "folder_id": 2}, p, "", 200)
	f.req("POST", "/library/tags", row{"name": "Design"}, p, "", 200)
	f.req("POST", "/library/tags/bulk-assign", row{"file_ids": []int{1}, "tag_ids": []int{1}}, p, "", 200)
	if !strings.Contains(string(f.req("GET", "/library/folders/2/readme", nil, p, "", 200)), "original") {
		t.Fatal("readme missing")
	}
	if !strings.Contains(string(f.req("GET", "/library/files?folder_id=1&recursive=true&tag_ids=1", nil, p, "", 200)), "README.md") {
		t.Fatal("recursive or tag browsing failed")
	}
	f.req("POST", "/library/files/copy", row{"file_ids": []int{1}}, p, "", 200)
	f.file("/library/files/1/versions", "README.md", "changed", p, 200)
	if got := string(f.req("GET", "/library/files/1/download", nil, p, "", 200)); got != "changed" {
		t.Fatal(got)
	}
	f.req("POST", "/library/files/1/versions/1/restore", nil, p, "", 200)
	if got := string(f.req("GET", "/library/files/1/download", nil, p, "", 200)); got != "original" {
		t.Fatal(got)
	}
	if got := string(f.req("GET", "/library/files/2/download", nil, p, "", 200)); got != "original" {
		t.Fatal(got)
	}
	f.req("POST", "/library/bulk-delete", row{"folder_ids": []int{1}}, p, "", 200)
	f.req("GET", "/library/files/1", nil, p, "", 404)
	if !strings.Contains(string(f.req("GET", "/library/trash", nil, p, "", 200)), "README.md") {
		t.Fatal("trash missing")
	}
	f.req("POST", "/library/trash/1/restore", nil, p, "", 200)
	if got := string(f.req("GET", "/library/files/1/download", nil, p, "", 200)); got != "original" {
		t.Fatal(got)
	}
}
func TestArchivePerPlateAndScopedRuns(t *testing.T) {
	f := farmFixture(t)
	f.upload(f.admin)
	f.req("PATCH", "/archives/1", row{"tags": "Bracket,blue", "notes": "enclosure prototype"}, f.admin, "", 200)
	if !strings.Contains(string(f.req("GET", "/archives/search?q=prototype", nil, f.admin, "", 200)), "design.3mf") {
		t.Fatal("note did not match")
	}
	f.req("PUT", "/archives/tags/Bracket", row{"new_name": "Assembly"}, f.admin, "", 200)
	if !strings.Contains(string(f.req("GET", "/archives/tags", nil, f.admin, "", 200)), "Assembly") {
		t.Fatal("tag rename failed")
	}
	if got := string(f.req("GET", "/archives/1/gcode?plate=7", nil, f.admin, "", 200)); !strings.Contains(got, "simulation") {
		t.Fatal(got)
	}
	f.req("GET", "/archives/1/gcode?plate=3", nil, f.admin, "", 404)
	_, e := f.s.db.Exec("INSERT INTO print_log_entries(archive_id,printer_id,print_name,status,completed_at) VALUES(1,1,'Visible','completed','2025-01-01'),(1,2,'Other job','failed','2025-02-01')")
	if e != nil {
		t.Fatal(e)
	}
	p := access.Principal{Admin: true, PrinterIDs: map[int64]bool{1: true}}
	f.req("GET", "/archives/1/runs", nil, p, "", 200)
	if got := string(f.req("GET", "/archives/1/runs", nil, p, "", 200)); strings.Contains(got, "Other job") || !strings.Contains(got, "Visible") {
		t.Fatal(got)
	}
	var detail row
	json.Unmarshal(f.req("GET", "/archives/1", nil, p, "", 200), &detail)
	if detail["status"] != "completed" || num(detail["run_count"]) != 1 {
		t.Fatal(detail)
	}
}
