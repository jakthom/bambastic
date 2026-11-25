package production

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"bambastic/internal/access"
)

func TestQualityPromptBindsRunAndRequiresDecision(t *testing.T) {
	f := farmFixture(t)
	ctx := t.Context()
	f.upload(f.admin)
	for i := 0; i < 2; i++ {
		f.req("POST", "/queue/", row{"archive_id": 1, "printer_id": 1, "plate_id": 2, "confirm_outcome": true}, f.admin, "", 200)
		j, e := f.s.claim(ctx, f.pr)
		if e != nil || j == nil {
			t.Fatal(j, e)
		}
		f.s.send(ctx, j)
		for _, st := range []string{"RUNNING", "FINISH"} {
			if e = f.s.observe(ctx, f.pr, row{"gcode_state": st, "gcode_file": f.hw.uploaded, "subtask_id": val(j["id"])}); e != nil {
				t.Fatal(e)
			}
		}
	}
	mux := http.NewServeMux()
	f.s.RegisterDownloads(mux, nil)
	var links row
	json.Unmarshal(f.req("GET", "/archives/1/runs/1/confirmation", nil, f.admin, "", 200), &links)
	get := func(method, url string) row {
		t.Helper()
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(method, "/api/v1/archives"+url, nil))
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		var m row
		json.Unmarshal(w.Body.Bytes(), &m)
		return m
	}
	if m := get("GET", val(links["reject_url"])); flag(m["answered"]) {
		t.Fatal("GET spent link", m)
	}
	if m := get("POST", val(links["reject_url"])); m["recorded_verdict"] != "reject" {
		t.Fatal(m)
	}
	if m := get("POST", val(links["good_url"])); m["recorded_verdict"] != "reject" {
		t.Fatal("link changed decision", m)
	}
	latest, e := one(ctx, f.s.db, "SELECT * FROM print_archives WHERE id=1")
	if e != nil || latest["user_verdict"] != nil {
		t.Fatal("older decision copied onto archive", e, latest)
	}
	data := f.req("GET", "/print-log/", nil, f.admin, "", 200)
	if bytes.Contains(data, []byte(`"confirm_token":`)) {
		t.Fatal("leaked confirmation capability")
	}
	f.req("PATCH", "/archives/1", row{"run_id": 1, "notes": "unrelated"}, f.admin, "", 400)
	f.req("PATCH", "/archives/1", row{"run_id": 2, "user_verdict": "good", "user_verdict_source": "link"}, f.admin, "", 422)
	f.req("PATCH", "/archives/1", row{"run_id": 2, "user_verdict": "good", "user_verdict_source": "dialog"}, access.Principal{Permissions: map[string]bool{"archives:read_all": true, "archives:update_all": true}, PrinterIDs: map[int64]bool{12: true}}, "", 404)
	f.req("PATCH", "/archives/1", row{"run_id": 2, "user_verdict": "good", "user_verdict_source": "dialog"}, f.admin, "", 200)
	latest, e = one(ctx, f.s.db, "SELECT * FROM print_archives WHERE id=1")
	if e != nil || latest["user_verdict"] != "good" || latest["user_verdict_source"] != "dialog" {
		t.Fatal(latest, e)
	}
	f.req("PATCH", "/archives/1", row{"run_id": 2, "user_verdict": nil}, f.admin, "", 200)
	latest, e = one(ctx, f.s.db, "SELECT * FROM print_archives WHERE id=1")
	if e != nil || latest["user_verdict"] != nil || latest["confirm_token_used_at"] == nil {
		t.Fatal(latest, e)
	}
	token := val(latest["confirm_token"])
	if len(token) != 64 || strings.Contains(val(links["good_url"]), token) {
		t.Fatal("capability was reused")
	}
}
