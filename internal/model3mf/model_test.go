package model3mf

import (
	"archive/zip"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestCoreAndMultipartGeometry(t *testing.T) {
	part := `<model unit="centimeter"><resources><object id="7" type="model"><mesh><vertices><vertex x="0" y="0" z="0"/><vertex x="1" y="0" z="0"/><vertex x="0" y="1" z="0"/></vertices><triangles><triangle v1="0" v2="1" v3="2"/></triangles></mesh></object></resources></model>`
	build := `<model unit="millimeter" xmlns:p="http://schemas.microsoft.com/3dmanufacturing/production/2015/06"><resources><object id="1"><components><component p:path="/3D/Objects/part.model" objectid="7" transform="1 0 0 0 1 0 0 0 1 5 0 0"/></components></object></resources><build><item objectid="1" transform="1 0 0 0 1 0 0 0 1 0 7 0"/></build></model>`
	archive := func(name string, members map[string]string) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), name)
		f, e := os.Create(p)
		if e != nil {
			t.Fatal(e)
		}
		w := zip.NewWriter(f)
		for n, b := range members {
			a, _ := w.Create(n)
			a.Write([]byte(b))
		}
		if e = w.Close(); e != nil {
			t.Fatal(e)
		}
		f.Close()
		return p
	}
	entries := map[string]string{"3D/3dmodel.model": build, "3D/Objects/part.model": part}
	m, e := Read(archive("assembly.3mf", entries), "3mf", 10)
	if e != nil || m.Faces() != 1 || m.lo != (point{5, 7, 0}) || m.hi != (point{15, 17, 0}) {
		t.Fatal(m, e)
	}
	entries["Metadata/plate_1.gcode"] = ";"
	if _, e := Read(archive("sliced.3mf", entries), "3mf", 10); e == nil {
		t.Fatal("accepted sliced source")
	}
	cyclic := `<model><resources><object id="1"><components><component objectid="1"/></components></object></resources><build><item objectid="1"/></build></model>`
	if _, e := Read(archive("cycle.3mf", map[string]string{"3D/3dmodel.model": cyclic}), "3mf", 10); e == nil {
		t.Fatal("accepted recursive object")
	}
}
func TestBinarySTLFiniteCoordinates(t *testing.T) {
	b := make([]byte, 134)
	binary.LittleEndian.PutUint32(b[80:], 1)
	binary.LittleEndian.PutUint32(b[108:], math.Float32bits(10))
	binary.LittleEndian.PutUint32(b[124:], math.Float32bits(15))
	p := filepath.Join(t.TempDir(), "part.stl")
	os.WriteFile(p, b, 0600)
	m, e := Read(p, "stl", 1)
	if e != nil || m.Faces() != 1 || m.hi != (point{10, 15, 0}) {
		t.Fatal(m, e)
	}
	binary.LittleEndian.PutUint32(b[108:], math.Float32bits(float32(math.NaN())))
	os.WriteFile(p, b, 0600)
	if _, e := Read(p, "stl", 1); e == nil {
		t.Fatal("non-finite coordinate accepted")
	}
}
