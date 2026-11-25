package management

import (
	"archive/zip"
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type extrusionSample struct {
	layer int
	mm    float64
}

// layerExtrusion stores sparse cumulative net extrusion per slicer tool.
// Subtracting retractions avoids counting an unretract as additional material.
type layerExtrusion struct {
	series map[int][]extrusionSample
	end    int
}

func (x *layerExtrusion) through(tool, layer int) float64 {
	if x == nil || layer <= 0 {
		return 0
	}
	v := x.series[tool]
	if len(v) == 0 && len(x.series) == 1 {
		v = x.series[-1]
	} // single unlabeled toolpath
	at := sort.Search(len(v), func(i int) bool { return v[i].layer > layer })
	if at == 0 {
		return 0
	}
	return v[at-1].mm
}
func parseExtrusion(ctx context.Context, rd io.Reader) (*layerExtrusion, error) {
	out := &layerExtrusion{series: map[int][]extrusionSample{}}
	layer, tool := 0, -1
	relative := true
	absoluteE := 0.
	cur := map[int]float64{}
	dirty := map[int]bool{}
	count := 0
	flush := func() error {
		for t := range dirty {
			v := out.series[t]
			sample := extrusionSample{layer, math.Max(0, cur[t])}
			if len(v) > 0 && v[len(v)-1].layer == layer {
				v[len(v)-1] = sample
			} else {
				v = append(v, sample)
				count++
			}
			out.series[t] = v
		}
		clear(dirty)
		if count > 1000000 {
			return errors.New("layer history exceeds sample limit")
		}
		return nil
	}
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for line := 0; sc.Scan(); line++ {
		if line%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		body, _, _ := strings.Cut(sc.Text(), ";")
		args := strings.Fields(body)
		if len(args) == 0 {
			continue
		}
		cmd := strings.ToUpper(args[0])
		switch cmd {
		case "M82":
			relative = false
			continue
		case "M83":
			relative = true
			continue
		case "M73", "M620", "G0", "G1", "G2", "G3", "G92":
		default:
			if len(cmd) > 1 && cmd[0] == 'T' {
				if t, e := strconv.Atoi(cmd[1:]); e == nil && t >= 0 && t < 128 {
					tool = t
				}
			}
			continue
		}
		for _, s := range args[1:] {
			if len(s) < 2 {
				continue
			}
			name := s[0]
			if name >= 'a' && name <= 'z' {
				name -= 32
			}
			switch {
			case cmd == "M73" && name == 'L':
				n, e := strconv.Atoi(s[1:])
				if e != nil || n < 0 || n > 1000000 {
					continue
				}
				if n < layer {
					return nil, errors.New("nonmonotonic toolpath layers")
				}
				if n != layer {
					if e = flush(); e != nil {
						return nil, e
					}
					layer = n
					out.end = n
				}
			case cmd == "M620" && name == 'S':
				n, e := strconv.Atoi(strings.TrimRight(strings.ToUpper(s[1:]), "A"))
				if e != nil {
					continue
				}
				if n == 255 {
					tool = -2
				} else if n >= 0 && n < 128 {
					tool = n
				}
			case name == 'E' && (cmd == "G0" || cmd == "G1" || cmd == "G2" || cmd == "G3" || cmd == "G92"):
				n, e := strconv.ParseFloat(s[1:], 64)
				if e != nil || math.IsNaN(n) || math.IsInf(n, 0) {
					return nil, errors.New("invalid extrusion coordinate")
				}
				if cmd == "G92" {
					absoluteE = n
					continue
				}
				delta := n
				if !relative {
					delta = n - absoluteE
					absoluteE = n
				} else {
					absoluteE += n
				}
				if tool != -2 && delta != 0 {
					cur[tool] += delta
					dirty[tool] = true
				}
			}
		}
	}
	if e := sc.Err(); e != nil {
		return nil, e
	}
	if e := flush(); e != nil {
		return nil, e
	}
	if out.end == 0 || len(out.series) == 0 {
		return nil, nil
	}
	return out, nil
}

func readExtrusion(ctx context.Context, path string, plate int64) (*layerExtrusion, error) {
	const limit = 512 << 20
	switch strings.ToLower(filepath.Ext(path)) {
	case ".gcode":
		f, e := os.Open(path)
		if e != nil {
			return nil, e
		}
		defer f.Close()
		st, e := f.Stat()
		if e != nil || st.Size() > limit {
			return nil, errors.New("G-code size unavailable or over limit")
		}
		return parseExtrusion(ctx, f)
	case ".3mf":
		z, e := zip.OpenReader(path)
		if e != nil {
			return nil, e
		}
		defer z.Close()
		name := fmt.Sprintf("Metadata/plate_%d.gcode", plate)
		var selected, only *zip.File
		members := 0
		for _, f := range z.File {
			if !strings.HasSuffix(strings.ToLower(f.Name), ".gcode") {
				continue
			}
			members++
			only = f
			if f.Name == name {
				selected = f
			}
		}
		if selected == nil && members == 1 {
			selected = only
		}
		if selected == nil || selected.UncompressedSize64 > limit {
			return nil, nil
		}
		r, e := selected.Open()
		if e != nil {
			return nil, e
		}
		defer r.Close()
		return parseExtrusion(ctx, io.LimitReader(r, limit+1))
	}
	return nil, nil
}
