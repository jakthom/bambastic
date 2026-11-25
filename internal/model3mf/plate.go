package model3mf

import (
	"archive/zip"
	"bufio"
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"sort"
)

type Part struct {
	Name   string
	Mesh   *Mesh
	Copies int
}
type placement struct {
	part  int
	delta point
}

func layout(parts []Part) ([]placement, error) {
	var locations []placement
	var area, widest float64
	for i, p := range parts {
		if p.Mesh == nil || p.Mesh.Faces() == 0 || p.Copies < 1 || p.Copies > 100-len(locations) {
			return nil, errors.New("select 1 to 100 model instances")
		}
		w, d := float64(p.Mesh.hi[0]-p.Mesh.lo[0]), float64(p.Mesh.hi[1]-p.Mesh.lo[1])
		area += (w + 5) * (d + 5) * float64(p.Copies)
		widest = max(widest, w)
		for range p.Copies {
			locations = append(locations, placement{part: i})
		}
	}
	if len(locations) == 0 {
		return nil, errors.New("no models selected")
	}
	limit := float32(max(widest, math.Sqrt(area)))
	order := make([]int, len(locations))
	for i := range order {
		order[i] = i
	}
	depth := func(i int) float32 { m := parts[locations[i].part].Mesh; return m.hi[1] - m.lo[1] }
	sort.SliceStable(order, func(i, j int) bool { return depth(order[i]) > depth(order[j]) })
	var x, y, deep, width float32
	for _, i := range order {
		m := parts[locations[i].part].Mesh
		w, d := m.hi[0]-m.lo[0], depth(i)
		if x > 0 && x+w > limit {
			y += deep + 5
			x = 0
			deep = 0
		}
		locations[i].delta = point{x - m.lo[0], y - m.lo[1], -m.lo[2]}
		width = max(width, x+w)
		deep = max(deep, d)
		x += w + 5
	}
	for i := range locations {
		locations[i].delta[0] -= width / 2
		locations[i].delta[1] -= (y + deep) / 2
	}
	return locations, nil
}

type screen struct{ x, y, depth float64 }

func project(p, offset point) screen {
	x, y, z := float64(p[0])+float64(offset[0]), float64(p[1])+float64(offset[1]), float64(p[2])+float64(offset[2])
	return screen{x - y, 0.40*(x+y) - z, float64(x+y) + 0.8*z}
}
func Thumbnail(parts []Part) ([]byte, error) {
	placements, e := layout(parts)
	if e != nil {
		return nil, e
	}
	const edge = 256
	left, top, right, bottom := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, loc := range placements {
		m := parts[loc.part].Mesh
		for n := 0; n < 8; n++ {
			p := m.lo
			for i := range p {
				if n&(1<<i) != 0 {
					p[i] = m.hi[i]
				}
			}
			v := project(p, loc.delta)
			left = min(left, v.x)
			right = max(right, v.x)
			top = min(top, v.y)
			bottom = max(bottom, v.y)
		}
	}
	scale := (edge - 16) / max(right-left, bottom-top, 1e-6)
	cx, cy := (right+left)/2, (top+bottom)/2
	pixels := image.NewRGBA(image.Rect(0, 0, edge, edge))
	depth := make([]float64, edge*edge)
	for i := range depth {
		depth[i] = math.Inf(-1)
	}
	for _, loc := range placements {
		m := parts[loc.part].Mesh
		step := max(1, (len(m.faces)*len(placements)+29999)/30000)
		for i := 0; i < len(m.faces); i += step {
			f := m.faces[i]
			var v [3]screen
			for j, idx := range f {
				v[j] = project(m.vertices[idx], loc.delta)
				v[j].x = (v[j].x-cx)*scale + edge/2
				v[j].y = (v[j].y-cy)*scale + edge/2
			}
			a, b, c := v[0], v[1], v[2]
			area := (b.y-c.y)*(a.x-c.x) + (c.x-b.x)*(a.y-c.y)
			if math.Abs(area) < 1e-8 {
				continue
			}
			p, q, r := m.vertices[f[0]], m.vertices[f[1]], m.vertices[f[2]]
			ux, uy, uz := float64(q[0]-p[0]), float64(q[1]-p[1]), float64(q[2]-p[2])
			vx, vy, vz := float64(r[0]-p[0]), float64(r[1]-p[1]), float64(r[2]-p[2])
			nx, ny, nz := uy*vz-uz*vy, uz*vx-ux*vz, ux*vy-uy*vx
			light := 0.5 + 0.5*math.Abs((0.3*nx-0.4*ny+0.866*nz)/max(1e-20, math.Sqrt(nx*nx+ny*ny+nz*nz)))
			fill := color.RGBA{uint8(45 * light), uint8(185 * light), uint8(213 * light), 255}
			for y := max(0, int(math.Floor(min(a.y, b.y, c.y)))); y <= min(edge-1, int(math.Ceil(max(a.y, b.y, c.y)))); y++ {
				for x := max(0, int(math.Floor(min(a.x, b.x, c.x)))); x <= min(edge-1, int(math.Ceil(max(a.x, b.x, c.x)))); x++ {
					dx, dy := float64(x)+0.5-c.x, float64(y)+0.5-c.y
					u := ((b.y-c.y)*dx + (c.x-b.x)*dy) / area
					w := ((c.y-a.y)*dx + (a.x-c.x)*dy) / area
					t := 1 - u - w
					if min(u, w, t) < 0 {
						continue
					}
					z := u*a.depth + w*b.depth + t*c.depth
					k := y*edge + x
					if z >= depth[k] {
						depth[k] = z
						pixels.SetRGBA(x, y, fill)
					}
				}
			}
		}
	}
	var buf bytes.Buffer
	e = png.Encode(&buf, pixels)
	return buf.Bytes(), e
}

func Write(w io.Writer, parts []Part, preview []byte) error {
	loc, e := layout(parts)
	if e != nil {
		return e
	}
	z := zip.NewWriter(w)
	add := func(name, body string) error {
		out, e := z.Create(name)
		if e != nil {
			return e
		}
		_, e = io.WriteString(out, body)
		return e
	}
	if e = add("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="model" ContentType="application/vnd.ms-package.3dmanufacturing-3dmodel+xml"/><Default Extension="png" ContentType="image/png"/></Types>`); e != nil {
		return e
	}
	if e = add("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Target="/3D/3dmodel.model" Id="model" Type="http://schemas.microsoft.com/3dmanufacturing/2013/01/3dmodel"/><Relationship Target="/Metadata/thumbnail.png" Id="preview" Type="http://schemas.openxmlformats.org/package/2006/relationships/metadata/thumbnail"/></Relationships>`); e != nil {
		return e
	}
	out, e := z.Create("3D/3dmodel.model")
	if e != nil {
		return e
	}
	b := bufio.NewWriterSize(out, 64<<10)
	_, _ = io.WriteString(b, `<?xml version="1.0" encoding="UTF-8"?><model xmlns="http://schemas.microsoft.com/3dmanufacturing/core/2015/02" unit="millimeter"><resources>`)
	for i, p := range parts {
		fmt.Fprintf(b, `<object id="%d" type="model" name="`, i+1)
		if e = xml.EscapeText(b, []byte(p.Name)); e != nil {
			return e
		}
		io.WriteString(b, `"><mesh><vertices>`)
		for _, v := range p.Mesh.vertices {
			fmt.Fprintf(b, `<vertex x="%g" y="%g" z="%g"/>`, v[0], v[1], v[2])
		}
		io.WriteString(b, `</vertices><triangles>`)
		for _, v := range p.Mesh.faces {
			fmt.Fprintf(b, `<triangle v1="%d" v2="%d" v3="%d"/>`, v[0], v[1], v[2])
		}
		io.WriteString(b, `</triangles></mesh></object>`)
	}
	io.WriteString(b, `</resources><build>`)
	for _, l := range loc {
		fmt.Fprintf(b, `<item objectid="%d" transform="1 0 0 0 1 0 0 0 1 %g %g %g"/>`, l.part+1, l.delta[0], l.delta[1], l.delta[2])
	}
	io.WriteString(b, `</build></model>`)
	if e = b.Flush(); e != nil {
		return e
	}
	out, e = z.Create("Metadata/thumbnail.png")
	if e != nil {
		return e
	}
	if _, e = out.Write(preview); e != nil {
		return e
	}
	return z.Close()
}
