package production

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"bambastic/internal/access"
)

type snippets struct {
	Start string `json:"start_gcode"`
	End   string `json:"end_gcode"`
}

var gcodeVar = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

func replaceVars(text string, header map[string]string) string {
	aliases := map[string]string{"max_layer_z": "max_z_height", "max_print_height": "max_z_height", "total_layers": "total_layer_number"}
	return gcodeVar.ReplaceAllStringFunc(text, func(k string) string {
		name := k[1 : len(k)-1]
		if x, ok := header[name]; ok {
			return x
		}
		if x, ok := header[aliases[name]]; ok {
			return x
		}
		return k
	})
}

type gcodeOffsets struct {
	start, end, size int64
	header           map[string]string
	finalLF          bool
}

func inspectGcode(ctx context.Context, f *zip.File) (gcodeOffsets, error) {
	x := gcodeOffsets{start: -1, end: -1, header: map[string]string{}}
	if f.UncompressedSize64 > 2<<30 {
		return x, errors.New("G-code exceeds size limit")
	}
	r, e := f.Open()
	if e != nil {
		return x, e
	}
	defer r.Close()
	b := bufio.NewReaderSize(r, 256<<10)
	hdr := false
	atStart := true
	for {
		if e = ctx.Err(); e != nil {
			return x, e
		}
		data, err := b.ReadSlice('\n')
		if len(data) > 0 {
			if atStart {
				line := strings.TrimSpace(string(data))
				switch {
				case line == "; HEADER_BLOCK_START":
					hdr = true
				case line == "; HEADER_BLOCK_END":
					hdr = false
				case line == "; MACHINE_START_GCODE_END" && x.start < 0:
					x.start = x.size
				case line == "; EXECUTABLE_BLOCK_END" && x.end < 0:
					x.end = x.size
				case hdr && strings.HasPrefix(line, ";") && len(line) < 4096 && len(x.header) < 1024:
					k, v, ok := strings.Cut(line[1:], ":")
					if ok {
						k, _, _ = strings.Cut(k, "[")
						x.header[strings.ReplaceAll(strings.ToLower(strings.TrimSpace(k)), " ", "_")] = strings.TrimSpace(v)
					}
				}
			}
			x.finalLF = data[len(data)-1] == '\n'
			x.size += int64(len(data))
			atStart = x.finalLF
			if x.size > int64(f.UncompressedSize64) {
				return x, errors.New("Invalid G-code size")
			}
		}
		switch err {
		case nil, bufio.ErrBufferFull:
			continue
		case io.EOF:
			if x.size != int64(f.UncompressedSize64) {
				return x, io.ErrUnexpectedEOF
			}
			return x, nil
		default:
			return x, err
		}
	}
}
func rewritePlate(ctx context.Context, dir, src string, pl int64, sn snippets) (string, error) {
	z, e := zip.OpenReader(src)
	if e != nil {
		return "", e
	}
	defer z.Close()
	target := fmt.Sprintf("Metadata/plate_%d.gcode", pl)
	var g *zip.File
	for _, f := range z.File {
		if f.Name == target {
			if g != nil {
				return "", errors.New("Ambiguous G-code member")
			}
			g = f
		}
	}
	if g == nil {
		return "", errors.New("Selected plate has no G-code")
	}
	m, e := inspectGcode(ctx, g)
	if e != nil {
		return "", e
	}
	if m.start < 0 {
		m.start = 0
	}
	if m.end < 0 {
		m.end = m.size
	}
	if m.start > m.end {
		return "", errors.New("Invalid G-code block order")
	}
	tmp, e := os.CreateTemp(dir, "dispatch-*.3mf")
	if e != nil {
		return "", e
	}
	keep := false
	defer func() {
		tmp.Close()
		if !keep {
			os.Remove(tmp.Name())
		}
	}()
	dst := zip.NewWriter(tmp)
	hasMD5 := false
	digest := md5.New() // The firmware specifies uppercase MD5 for this sidecar.
	for _, f := range z.File {
		if e = ctx.Err(); e != nil {
			return "", e
		}
		if f.Name == target+".md5" {
			hasMD5 = true
			continue
		}
		if f != g {
			if e = dst.Copy(f); e != nil {
				return "", e
			}
			continue
		}
		out, e := dst.CreateHeader(&zip.FileHeader{Name: target, Method: f.Method, Modified: f.Modified})
		if e != nil {
			return "", e
		}
		rd, e := g.Open()
		if e != nil {
			return "", e
		}
		defer rd.Close()
		w := io.MultiWriter(out, digest)
		var offset int64
		for _, p := range []struct {
			pos  int64
			text string
		}{{m.start, sn.Start}, {m.end, sn.End}} {
			if strings.TrimSpace(p.text) == "" {
				continue
			}
			if _, e = io.CopyN(w, rd, p.pos-offset); e != nil {
				return "", e
			}
			offset = p.pos
			txt := strings.TrimRight(replaceVars(p.text, m.header), "\r\n") + "\n"
			if p.pos == m.size && !m.finalLF {
				txt = "\n" + txt
			}
			if _, e = io.WriteString(w, txt); e != nil {
				return "", e
			}
		}
		if _, e = io.Copy(w, rd); e != nil {
			return "", e
		}
	}
	if hasMD5 {
		w, e := dst.Create(target + ".md5")
		if e != nil {
			return "", e
		}
		if _, e = fmt.Fprintf(w, "%X", digest.Sum(nil)); e != nil {
			return "", e
		}
	}
	if e = dst.Close(); e != nil {
		return "", e
	}
	if e = tmp.Close(); e != nil {
		return "", e
	}
	keep = true
	return tmp.Name(), nil
}
func (s *Service) dispatchBytes(ctx context.Context, job row, original string) (string, error) {
	if !flag(job["gcode_injection"]) || !strings.HasSuffix(strings.ToLower(val(job["remote"])), ".3mf") {
		return original, nil
	}
	raw, e := access.Setting(ctx, s.db, "gcode_snippets", "")
	if e != nil {
		return "", e
	}
	if strings.TrimSpace(raw) == "" {
		return original, nil
	}
	var all map[string]snippets
	if e = json.NewDecoder(bytes.NewBufferString(raw)).Decode(&all); e != nil {
		return "", e
	}
	sets := all[val(job["dispatch_model"])]
	if sets.Start == "" && sets.End == "" {
		return original, nil
	}
	return rewritePlate(ctx, filepath.Join(s.dir, "production"), original, num(job["plate_id"]), sets)
}
