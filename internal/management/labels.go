package management

import (
	"archive/zip"
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/skip2/go-qrcode"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed labelfonts/*.ttf
var labelFonts embed.FS
var loadLabelFonts = sync.OnceValues(func() ([]*opentype.Font, error) {
	var fonts []*opentype.Font
	for _, name := range []string{"LiberationSans-Regular.ttf", "LiberationSans-Bold.ttf", "DroidSansFallbackFull.ttf", "NotoSansKR-Hangul.ttf"} {
		b, e := labelFonts.ReadFile("labelfonts/" + name)
		if e != nil {
			return nil, e
		}
		f, e := opentype.Parse(b)
		if e != nil {
			return nil, e
		}
		fonts = append(fonts, f)
	}
	return fonts, nil
})

type labelSheet struct {
	pw, ph, w, h, x, y, gx, gy float64
	cols, rows                 int
}

func sheet(d row) (labelSheet, error) {
	name := str(d["template"])
	w, h := 0., 0.
	var l labelSheet
	switch name {
	case "ams_holder_74x33":
		w, h = 74, 33
	case "ams_holder_75x55":
		w, h = 75, 55
	case "box_40x30":
		w, h = 40, 30
	case "box_62x29":
		w, h = 62, 29
	case "avery_5160":
		l = labelSheet{215.9, 279.4, 66.675, 25.4, 4.76, 12.7, 3.175, 0, 3, 10}
	case "avery_l7160":
		l = labelSheet{210, 297, 63.5, 38.1, 7, 15.15, 2.5, 0, 3, 7}
	case "custom":
		v := sub(d["layout"])
		if v["page"] == "letter" {
			l.pw, l.ph = 215.9, 279.4
		} else if v["page"] == "a4" {
			l.pw, l.ph = 210, 297
		} else {
			return l, invalid("Unknown sheet size")
		}
		l.cols, l.rows = int(num(v["cols"])), int(num(v["rows"]))
		l.w, l.h, l.x, l.y, l.gx, l.gy = num(v["label_w_mm"]), num(v["label_h_mm"]), num(v["left_mm"]), num(v["top_mm"]), num(v["col_gap_mm"]), num(v["row_gap_mm"])
		if l.cols < 1 || l.rows < 1 || l.cols > 50 || l.rows > 50 || l.w < 5 || l.h < 5 || l.x < 0 || l.y < 0 || l.gx < 0 || l.gy < 0 || l.x+float64(l.cols)*l.w+float64(l.cols-1)*l.gx > l.pw+0.25 || l.y+float64(l.rows)*l.h+float64(l.rows-1)*l.gy > l.ph+0.25 {
			return l, invalid("Label grid exceeds the sheet")
		}
	default:
		return l, invalid("Unknown label template")
	}
	if name != "custom" && d["layout"] != nil {
		return l, invalid("Only custom templates accept a layout")
	}
	if w > 0 {
		l = labelSheet{pw: w, ph: h, w: w, h: h, cols: 1, rows: 1}
	}
	return l, nil
}

var labelFields = strings.Fields("brand material hex name location material_number temps weight note added qr spool_id")

func labelText(v row, k string) string {
	switch k {
	case "brand", "note", "material_number":
		return str(v[k])
	case "material":
		return strings.TrimSpace(str(v["material"]) + " " + str(v["subtype"]))
	case "hex":
		h := strings.TrimPrefix(str(v["rgba"]), "#")
		if len(h) >= 6 {
			return "#" + strings.ToUpper(h[:6])
		}
	case "name":
		for _, col := range []string{"color_name", "slicer_filament_name", "material"} {
			if s := str(v[col]); s != "" {
				return s
			}
		}
	case "location":
		return str(v["storage_location"])
	case "temps":
		lo, hi := str(v["nozzle_temp_min"]), str(v["nozzle_temp_max"])
		if lo != "" && hi != "" {
			return lo + "–" + hi + " °C"
		}
	case "weight":
		if v["label_weight"] != nil {
			return str(v["label_weight"]) + " g"
		}
	case "added":
		date := str(v["created_at"])
		if len(date) >= 10 {
			return date[:10]
		}
	case "spool_id":
		return "#" + str(v["id"])
	}
	return ""
}
func renderCell(v row, ww, hh int, fields map[string]bool, mono bool, base string) (*image.RGBA, error) {
	img := image.NewRGBA(image.Rect(0, 0, ww, hh))
	draw.Draw(img, img.Bounds(), image.White, image.Point{}, draw.Src)
	pad := max(2, hh/24)
	x, y := pad, pad
	right := ww - pad
	if !mono {
		colors := strings.Split(strings.Join([]string{str(v["rgba"]), str(v["extra_colors"])}, ","), ",")
		active := []color.Color{}
		for _, h := range colors {
			if c, ok := csvRGBA(strings.TrimSpace(h)); ok {
				var r, g, b uint8
				if _, e := fmt.Sscanf(c[:6], "%02x%02x%02x", &r, &g, &b); e == nil {
					active = append(active, color.RGBA{r, g, b, 255})
				}
			}
		}
		for i, c := range active {
			draw.Draw(img, image.Rect(x, pad+i*(hh-2*pad)/len(active), x+pad, (i+1)*(hh-2*pad)/len(active)+pad), image.NewUniform(c), image.Point{}, draw.Src)
		}
		if len(active) > 0 {
			x += 2 * pad
		}
	}
	if fields["qr"] {
		size := min(hh-2*pad, ww*38/100)
		code, e := qrcode.New(base+"/inventory?spool="+str(v["id"]), qrcode.Medium)
		if e != nil {
			return nil, e
		}
		draw.Draw(img, image.Rect(right-size, (hh-size)/2, right, (hh+size)/2), code.Image(size), image.Point{}, draw.Src)
		right -= size + pad
	}
	type line struct {
		s    string
		bold bool
	}
	lines := []line{}
	for _, k := range labelFields {
		if fields[k] && k != "qr" {
			if text := strings.TrimSpace(labelText(v, k)); text != "" {
				lines = append(lines, line{text, k == "name" || k == "material"})
			}
		}
	}
	if len(lines) == 0 {
		return img, nil
	}
	baseFonts, e := loadLabelFonts()
	if e != nil {
		return nil, e
	}
	lineHeight := min((hh-2*pad)/len(lines), max(9, hh/5))
	size := max(4, float64(lineHeight)*.77)
	faces := make([]font.Face, len(baseFonts))
	for n, f := range baseFonts {
		face, e := opentype.NewFace(f, &opentype.FaceOptions{Size: size, DPI: 72, Hinting: font.HintingFull})
		if e != nil {
			return nil, e
		}
		faces[n] = face
		defer face.Close()
	}
	y += (hh - 2*pad - lineHeight*len(lines)) / 2
	for _, l := range lines {
		pen := fixed.I(x)
		for _, ch := range l.s {
			f := faces[0]
			if l.bold {
				f = faces[1]
			}
			advance, ok := f.GlyphAdvance(ch)
			if !ok {
				for _, fallback := range faces[2:] {
					if a, ok := fallback.GlyphAdvance(ch); ok {
						f = fallback
						advance = a
						break
					}
				}
			}
			if pen+advance > fixed.I(right) {
				break
			}
			drawer := font.Drawer{Dst: img, Src: image.Black, Face: f, Dot: fixed.Point26_6{X: pen, Y: fixed.I(y + int(size))}}
			drawer.DrawString(string(ch))
			pen += advance
		}
		y += lineHeight
	}
	return img, nil
}

var labelJobs = make(chan struct{}, 2)

func (s *Service) labelRoutes(m *http.ServeMux) {
	for _, backend := range []string{"inventory", "spoolman"} {
		for _, preview := range []bool{false, true} {
			path := backend + "/labels"
			if preview {
				path += "/preview"
			}
			s.route(m, "POST", path, inventoryRead, func(r request) (any, error) {
				select {
				case labelJobs <- struct{}{}:
					defer func() { <-labelJobs }()
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				d, e := r.body()
				if e != nil {
					return nil, e
				}
				layout, e := sheet(d)
				if e != nil {
					return nil, e
				}
				format := str(d["format"])
				if format == "" {
					format = "pdf"
				}
				ids, _ := d["spool_ids"].([]any)
				start := int(num(d["starting_position"]))
				if start == 0 {
					start = 1
				}
				dpi := int(num(d["dpi"]))
				if dpi == 0 {
					dpi = 300
				}
				if preview {
					ids = []any{d["spool_id"]}
					format = "png"
					dpi = 300
					layout = labelSheet{pw: layout.w, ph: layout.h, w: layout.w, h: layout.h, cols: 1, rows: 1}
				}
				cap := layout.cols * layout.rows
				pages := (len(ids)+start-2)/cap + 1
				if len(ids) < 1 || len(ids) > 500 || start < 1 || start > cap || format != "pdf" && format != "png" || dpi != 203 && dpi != 300 && dpi != 600 || format == "png" && cap > 1 && pages*dpi > 15000 {
					return nil, invalid("Unsupported label output or sheet capacity")
				}
				fields := map[string]bool{}
				if selected, ok := d["fields"].([]any); ok {
					for _, f := range selected {
						if !slices.Contains(labelFields, str(f)) {
							return nil, invalid("Unknown label field")
						}
						fields[str(f)] = true
					}
				} else {
					for _, k := range strings.Fields("brand material hex name location qr spool_id") {
						fields[k] = true
					}
				}
				base := strings.TrimRight(setting(r.Context(), r.tx, "external_url", ""), "/")
				if u, e := url.Parse(base); e != nil || u.Hostname() == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
					scheme := "http"
					if r.TLS != nil {
						scheme = "https"
					}
					base = scheme + "://" + r.Host
				}
				data := []row{}
				loaded := map[int64]row{}
				for _, k := range ids {
					id := intID(k)
					if id < 1 {
						return nil, invalid("Invalid spool ID")
					}
					v := loaded[id]
					if v == nil {
						if backend == "inventory" {
							v, e = get(r.tx, "spool", id)
						} else {
							v, e = rawSpool(r, id)
							if e == nil {
								v = remoteSpool(v)
							}
						}
						if e != nil {
							return nil, e
						}
						loaded[id] = v
					}
					data = append(data, v)
				}
				pdf := fpdf.NewCustom(&fpdf.InitType{UnitStr: "mm", Size: fpdf.SizeType{Wd: layout.pw, Ht: layout.ph}})
				pdf.SetAutoPageBreak(false, 0)
				pdf.SetCreationDate(time.Now().UTC())
				var out bytes.Buffer
				zipper := zip.NewWriter(&out)
				scale := float64(dpi) / 25.4
				if format == "pdf" {
					scale = 300 / 25.4
				}
				px := func(v float64) int { return int(math.Round(v * scale)) }
				for page := 0; page < pages; page++ {
					if e := r.Context().Err(); e != nil {
						return nil, e
					}
					var pageImg *image.RGBA
					if format == "pdf" {
						pdf.AddPage()
					} else {
						pageImg = image.NewRGBA(image.Rect(0, 0, px(layout.pw), px(layout.ph)))
						draw.Draw(pageImg, pageImg.Bounds(), image.White, image.Point{}, draw.Src)
					}
					for slot := 0; slot < cap; slot++ {
						n := page*cap + slot - start + 1
						if n < 0 || n >= len(data) {
							continue
						}
						cell, e := renderCell(data[n], px(layout.w), px(layout.h), fields, truth(d["monochrome"]), base)
						if e != nil {
							return nil, e
						}
						x := layout.x + float64(slot%layout.cols)*(layout.w+layout.gx)
						y := layout.y + float64(slot/layout.cols)*(layout.h+layout.gy)
						if format == "pdf" {
							var encoded bytes.Buffer
							if e = png.Encode(&encoded, cell); e != nil {
								return nil, e
							}
							name := fmt.Sprintf("cell-%d", n)
							opt := fpdf.ImageOptions{ImageType: "PNG"}
							pdf.RegisterImageOptionsReader(name, opt, &encoded)
							pdf.ImageOptions(name, x, y, layout.w, layout.h, false, opt, 0, "")
						} else {
							rect := cell.Bounds().Add(image.Pt(px(x), px(y)))
							draw.Draw(pageImg, rect, cell, image.Point{}, draw.Src)
						}
					}
					if format == "png" {
						if pages == 1 {
							if e = png.Encode(&out, pageImg); e != nil {
								return nil, e
							}
							name := "bambastic-labels.png"
							if preview {
								name = ""
							}
							return download{"image/png", name, out.Bytes()}, nil
						}
						z, e := zipper.Create(fmt.Sprintf("labels-%03d.png", page+1))
						if e != nil {
							return nil, e
						}
						if e = png.Encode(z, pageImg); e != nil {
							return nil, e
						}
					}
				}
				if format == "pdf" {
					if e := pdf.Output(&out); e != nil {
						return nil, e
					}
					return download{"application/pdf", "bambastic-labels.pdf", out.Bytes()}, nil
				}
				if e := zipper.Close(); e != nil {
					return nil, e
				}
				return download{"application/zip", "bambastic-labels.zip", out.Bytes()}, nil
			})
		}
	}
}
