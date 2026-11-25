package production

import (
	"archive/zip"
	"bambastic/internal/access"
	"bytes"
	"encoding/json"
	"github.com/go-pdf/fpdf"
	"image"
	"image/color"
	"image/png"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

const triangleSTL = "solid geometry\nfacet normal 0 0 1\n outer loop\n vertex 0 0 0\n vertex 10 0 0\n vertex 0 10 0\n endloop\nendfacet\nendsolid\n"

func TestLibraryModelCombineAndPreview(t *testing.T) {
	f := farmFixture(t)
	p := f.admin
	f.file("/library/files", "bracket.stl", triangleSTL, p, 200)
	makeDesign := func(k int, filename string) row {
		t.Helper()
		var out row
		body := row{"filename": filename, "items": []any{row{"file_id": k, "copies": 2}, row{"file_id": k}}}
		if e := json.Unmarshal(f.req("POST", "/library/files/combine", body, p, "", 200), &out); e != nil {
			t.Fatal(e)
		}
		return out
	}
	combined := makeDesign(1, "assembly")
	if combined["filename"] != "assembly.3mf" || combined["thumbnail_path"] == nil || num(combined["id"]) != 2 {
		t.Fatal(combined)
	}
	data := f.req("GET", "/library/files/2/download", nil, p, "", 200)
	z, e := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if e != nil {
		t.Fatal(e)
	}
	var found bool
	for _, member := range z.File {
		if member.Name == "3D/3dmodel.model" {
			r, _ := member.Open()
			b, e := io.ReadAll(r)
			r.Close()
			if e != nil || bytes.Count(b, []byte("<object ")) != 1 || bytes.Count(b, []byte("<item ")) != 3 {
				t.Fatal("invalid model", string(b))
			}
			found = true
		}
	}
	if !found {
		t.Fatal("model missing")
	}
	if _, e = png.Decode(bytes.NewReader(f.req("GET", "/library/files/2/thumbnail", nil, p, "", 200))); e != nil {
		t.Fatal(e)
	}
	nested := makeDesign(2, "combined again")
	if num(nested["id"]) != 3 {
		t.Fatal(nested)
	}
	f.file("/library/files", "sliced.gcode", "; slicer output", p, 200)
	f.req("POST", "/library/files/combine", row{"filename": "no", "items": []any{row{"file_id": 4}}}, p, "", 400)
	f.req("POST", "/library/files/combine", row{"filename": "../nested", "items": []any{row{"file_id": 1}}}, p, "", 400)
	f.req("POST", "/library/files/combine", row{"filename": "no", "items": []any{row{"file_id": 1, "copies": 100}, row{"file_id": 1}}}, p, "", 400)
	other := access.Principal{ID: 92, Permissions: map[string]bool{"library:upload": true, "library:read_own": true}}
	f.req("POST", "/library/files/combine", row{"filename": "unavailable", "items": []any{row{"file_id": 1}}}, other, "", 404)
	f.file("/library/files", "invalid.stl", "invalid", p, 200)
	var res row
	json.Unmarshal(f.req("POST", "/library/generate-stl-thumbnails", row{"file_ids": []int{1, 5}}, p, "", 200), &res)
	if num(res["processed"]) != 2 || num(res["succeeded"]) != 1 || num(res["failed"]) != 1 {
		t.Fatal(res)
	}
	f.req("GET", "/library/files/1/thumbnail", nil, p, "", 200)
	f.file("/library/files", "data.csv", "measurements\n2\n", p, 200)
	img := image.NewRGBA(image.Rect(0, 0, 1024, 128))
	img.SetRGBA(7, 7, color.RGBA{0, 100, 200, 255})
	var pixels bytes.Buffer
	png.Encode(&pixels, img)
	send := func(data []byte, actor access.Principal, code int, match string) {
		t.Helper()
		var b bytes.Buffer
		form := multipart.NewWriter(&b)
		part, _ := form.CreateFormFile("thumbnail", "thumbnail.png")
		part.Write(data)
		form.Close()
		r := httptest.NewRequest("POST", "/api/v1/library/files/6/preview-thumbnail", &b)
		r.Header.Set("Content-Type", form.FormDataContentType())
		w := httptest.NewRecorder()
		f.mux.ServeHTTP(w, r.WithContext(access.With(r.Context(), actor)))
		if w.Code != code || !strings.Contains(w.Body.String(), match) {
			t.Fatalf("%d %s", w.Code, w.Body)
		}
	}
	send(pixels.Bytes(), other, 403, "")
	send([]byte("invalid"), p, 400, "PNG")
	send(pixels.Bytes(), p, 200, `"updated":true`)
	send(pixels.Bytes(), p, 200, `"updated":false`)
	cfg, e := png.DecodeConfig(bytes.NewReader(f.req("GET", "/library/files/6/thumbnail", nil, p, "", 200)))
	if e != nil || cfg.Width != 512 || cfg.Height != 64 {
		t.Fatal(cfg, e)
	}
}

func TestPDFFirstPageThumbnail(t *testing.T) {
	if _, e := exec.LookPath("pdftoppm"); e != nil {
		t.Skip("Poppler is optional in native installs")
	}
	f := farmFixture(t)
	doc := fpdf.New("P", "mm", "A4", "")
	doc.AddPage()
	doc.SetFont("Helvetica", "", 16)
	doc.Text(15, 20, "Assembly drawing")
	var b bytes.Buffer
	if e := doc.Output(&b); e != nil {
		t.Fatal(e)
	}
	f.file("/library/files", "plan.pdf", b.String(), f.admin, 200)
	got := f.req("POST", "/library/generate-stl-thumbnails", row{"all_missing": true}, f.admin, "", 200)
	if !bytes.Contains(got, []byte(`"succeeded":1`)) {
		t.Fatal(string(got))
	}
	cfg, e := png.DecodeConfig(bytes.NewReader(f.req("GET", "/library/files/1/thumbnail", nil, f.admin, "", 200)))
	if e != nil || max(cfg.Width, cfg.Height) != 256 {
		t.Fatal(cfg, e)
	}
}
