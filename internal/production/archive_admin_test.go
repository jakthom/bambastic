package production

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"bambastic/internal/access"
)

func TestAdminArchiveMergeRetentionAndReprice(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	a := f.file("/archives/upload", "one.gcode", "; model\nG1 X2\n", p, 200)
	b := f.file("/archives/upload", "two.gcode", "; model\nG1 X2\n", p, 200)
	head, dupe := num(a["id"]), num(b["id"])
	ctx := context.Background()
	if out := string(f.req("GET", "/archives/merge-count", nil, p, "", 200)); !strings.Contains(out, `"groups":1`) {
		t.Fatal(out)
	}
	out := string(f.req("POST", "/archives/merge", row{"survivor_ids": []int64{head}}, p, "", 200))
	if !strings.Contains(out, `"merged_ids"`) {
		t.Fatal(out)
	}
	v, e := f.s.getAsset(ctx, f.s.db, p, "archives", dupe, "read")
	if e != nil || num(v["id"]) != head {
		t.Fatal(v, e)
	}
	if _, e = f.s.db.Exec("UPDATE print_archives SET filament_used_grams=100,filament_type='PLA',created_at=datetime('now','-45 days') WHERE id=?", head); e != nil {
		t.Fatal(e)
	}
	f.req("POST", "/archives/recalculate-costs", nil, p, "", 200)
	var cost float64
	if e = f.s.db.QueryRow("SELECT cost FROM print_archives WHERE id=?", head).Scan(&cost); e != nil || cost != 2.5 {
		t.Fatal(cost, e)
	}
	f.req("POST", "/archives/search/rebuild-index", nil, p, "", 200)
	f.req("PUT", "/archives/purge/settings", row{"days": 28, "enabled": false}, p, "", 200)
	if v := string(f.req("GET", "/archives/purge/preview?older_than_days=30", nil, p, "", 200)); !strings.Contains(v, `"count":1`) {
		t.Fatal(v)
	}
	f.req("POST", "/archives/purge", row{"older_than_days": 30}, p, "", 200)
	f.req("GET", fmt.Sprintf("/archives/%d", head), nil, p, "", 404)
}
func TestLibraryMountZipAndSharedTrash(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	mount := t.TempDir()
	t.Setenv("BAMBASTIC_EXTERNAL_ROOTS", mount)
	if e := os.Mkdir(filepath.Join(mount, "designs"), 0700); e != nil {
		t.Fatal(e)
	}
	os.WriteFile(filepath.Join(mount, "designs", "part.gcode"), []byte("G1 X2\n"), 0600)
	f.req("POST", "/library/folders/external", row{"external_path": mount, "name": "Gallery", "readonly": true}, p, "", 200)
	out := string(f.req("POST", "/library/folders/1/scan", nil, p, "", 200))
	if !strings.Contains(out, `"added":1`) {
		t.Fatal(out)
	}
	out = string(f.req("POST", "/library/folders/1/scan", nil, p, "", 200))
	if !strings.Contains(out, `"added":0`) {
		t.Fatal(out)
	}
	os.Symlink(f.s.dir, filepath.Join(mount, "outside"))
	f.req("POST", "/library/folders/external", row{"external_path": filepath.Join(mount, "outside"), "name": "Private"}, p, "", 400)
	upload := func(name string, code int) row {
		var data bytes.Buffer
		z := zip.NewWriter(&data)
		member, _ := z.Create(name)
		io.WriteString(member, "G1 X2\n")
		z.Close()
		var b bytes.Buffer
		form := multipart.NewWriter(&b)
		part, _ := form.CreateFormFile("file", "input.zip")
		part.Write(data.Bytes())
		form.Close()
		r := httptest.NewRequest("POST", "/api/v1/library/files/extract-zip", &b)
		r.Header.Set("Content-Type", form.FormDataContentType())
		r = r.WithContext(access.With(r.Context(), p))
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r)
		if w.Code != code {
			t.Fatal(w.Code, w.Body.String())
		}
		var v row
		json.Unmarshal(w.Body.Bytes(), &v)
		return v
	}
	upload("../escape.gcode", 422)
	if v := upload("Models/copy.gcode", 200); num(v["extracted"]) != 1 {
		t.Fatal(v)
	}
	var file int64
	if e := f.s.db.QueryRow("SELECT id FROM library_files WHERE filename='copy.gcode'").Scan(&file); e != nil {
		t.Fatal(e)
	}
	f.req("PUT", "/library/trash/settings", row{"retention_days": 7, "auto_purge_days": 30}, p, "", 200)
	f.req("DELETE", fmt.Sprintf("/library/files/%d", file), nil, p, "", 200)
	f.req("DELETE", fmt.Sprintf("/library/trash/%d", file), nil, p, "", 200)
	if e := f.s.reclaimLibrary(context.Background()); e != nil {
		t.Fatal(e)
	}
	var rel string
	if e := f.s.db.QueryRow("SELECT file_path FROM library_files WHERE filename='part.gcode'").Scan(&rel); e != nil {
		t.Fatal(e)
	}
	if b, e := os.ReadFile(filepath.Join(f.s.dir, rel)); e != nil || string(b) != "G1 X2\n" {
		t.Fatal("shared content was reclaimed", e)
	}
}
