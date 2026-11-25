// Package model3mf reads printable geometry and creates a one-plate 3MF design.
package model3mf

import (
	"archive/zip"
	"bufio"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"strconv"
	"strings"
)

const MaxBytes = 300 << 20
const MaxFaces = 5_000_000

type point [3]float32
type face [3]uint32
type Mesh struct {
	vertices []point
	faces    []face
	lo, hi   point
}

func (m *Mesh) Faces() int { return len(m.faces) }
func (m *Mesh) addVertex(p point) error {
	for i, n := range p {
		if math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) || math.Abs(float64(n)) > 1e9 {
			return errors.New("invalid mesh coordinate")
		}
		if len(m.vertices) == 0 {
			m.lo[i] = n
			m.hi[i] = n
		} else {
			m.lo[i] = min(n, m.lo[i])
			m.hi[i] = max(n, m.hi[i])
		}
	}
	m.vertices = append(m.vertices, p)
	return nil
}
func Read(filename, kind string, limit int) (*Mesh, error) {
	f, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if st.Size() > MaxBytes || !st.Mode().IsRegular() {
		return nil, errors.New("model exceeds source size limit")
	}
	if limit <= 0 {
		return nil, errors.New("model exceeds triangle limit")
	}
	var m *Mesh
	switch strings.ToLower(kind) {
	case "stl":
		m, err = readSTL(f, st.Size(), limit)
	case "3mf":
		m, err = read3MF(f, st.Size(), limit)
	default:
		return nil, errors.New("only STL and model-only 3MF files can be combined")
	}
	if err == nil && len(m.faces) == 0 {
		return nil, errors.New("model has no triangles")
	}
	return m, err
}
func readSTL(f *os.File, size int64, limit int) (*Mesh, error) {
	var head [84]byte
	n, err := f.ReadAt(head[:], 0)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	m := new(Mesh)
	if n == 84 {
		count := int64(binary.LittleEndian.Uint32(head[80:]))
		if size == 84+50*count {
			if count > int64(limit) {
				return nil, errors.New("model exceeds triangle limit")
			}
			rd := bufio.NewReader(io.NewSectionReader(f, 84, size-84))
			var rec [50]byte
			m.vertices = make([]point, 0, 3*count)
			m.faces = make([]face, 0, count)
			for i := int64(0); i < count; i++ {
				if _, err := io.ReadFull(rd, rec[:]); err != nil {
					return nil, err
				}
				idx := uint32(len(m.vertices))
				for v := 0; v < 3; v++ {
					var p point
					for j := 0; j < 3; j++ {
						p[j] = math.Float32frombits(binary.LittleEndian.Uint32(rec[12+12*v+j*4:]))
					}
					if err := m.addVertex(p); err != nil {
						return nil, err
					}
				}
				m.faces = append(m.faces, face{idx, idx + 1, idx + 2})
			}
			return m, nil
		}
	}
	scanner := bufio.NewScanner(f)
	inside := false
	points := 0
	sawSolid := false
	for scanner.Scan() {
		words := strings.Fields(scanner.Text())
		if len(words) == 0 {
			continue
		}
		switch strings.ToLower(words[0]) {
		case "solid":
			sawSolid = true
		case "facet":
			if inside || !sawSolid || len(m.faces) >= limit {
				return nil, errors.New("invalid STL facet or triangle limit exceeded")
			}
			inside = true
			points = 0
		case "vertex":
			if !inside || len(words) != 4 || points == 3 {
				return nil, errors.New("invalid STL vertex")
			}
			var p point
			for i := 0; i < 3; i++ {
				n, e := strconv.ParseFloat(words[i+1], 32)
				if e != nil {
					return nil, errors.New("invalid STL coordinate")
				}
				p[i] = float32(n)
			}
			if e := m.addVertex(p); e != nil {
				return nil, e
			}
			points++
		case "endfacet":
			if !inside || points != 3 {
				return nil, errors.New("incomplete STL facet")
			}
			inside = false
			n := uint32(len(m.vertices))
			m.faces = append(m.faces, face{n - 3, n - 2, n - 1})
		case "endsolid", "outer", "endloop":
		default:
			return nil, errors.New("unrecognized STL record")
		}
	}
	if inside {
		return nil, errors.New("incomplete STL facet")
	}
	return m, scanner.Err()
}

type matrix [12]float64

var identity = matrix{1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0}

func (t matrix) apply(p point) point {
	var out point
	for i := range out {
		out[i] = float32(float64(p[0])*t[i] + float64(p[1])*t[3+i] + float64(p[2])*t[6+i] + t[9+i])
	}
	return out
}
func compose(parent, child matrix) matrix {
	var out matrix
	for i := 0; i < 4; i++ {
		for j := 0; j < 3; j++ {
			for k := 0; k < 3; k++ {
				out[i*3+j] += child[i*3+k] * parent[k*3+j]
			}
		}
	}
	for j := 0; j < 3; j++ {
		out[9+j] += parent[9+j]
	}
	return out
}

type ref struct {
	file, id  string
	transform matrix
}
type resource struct {
	mesh     Mesh
	children []ref
}

func attrib(e xml.StartElement, key string) string {
	for _, a := range e.Attr {
		if a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}
func objRef(current string, e xml.StartElement) (ref, error) {
	r := ref{file: current, id: attrib(e, "objectid"), transform: identity}
	if r.id == "" {
		return r, errors.New("missing 3MF object id")
	}
	if p := attrib(e, "path"); p != "" {
		if strings.HasPrefix(p, "/") {
			r.file = strings.TrimPrefix(p, "/")
		} else {
			r.file = path.Join(path.Dir(current), p)
		}
		if path.Clean(r.file) != r.file || strings.HasPrefix(r.file, "../") || strings.ContainsAny(r.file, "\\:") {
			return r, errors.New("invalid 3MF part path")
		}
	}
	if s := attrib(e, "transform"); s != "" {
		a := strings.Fields(s)
		if len(a) != 12 {
			return r, errors.New("invalid 3MF transform")
		}
		for i, n := range a {
			x, err := strconv.ParseFloat(n, 64)
			if err != nil || math.IsNaN(x) || math.IsInf(x, 0) {
				return r, errors.New("invalid 3MF transform")
			}
			r.transform[i] = x
		}
	}
	return r, nil
}

type document struct {
	objects map[string]*resource
	build   []ref
	scale   float64
}
type counts struct{ points, faces, refs int }

func parseDocument(member *zip.File, limit int, used *counts) (*document, error) {
	r, err := member.Open()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	d := &document{objects: map[string]*resource{}, scale: 1}
	dec := xml.NewDecoder(r)
	var obj *resource
	stack := []string{}
	for {
		token, e := dec.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		switch el := token.(type) {
		case xml.StartElement:
			if len(stack) > 128 {
				return nil, errors.New("3MF nesting limit exceeded")
			}
			parent := ""
			if len(stack) > 0 {
				parent = stack[len(stack)-1]
			}
			name := el.Name.Local
			stack = append(stack, name)
			switch {
			case name == "model" && parent == "":
				units := map[string]float64{"": 1, "millimeter": 1, "centimeter": 10, "meter": 1000, "inch": 25.4, "foot": 304.8, "micron": 0.001}
				var ok bool
				d.scale, ok = units[attrib(el, "unit")]
				if !ok {
					return nil, errors.New("unknown 3MF model unit")
				}
			case name == "object" && parent == "resources":
				id := attrib(el, "id")
				if id == "" || d.objects[id] != nil || obj != nil {
					return nil, errors.New("invalid 3MF object")
				}
				used.refs++
				obj = new(resource)
				d.objects[id] = obj
			case name == "vertex" && parent == "vertices" && obj != nil:
				used.points++
				if used.points > limit*3 {
					return nil, errors.New("3MF vertex limit exceeded")
				}
				var p point
				for i, k := range []string{"x", "y", "z"} {
					n, e := strconv.ParseFloat(attrib(el, k), 32)
					if e != nil {
						return nil, errors.New("invalid 3MF coordinate")
					}
					p[i] = float32(n)
				}
				if e = obj.mesh.addVertex(p); e != nil {
					return nil, e
				}
			case name == "triangle" && parent == "triangles" && obj != nil:
				used.faces++
				if used.faces > limit {
					return nil, errors.New("3MF triangle limit exceeded")
				}
				var tri face
				for i, k := range []string{"v1", "v2", "v3"} {
					n, e := strconv.ParseUint(attrib(el, k), 10, 32)
					if e != nil || n >= uint64(len(obj.mesh.vertices)) {
						return nil, errors.New("invalid 3MF triangle")
					}
					tri[i] = uint32(n)
				}
				obj.mesh.faces = append(obj.mesh.faces, tri)
			case name == "component" && parent == "components" && obj != nil:
				child, e := objRef(member.Name, el)
				if e != nil {
					return nil, e
				}
				used.refs++
				obj.children = append(obj.children, child)
			case name == "item" && parent == "build":
				item, e := objRef(member.Name, el)
				if e != nil {
					return nil, e
				}
				used.refs++
				d.build = append(d.build, item)
			}
			if used.refs > 100000 {
				return nil, errors.New("too many 3MF objects")
			}
		case xml.EndElement:
			if el.Name.Local == "object" {
				obj = nil
			}
			stack = stack[:len(stack)-1]
		}
	}
	return d, nil
}
func read3MF(f *os.File, size int64, limit int) (*Mesh, error) {
	z, err := zip.NewReader(f, size)
	if err != nil {
		return nil, err
	}
	if len(z.File) > 4096 {
		return nil, errors.New("too many 3MF members")
	}
	docs := map[string]*document{}
	var main string
	var amount uint64
	var used counts
	for _, member := range z.File {
		name := strings.ToLower(member.Name)
		if strings.HasSuffix(name, ".gcode") || strings.HasSuffix(name, ".gcode.gz") {
			return nil, errors.New("sliced 3MF files cannot be combined")
		}
		if !strings.HasSuffix(name, ".model") {
			continue
		}
		if member.UncompressedSize64 > MaxBytes-amount {
			return nil, errors.New("expanded 3MF exceeds size limit")
		}
		amount += member.UncompressedSize64
		if docs[member.Name] != nil {
			return nil, errors.New("duplicate 3MF model member")
		}
		d, e := parseDocument(member, limit, &used)
		if e != nil {
			return nil, e
		}
		docs[member.Name] = d
		if name == "3d/3dmodel.model" {
			main = member.Name
		}
	}
	if main == "" && len(docs) == 1 {
		for k := range docs {
			main = k
		}
	}
	root := docs[main]
	if root == nil || len(root.build) == 0 {
		return nil, errors.New("missing 3MF plate geometry")
	}
	out := new(Mesh)
	active := map[[2]string]bool{}
	expanded := 0
	var build func(ref, matrix, float64) error
	build = func(r ref, parent matrix, parentScale float64) error {
		d := docs[r.file]
		if d == nil || d.objects[r.id] == nil {
			return errors.New("unresolved 3MF model reference")
		}
		key := [2]string{r.file, r.id}
		expanded++
		if active[key] || len(active) > 64 || expanded > 100000 {
			return errors.New("3MF component expansion limit exceeded")
		}
		active[key] = true
		defer delete(active, key)
		t := r.transform
		for i := 0; i < 9; i++ {
			t[i] *= d.scale / parentScale
		}
		world := compose(parent, t)
		o := d.objects[r.id]
		if len(out.faces)+len(o.mesh.faces) > limit || len(out.vertices)+len(o.mesh.vertices) > limit*3 {
			return errors.New("combined geometry exceeds limit")
		}
		idx := uint32(len(out.vertices))
		for _, v := range o.mesh.vertices {
			if err := out.addVertex(world.apply(v)); err != nil {
				return err
			}
		}
		for _, f := range o.mesh.faces {
			out.faces = append(out.faces, face{f[0] + idx, f[1] + idx, f[2] + idx})
		}
		for _, c := range o.children {
			if e := build(c, world, d.scale); e != nil {
				return e
			}
		}
		return nil
	}
	base := identity
	for _, i := range []int{0, 4, 8} {
		base[i] = root.scale
	}
	for _, item := range root.build {
		if err := build(item, base, root.scale); err != nil {
			return nil, fmt.Errorf("3MF build: %w", err)
		}
	}
	return out, nil
}
