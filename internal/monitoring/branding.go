package monitoring

import (
	"bambastic/internal/access"
	"bambastic/internal/httpx"
	"bytes"
	"errors"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
	"image"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
)

func (s *Service) registerBranding(public, api *http.ServeMux) {
	for _, m := range []string{"GET", "POST", "DELETE"} {
		api.HandleFunc(m+" /api/v1/settings/overlay-logo", s.branding)
	}
	route := "GET /api/v1/overlay-branding/logo"
	h := s.withMedia("overlay", s.streamLogo)
	public.HandleFunc(route, h)
	if api != public {
		api.HandleFunc(route, h)
	}
}
func (s *Service) streamLogo(w http.ResponseWriter, r *http.Request) {
	if !access.Require(w, r, "camera:view") {
		return
	}
	s.readLogo(w, r)
}
func (s *Service) readLogo(w http.ResponseWriter, r *http.Request) {
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 503, "Cannot open branding storage")
		return
	}
	defer root.Close()
	f, e := root.Open("overlay-branding/logo.png")
	if e != nil {
		httpx.Error(w, 404, "No overlay logo saved")
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.Copy(w, f)
}
func scaled(img image.Image, maxSize int) *image.NRGBA {
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if n := max(width, height); n > maxSize {
		width = max(1, width*maxSize/n)
		height = max(1, height*maxSize/n)
	}
	dst := image.NewNRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(dst, dst.Bounds(), img, b, draw.Src, nil)
	return dst
}
func (s *Service) branding(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		if access.Require(w, r, "settings:read") {
			s.readLogo(w, r)
		}
		return
	}
	if !access.Require(w, r, "settings:update") {
		return
	}
	root, e := os.OpenRoot(s.dir)
	if e != nil {
		httpx.Error(w, 503, "Cannot open branding storage")
		return
	}
	defer root.Close()
	const target = "overlay-branding/logo.png"
	if r.Method == "DELETE" {
		e = root.Remove(target)
		if e != nil && !errors.Is(e, os.ErrNotExist) {
			httpx.Error(w, 500, "Cannot delete overlay logo")
			return
		}
		httpx.JSON(w, 200, object{"status": "ok"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 3<<20)
	f, _, e := r.FormFile("file")
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	if e != nil {
		httpx.Error(w, 400, "Choose a PNG or WebP file")
		return
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, 2<<20+1))
	if e != nil || len(b) > 2<<20 {
		httpx.Error(w, 413, "The logo exceeds two MiB")
		return
	}
	cfg, format, e := image.DecodeConfig(bytes.NewReader(b))
	if e != nil || (format != "webp" && format != "png") || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 4_000_000 {
		httpx.Error(w, 400, "Choose a PNG or WebP image of up to four million pixels")
		return
	}
	// RIFF ANIM / PNG acTL chunks mark animations, which cannot be represented by a static channel logo.
	if bytes.Contains(b, []byte("acTL")) || format == "webp" && bytes.Contains(b, []byte("ANIM")) {
		httpx.Error(w, 400, "Choose a static logo")
		return
	}
	img, _, e := image.Decode(bytes.NewReader(b))
	if e != nil {
		httpx.Error(w, 400, "Invalid logo")
		return
	}
	if e = root.MkdirAll("overlay-branding", 0700); e != nil {
		httpx.Error(w, 500, "Cannot create logo storage")
		return
	}
	key, _ := tokenSecret()
	name := filepath.Join("overlay-branding", key+".png")
	out, e := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		httpx.Error(w, 500, "Cannot write overlay logo")
		return
	}
	defer root.Remove(name)
	e = png.Encode(out, scaled(img, 512))
	closeErr := out.Close()
	if e != nil || closeErr != nil || root.Rename(name, target) != nil {
		httpx.Error(w, 500, "Cannot write overlay logo")
		return
	}
	httpx.JSON(w, 200, object{"status": "ok"})
}
