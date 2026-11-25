package production

import (
	"bambastic/internal/access"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWritableExternalUploadTransferUnlinkAndRecovery(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	p := f.admin
	allowed := t.TempDir()
	t.Setenv("BAMBASTIC_EXTERNAL_ROOTS", allowed)
	dest := filepath.Join(allowed, "exchange")
	if e := os.Mkdir(dest, 0700); e != nil {
		t.Fatal(e)
	}
	f.req("POST", "/library/folders/external", row{"external_path": dest, "name": "Exchange", "readonly": false}, p, "", 200)
	upload := func(name, content string, folder int, code int) row {
		t.Helper()
		var b bytes.Buffer
		form := multipart.NewWriter(&b)
		part, e := form.CreateFormFile("file", name)
		if e != nil {
			t.Fatal(e)
		}
		io.WriteString(part, content)
		form.WriteField("folder_id", fmt.Sprint(folder))
		form.Close()
		r := httptest.NewRequest("POST", "/api/v1/library/files", &b)
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r.WithContext(access.With(ctx, p)))
		if w.Code != code {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
		var v row
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	path := func(n string) string { return filepath.Join(dest, n) }
	assert := func(n, want string) {
		t.Helper()
		data, e := os.ReadFile(path(n))
		if e != nil || string(data) != want {
			t.Fatalf("%s: %q %v", n, data, e)
		}
	}
	missing := func(n string) {
		t.Helper()
		if _, e := os.Lstat(path(n)); !os.IsNotExist(e) {
			t.Fatal(n, e)
		}
	}
	if v := upload("part.stl", triangleSTL, 1, 200); !flag(v["is_external"]) {
		t.Fatal(v)
	}
	assert("part.stl", triangleSTL)
	upload("part.stl", "collision", 1, 409)
	assert("part.stl", triangleSTL)
	f.req("PUT", "/library/files/1", row{"filename": "new.stl"}, p, "", 200)
	assert("new.stl", triangleSTL)
	missing("part.stl")
	// Deliberately fail the SQL write after both disk changes have been staged.
	_, e := f.s.db.Exec("CREATE TRIGGER fail_rename BEFORE UPDATE OF filename ON library_files BEGIN SELECT RAISE(ABORT,'fixture rollback'); END;")
	if e != nil {
		t.Fatal(e)
	}
	f.req("PUT", "/library/files/1", row{"filename": "never.stl"}, p, "", 500)
	assert("new.stl", triangleSTL)
	missing("never.stl")
	f.s.db.Exec("DROP TRIGGER fail_rename")
	if v := string(f.req("POST", "/library/files/copy", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(v, `"copied":1`) {
		t.Fatal(v)
	}
	if v := string(f.req("POST", "/library/files/move", row{"file_ids": []int{2}, "folder_id": 1}, p, "", 200)); !strings.Contains(v, "name_collision") {
		t.Fatal(v)
	}
	if e = os.WriteFile(path("new.stl"), []byte("externally edited"), 0600); e != nil {
		t.Fatal(e)
	}
	if v := string(f.req("POST", "/library/files/move", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(v, "source_changed") {
		t.Fatal(v)
	}
	assert("new.stl", "externally edited")
	f.req("POST", "/library/folders/1/scan", nil, p, "", 200)
	if v := string(f.req("POST", "/library/files/move", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(v, `"moved":1`) {
		t.Fatal(v)
	}
	missing("new.stl")
	if got := string(f.req("GET", "/library/files/1/download", nil, p, "", 200)); got != "externally edited" {
		t.Fatal(got)
	}
	if v := string(f.req("POST", "/library/files/move", row{"file_ids": []int{2}, "folder_id": 1}, p, "", 200)); !strings.Contains(v, `"moved":1`) {
		t.Fatal(v)
	}
	assert("new.stl", triangleSTL)
	// Reopen with uncommitted disk changes and a rolled-back SQL transaction,
	// as after an abrupt process exit.
	tx, e := f.s.db.BeginTx(ctx, nil)
	if e != nil {
		t.Fatal(e)
	}
	src, e := one(ctx, tx, "SELECT * FROM library_files WHERE id=2")
	if e != nil {
		t.Fatal(e)
	}
	fo, e := f.s.externalFolder(ctx, tx, 1)
	if e != nil {
		t.Fatal(e)
	}
	edits := externalEdits{}
	if e = edits.transfer(ctx, f.s, tx, src, fo, fo, "interrupted.stl", true); e != nil {
		t.Fatal(e)
	}
	assert("interrupted.stl", triangleSTL)
	missing("new.stl")
	tx.Rollback()
	if e = f.s.recoverExternal(); e != nil {
		t.Fatal(e)
	}
	assert("new.stl", triangleSTL)
	missing("interrupted.stl")
	f.req("DELETE", "/library/files/2", nil, p, "", 200)
	assert("new.stl", triangleSTL)
	f.req("GET", "/library/files/2", nil, p, "", 404)
	f.req("POST", "/library/folders/1/scan", nil, p, "", 200)
	f.req("DELETE", "/library/folders/1", nil, p, "", 200)
	assert("new.stl", triangleSTL)
	entries, e := os.ReadDir(f.s.externalJournal(""))
	if e != nil || len(entries) != 0 {
		t.Fatal("pending work", entries, e)
	}
}
func TestReadonlyExternalSubfolderCopyAndSymlinks(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	allowed := t.TempDir()
	t.Setenv("BAMBASTIC_EXTERNAL_ROOTS", allowed)
	sub := filepath.Join(allowed, "designs")
	os.Mkdir(sub, 0700)
	os.WriteFile(filepath.Join(sub, "model.stl"), []byte(triangleSTL), 0600)
	f.req("POST", "/library/folders/external", row{"external_path": allowed, "name": "Read only"}, p, "", 200)
	f.req("POST", "/library/folders/1/scan", nil, p, "", 200)
	if got := string(f.req("POST", "/library/files/move", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(got, "source_readonly") {
		t.Fatal(got)
	}
	if got := string(f.req("POST", "/library/files/copy", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(got, `"copied":1`) {
		t.Fatal(got)
	}
	f.req("POST", "/library/files/move", row{"file_ids": []int{2}, "folder_id": 2}, p, "", 403)
	os.Remove(filepath.Join(sub, "model.stl"))
	os.Symlink(filepath.Join(f.s.dir, "production"), filepath.Join(sub, "model.stl"))
	if got := string(f.req("POST", "/library/files/copy", row{"file_ids": []int{1}}, p, "", 200)); !strings.Contains(got, `"skipped":1`) {
		t.Fatal(got)
	}
}
