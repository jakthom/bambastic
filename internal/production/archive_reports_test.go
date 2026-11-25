package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestReportsUseVisibleRunAndQualityOutcomes(t *testing.T) {
	f := farmFixture(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, e := f.s.db.Exec(q, args...); e != nil {
			t.Fatal(e)
		}
	}
	exec("INSERT INTO users(id,username,password_hash,role,auth_source,is_active) VALUES(27,'report','x','user','local',1)")
	exec("INSERT INTO print_archives(filename,file_path,file_size,status,is_favorite,quantity,printer_id,created_by_id) VALUES('=formula.3mf','file.3mf',7,'completed',0,1,1,27)")
	for _, v := range [][]any{{"completed", "reject", "warped"}, {"failed", nil, "adhesion"}, {"cancelled", nil, nil}} {
		exec("INSERT INTO print_log_entries(archive_id,printer_id,print_name,created_by_id,status,user_verdict,failure_reason) VALUES(1,1,'shop part',27,?,?,?)", v...)
	}
	p := access.Principal{ID: 27, Permissions: map[string]bool{"archives:read_own": true}, PrinterIDs: map[int64]bool{1: true}}
	b := f.req("GET", "/archives/analysis/failures", nil, p, "", 200)
	var o row
	if json.Unmarshal(b, &o) != nil || num(o["total_prints"]) != 3 || num(o["failed_prints"]) != 1 || num(o["rejected_prints"]) != 1 || num(o["failure_rate"]) != 50 {
		t.Fatal(string(b))
	}
	b = f.req("GET", "/archives/last-per-printer", nil, p, "", 200)
	var list []row
	if json.Unmarshal(b, &list) != nil || len(list) != 1 || num(list[0]["run_id"]) != 3 || list[0]["status"] != "cancelled" {
		t.Fatal(string(b))
	}
	b = f.req("GET", "/archives/export?fields=filename,status", nil, p, "", 200)
	records, e := csv.NewReader(bytes.NewReader(b)).ReadAll()
	if e != nil || len(records) != 4 || records[1][0] != "'=formula.3mf" {
		t.Fatal(records, e)
	}
	b = f.req("GET", "/archives/export?format=xlsx&fields=filename", nil, p, "", 200)
	z, e := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if e != nil {
		t.Fatal(e)
	}
	for _, f := range z.File {
		if f.Name == "xl/worksheets/sheet1.xml" {
			r, e := f.Open()
			if e != nil {
				t.Fatal(e)
			}
			text, _ := io.ReadAll(r)
			r.Close()
			if strings.Contains(string(text), "<f>") || !strings.Contains(string(text), `t="inlineStr"`) {
				t.Fatal(string(text))
			}
		}
	}
	p.PrinterIDs = map[int64]bool{}
	b = f.req("GET", "/archives/export?fields=filename", nil, p, "", 200)
	if strings.Contains(string(b), "formula") {
		t.Fatal("restricted export leaked a filename")
	}
}
