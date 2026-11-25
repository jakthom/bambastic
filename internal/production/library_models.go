package production

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/png"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"bambastic/internal/access"
	"bambastic/internal/model3mf"
	"golang.org/x/image/draw"
)

// One mesh job at a time bounds resident geometry independently of HTTP concurrency.
var meshWorker = make(chan struct{}, 1)

func withMeshWorker(ctx context.Context, f func() error) error {
	select {
	case meshWorker <- struct{}{}:
		defer func() { <-meshWorker }()
		return f()
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (s *Service) modelRoutes(rt register) {
	rt("POST", "/library/files/combine", s.combineModels)
	rt("POST", "/library/files/{id}/preview-thumbnail", s.previewThumbnail)
	rt("POST", "/library/generate-stl-thumbnails", s.generateThumbnails)
}
func (s *Service) combineModels(w http.ResponseWriter, r *http.Request) (any, error) {
	if e := permitted(r, "library:upload"); e != nil {
		return nil, e
	}
	ctx := r.Context()
	p := access.From(ctx)
	v, e := input(w, r)
	if e != nil {
		return nil, e
	}
	name, ok := v["filename"].(string)
	name = strings.TrimSpace(name)
	if !ok || name == "" || strings.HasPrefix(name, ".") || strings.ContainsAny(name, "/\\:") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, fail(400, "Invalid filename")
	}
	if !strings.HasSuffix(strings.ToLower(name), ".3mf") {
		name += ".3mf"
	}
	if utf8.RuneCountInString(name) > 255 {
		return nil, fail(400, "Filename too long")
	}
	folder := num(v["folder_id"])
	if v["folder_id"] != nil && folder < 1 {
		return nil, fail(422, "Invalid folder")
	}
	if e = s.checkLibraryDestination(ctx, s.db, p, folder, true); e != nil {
		return nil, e
	}
	items, ok := v["items"].([]any)
	if !ok || len(items) == 0 || len(items) > 100 {
		return nil, fail(422, "Select 1 to 100 models")
	}
	sourceIDs := []int64{}
	copies := map[int64]int{}
	total := 0
	for _, one := range items {
		item, ok := one.(map[string]any)
		k := num(item["file_id"])
		n := int64(1)
		if item["copies"] != nil {
			n = num(item["copies"])
		}
		if !ok || k < 1 || n < 1 || n > 100 {
			return nil, fail(422, "Invalid model or copy count")
		}
		total += int(n)
		if total > 100 {
			return nil, fail(400, "At most 100 instances per plate")
		}
		if copies[k] == 0 {
			sourceIDs = append(sourceIDs, k)
		}
		copies[k] += int(n)
	}
	sources := make([]row, len(sourceIDs))
	for i, k := range sourceIDs {
		sources[i], e = s.getAsset(ctx, s.db, p, "library", k, "read")
		if e != nil {
			return nil, e
		}
	}
	temp, e := os.CreateTemp(filepath.Join(s.dir, "production"), "combined-*.3mf")
	if e != nil {
		return nil, e
	}
	defer os.Remove(temp.Name())
	defer temp.Close()
	var preview []byte
	e = withMeshWorker(ctx, func() error {
		var parts []model3mf.Part
		remaining := model3mf.MaxFaces
		var sourceBytes int64
		for _, src := range sources {
			if err := ctx.Err(); err != nil {
				return err
			}
			disk, err := s.assetPath(src["file_path"])
			if err != nil {
				return err
			}
			st, err := os.Stat(disk)
			if errors.Is(err, os.ErrNotExist) {
				return fail(404, "Source file missing on disk")
			}
			if err != nil {
				return err
			}
			sourceBytes += st.Size()
			if sourceBytes > model3mf.MaxBytes {
				return fail(400, "Combined sources exceed 300 MiB")
			}
			m, err := model3mf.Read(disk, val(src["file_type"]), remaining)
			if err != nil {
				return fail(400, err.Error())
			}
			remaining -= m.Faces()
			parts = append(parts, model3mf.Part{Name: val(src["filename"]), Mesh: m, Copies: copies[num(src["id"])]})
		}
		var err error
		preview, err = model3mf.Thumbnail(parts)
		if err != nil {
			return err
		}
		return model3mf.Write(temp, parts, preview)
	})
	if e != nil {
		return nil, e
	}
	if _, e = temp.Seek(0, io.SeekStart); e != nil {
		return nil, e
	}
	result, e := s.prepareAsset(temp, "library", name)
	if e != nil {
		return nil, e
	}
	thumb, e := s.prepareAsset(bytes.NewReader(preview), "library", "preview.png")
	if e != nil {
		return nil, e
	}
	result["thumbnail_path"] = thumb["file_path"]
	result["source_type"] = "combined"
	result["folder_id"] = id(folder)
	result["created_by_id"] = id(p.ID)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return nil, e
	}
	var edits externalEdits
	defer edits.finish(s)
	defer tx.Rollback()
	if p.ID > 0 {
		var active bool
		if e = tx.QueryRowContext(ctx, "SELECT is_active FROM users WHERE id=?", p.ID).Scan(&active); e != nil || !active {
			return nil, fail(403, "Account is no longer active")
		}
	}
	if e = s.checkLibraryDestination(ctx, tx, p, folder, true); e != nil {
		return nil, e
	}
	for i, k := range sourceIDs {
		latest, err := s.getAsset(ctx, tx, p, "library", k, "read")
		if err != nil {
			return nil, err
		}
		if latest["file_hash"] != sources[i]["file_hash"] {
			return nil, fail(409, "Source file changed during combine")
		}
	}
	k, e := edits.insertLibrary(ctx, s, tx, result, folder)
	if e != nil {
		return nil, e
	}
	if e = tx.Commit(); e != nil {
		return nil, e
	}
	return row{"id": k, "filename": name, "file_size": result["file_size"], "file_type": "3mf", "thumbnail_path": result["thumbnail_path"], "metadata": unpack(result["file_metadata"]), "duplicate_of": nil}, nil
}

func scalePNG(data []byte) ([]byte, error) {
	cfg, e := png.DecodeConfig(bytes.NewReader(data))
	if e != nil {
		return nil, fail(400, "Invalid PNG image")
	}
	if max(cfg.Width, cfg.Height) > 2048 {
		return nil, fail(400, "Thumbnail dimensions exceed 2048 pixels")
	}
	img, e := png.Decode(bytes.NewReader(data))
	if e != nil {
		return nil, fail(400, "Invalid PNG pixels")
	}
	if longest := max(cfg.Width, cfg.Height); longest > 512 {
		ratio := 512 / float64(longest)
		dst := image.NewRGBA(image.Rect(0, 0, max(1, int(math.Round(float64(cfg.Width)*ratio))), max(1, int(math.Round(float64(cfg.Height)*ratio)))))
		draw.CatmullRom.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Src, nil)
		img = dst
	}
	var buf bytes.Buffer
	e = png.Encode(&buf, img)
	return buf.Bytes(), e
}
func thumbnailPermission(p access.Principal) bool {
	return p.Has("library:update_all") || p.Has("library:update_own")
}
func (s *Service) previewThumbnail(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !thumbnailPermission(p) {
		return nil, fail(403, "Permission denied")
	}
	src, e := s.getAsset(ctx, s.db, p, "library", pathID(r), "update")
	if e != nil {
		return nil, e
	}
	switch val(src["file_type"]) {
	case "step", "stp", "pdf", "csv", "xlsx", "ods":
	default:
		return nil, fail(400, "File type does not accept client-rendered thumbnails")
	}
	if val(src["thumbnail_path"]) != "" {
		return row{"updated": false}, nil
	}
	r.Body = http.MaxBytesReader(w, r.Body, (2<<20)+(128<<10))
	if e = r.ParseMultipartForm(2 << 20); e != nil {
		var limit *http.MaxBytesError
		if errors.As(e, &limit) {
			return nil, fail(413, "Thumbnail exceeds 2 MiB")
		}
		return nil, fail(422, "Invalid thumbnail upload")
	}
	defer r.MultipartForm.RemoveAll()
	f, _, e := r.FormFile("thumbnail")
	if e != nil {
		return nil, fail(422, "Thumbnail file required")
	}
	defer f.Close()
	data, e := io.ReadAll(io.LimitReader(f, (2<<20)+1))
	if e != nil {
		return nil, e
	}
	if len(data) > 2<<20 {
		return nil, fail(413, "Thumbnail exceeds 2 MiB")
	}
	data, e = scalePNG(data)
	if e != nil {
		return nil, e
	}
	updated, e := s.storeThumbnail(ctx, p, src, data, true)
	return row{"updated": updated}, e
}
func (s *Service) storeThumbnail(ctx context.Context, p access.Principal, src row, data []byte, missing bool) (bool, error) {
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return false, e
	}
	defer tx.Rollback()
	latest, e := s.getAsset(ctx, tx, p, "library", num(src["id"]), "update")
	if e != nil {
		return false, e
	}
	if missing && val(latest["thumbnail_path"]) != "" {
		return false, nil
	}
	if val(src["file_path"]) != val(latest["file_path"]) {
		return false, fail(409, "File changed during thumbnail generation")
	}
	asset, e := s.prepareAsset(bytes.NewReader(data), "library", "preview.png")
	if e != nil {
		return false, e
	}
	q := "UPDATE library_files SET thumbnail_path=? WHERE id=? AND deleted_at IS NULL"
	if missing {
		q += " AND thumbnail_path IS NULL"
	}
	res, e := tx.ExecContext(ctx, q, asset["file_path"], src["id"])
	if e != nil {
		return false, e
	}
	n, e := res.RowsAffected()
	if e != nil {
		return false, e
	}
	return n > 0, tx.Commit()
}
func pdfPreview(ctx context.Context, src string) ([]byte, error) {
	program, e := exec.LookPath("pdftoppm")
	if e != nil {
		return nil, errors.New("PDF thumbnails require Poppler pdftoppm")
	}
	tmp, e := os.MkdirTemp("", "bambastic-pdf-*")
	if e != nil {
		return nil, e
	}
	defer os.RemoveAll(tmp)
	prefix := filepath.Join(tmp, "preview")
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if e = exec.CommandContext(ctx, program, "-f", "1", "-l", "1", "-singlefile", "-scale-to", "256", "-png", src, prefix).Run(); e != nil {
		return nil, errors.New("Could not render the first PDF page")
	}
	return os.ReadFile(prefix + ".png")
}
func (s *Service) generateThumbnails(w http.ResponseWriter, r *http.Request) (any, error) {
	ctx := r.Context()
	p := access.From(ctx)
	if !thumbnailPermission(p) {
		return nil, fail(403, "Permission denied")
	}
	v, e := input(w, r)
	if e != nil {
		return nil, e
	}
	ids, e := idsOf(v["file_ids"])
	if e != nil {
		return nil, e
	}
	folder := num(v["folder_id"])
	q := "SELECT * FROM library_files WHERE deleted_at IS NULL AND file_type IN ('stl','pdf')"
	args := []any{}
	switch {
	case len(ids) > 0:
		q += " AND id IN (" + strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",") + ")"
		for _, id := range ids {
			args = append(args, id)
		}
	case v["folder_id"] != nil:
		if folder < 1 {
			return nil, fail(422, "Invalid folder")
		}
		q += " AND folder_id=?"
		args = append(args, folder)
		if flag(v["all_missing"]) {
			q += " AND thumbnail_path IS NULL"
		}
	case flag(v["all_missing"]):
		q += " AND thumbnail_path IS NULL"
	default:
		q += " AND 0"
	}
	if !p.Has("library:update_all") {
		q += " AND created_by_id=?"
		args = append(args, p.ID)
	}
	rows, e := records(ctx, s.db, q+" ORDER BY id", args...)
	if e != nil {
		return nil, e
	}
	results := []row{}
	succeeded := 0
	for _, src := range rows {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		result := row{"file_id": src["id"], "filename": src["filename"], "success": false, "error": nil}
		err := withMeshWorker(ctx, func() error {
			path, e := s.assetPath(src["file_path"])
			if e != nil {
				return e
			}
			if _, e = os.Stat(path); e != nil {
				return errors.New("File not found on disk")
			}
			var img []byte
			if src["file_type"] == "pdf" {
				img, e = pdfPreview(ctx, path)
			} else {
				var mesh *model3mf.Mesh
				mesh, e = model3mf.Read(path, "stl", model3mf.MaxFaces)
				if e == nil {
					img, e = model3mf.Thumbnail([]model3mf.Part{{Mesh: mesh, Copies: 1}})
				}
			}
			if e != nil {
				return e
			}
			_, e = s.storeThumbnail(ctx, p, src, img, false)
			return e
		})
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["success"] = true
			succeeded++
		}
		results = append(results, result)
	}
	return row{"processed": len(results), "succeeded": succeeded, "failed": len(results) - succeeded, "results": results}, nil
}
