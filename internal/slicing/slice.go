package slicing

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"bambastic/internal/access"
)

//go:embed options.json
var optionData []byte
var processKeys = func() map[string]bool {
	var keys []string
	_ = json.Unmarshal(optionData, &keys)
	m := map[string]bool{}
	for _, k := range keys {
		m[k] = true
	}
	return m
}()
var color = regexp.MustCompile(`^#[0-9A-Fa-f]{6}([0-9A-Fa-f]{2})?$`)
var gcodeName = regexp.MustCompile(`^Metadata/plate_[1-9][0-9]*\.gcode$`)
var bedNames = map[string]string{"Cool Plate": "Cool Plate", "Engineering Plate": "Engineering Plate", "High Temp Plate": "High Temp Plate", "Textured PEI Plate": "Textured PEI Plate", "Supertack Plate": "Supertack Plate", "Textured Cool Plate": "Textured Cool Plate", "Cool Plate (SuperTack)": "Supertack Plate", "Smooth PEI Plate": "High Temp Plate"}

type sliceRequest struct {
	Printer    presetRef   `json:"printer_preset"`
	Process    presetRef   `json:"process_preset"`
	Filament   presetRef   `json:"filament_preset"`
	PrinterID  int64       `json:"printer_preset_id"`
	ProcessID  int64       `json:"process_preset_id"`
	FilamentID int64       `json:"filament_preset_id"`
	Filaments  []presetRef `json:"filament_presets"`
	Colors     []string    `json:"filament_colours"`
	Embedded   bool        `json:"use_embedded_settings"`
	Plate      *int        `json:"plate"`
	Bed        string      `json:"bed_type"`
	Orient     bool        `json:"auto_orient"`
	Arrange    bool        `json:"auto_arrange"`
	Design     []string    `json:"design_overrides"`
	Overrides  object      `json:"process_overrides"`
}
type recipe struct {
	printer, process            object
	filaments                   []object
	embedded, arrange, fallback bool
	target                      string
}

func projectSettings(z *zip.Reader) object {
	for _, v := range z.File {
		if v.Name != "Metadata/project_settings.config" {
			continue
		}
		if v.UncompressedSize64 > 16<<20 {
			return nil
		}
		f, e := v.Open()
		if e != nil {
			return nil
		}
		defer f.Close()
		var cfg object
		e = json.NewDecoder(io.LimitReader(f, 16<<20)).Decode(&cfg)
		if e != nil {
			return nil
		}
		return cfg
	}
	return nil
}
func modelName(name string) string {
	var m map[string]string
	_ = json.Unmarshal(printerModels, &m)
	var best, code string
	for k, v := range m {
		if len(k) > len(best) && strings.Contains(strings.ToLower(name), strings.ToLower(k)) {
			best = k
			code = v
		}
		if strings.EqualFold(v, name) {
			return v
		}
	}
	return code
}
func dual(model string) bool { return strings.HasPrefix(model, "H2D") || model == "H2C" }
func normalize(ref presetRef, id int64) (presetRef, error) {
	if id > 0 {
		if ref.ID != "" {
			return ref, errors.New("Choose either a preset reference or a legacy preset ID")
		}
		return presetRef{"local", strconv.FormatInt(id, 10)}, nil
	}
	return ref, nil
}
func overrideValue(v any) any {
	if list, ok := v.([]any); ok {
		out := make([]string, len(list))
		for i, x := range list {
			out[i] = asText(x)
		}
		return out
	}
	return asText(v)
}
func (s *Service) profiles(ctx context.Context, r sliceRequest, engine, ext string, f *os.File) (recipe, error) {
	var p recipe
	if r.Plate != nil && *r.Plate < 0 {
		return p, errors.New("Plate must be zero (all plates) or a positive plate number")
	}
	if r.Bed != "" && (bedNames[r.Bed] == "" || bedNames[r.Bed] == "Textured Cool Plate" && engine != "orcaslicer") {
		return p, errors.New("Unknown or incompatible bed type")
	}
	for _, c := range r.Colors {
		if c != "" && !color.MatchString(c) {
			return p, errors.New("Invalid filament colour")
		}
	}
	var cfg object
	if ext == ".3mf" {
		st, e := f.Stat()
		if e != nil {
			return p, e
		}
		z, e := zip.NewReader(f, st.Size())
		if e != nil {
			return p, errors.New("Invalid 3MF source")
		}
		cfg = projectSettings(z)
	}
	printer, e := normalize(r.Printer, r.PrinterID)
	if e != nil {
		return p, e
	}
	process, e := normalize(r.Process, r.ProcessID)
	if e != nil {
		return p, e
	}
	if len(r.Filaments) == 0 {
		one, e := normalize(r.Filament, r.FilamentID)
		if e != nil {
			return p, e
		}
		r.Filaments = []presetRef{one}
	}
	if len(r.Filaments) > 64 {
		return p, errors.New("Too many filament slots")
	}
	p.printer, e = s.resolve(ctx, "printer", printer)
	if e != nil {
		return p, e
	}
	p.process, e = s.resolve(ctx, "process", process)
	if e != nil {
		return p, e
	}
	srcModel := modelName(asText(cfg["printer_model"]))
	p.target = modelName(asText(p.printer["printer_model"]))
	if p.target == "" {
		p.target = modelName(asText(p.printer["name"]))
	}
	if p.target == "" {
		p.target = modelName(asText(p.printer["inherits"]))
	}
	p.arrange = r.Arrange || srcModel != "" && p.target != "" && dual(srcModel) != dual(p.target)
	p.fallback = cfg != nil && srcModel != "" && srcModel == p.target && !(r.Plate != nil && *r.Plate == 0) && !p.arrange && !r.Orient && r.Bed == "" && len(r.Overrides) == 0 && len(r.Design) == 0 && len(r.Colors) == 0
	if r.Embedded && cfg != nil {
		if srcModel == "" || srcModel != p.target {
			return p, errors.New("Embedded settings need a matching source and target printer model")
		}
		p.embedded = true
	}
	designKeys := map[string]bool{}
	if a, ok := cfg["different_settings_to_system"].([]any); ok && len(a) > 0 {
		for _, k := range strings.Split(asText(a[0]), ";") {
			designKeys[k] = true
		}
	}
	selected := map[string]bool{}
	for _, k := range r.Design {
		selected[k] = true
		if processKeys[k] && designKeys[k] && cfg[k] != nil {
			p.process[k] = cfg[k]
		}
	}
	for _, k := range []string{"enable_support", "support_type", "support_style", "support_threshold_angle", "support_on_build_plate_only", "support_critical_regions_only"} {
		if cfg[k] != nil && (r.Design == nil || selected[k] || !designKeys[k]) {
			p.process[k] = cfg[k]
		}
	}
	for k, v := range r.Overrides {
		if processKeys[k] && v != nil {
			p.process[k] = overrideValue(v)
		}
	}
	if r.Bed != "" {
		p.process["curr_bed_type"] = bedNames[r.Bed]
	}
	srcColors, _ := cfg["filament_colour"].([]any)
	for i, ref := range r.Filaments {
		fp, e := s.resolve(ctx, "filament", ref)
		if e != nil {
			return p, e
		}
		c := asText(fp["default_filament_colour"])
		if !color.MatchString(c) && i < len(srcColors) {
			c = asText(srcColors[i])
		}
		if i < len(r.Colors) && r.Colors[i] != "" {
			c = r.Colors[i]
		}
		if color.MatchString(c) {
			fp["filament_colour"] = []string{c}
		}
		p.filaments = append(p.filaments, fp)
	}
	return p, nil
}
func idString() string { var id [16]byte; rand.Read(id[:]); return hex.EncodeToString(id[:]) }
func sendBody(w *multipart.Writer, j *job, r sliceRequest, p recipe, f *os.File, id string) error {
	if _, e := f.Seek(0, 0); e != nil {
		return e
	}
	part, e := w.CreateFormFile("file", filepath.Base(j.source.name))
	if e != nil {
		return e
	}
	if _, e = io.Copy(part, f); e != nil {
		return e
	}
	if !p.embedded {
		profiles := []struct {
			field, name string
			o           object
		}{{"printerProfile", "printer.json", p.printer}, {"presetProfile", "preset.json", p.process}}
		for i, fp := range p.filaments {
			profiles = append(profiles, struct {
				field, name string
				o           object
			}{"filamentProfile", fmt.Sprintf("filament_%d.json", i+1), fp})
		}
		for _, pf := range profiles {
			part, e = w.CreateFormFile(pf.field, pf.name)
			if e != nil {
				return e
			}
			if e = json.NewEncoder(part).Encode(pf.o); e != nil {
				return e
			}
		}
	}
	fields := map[string]string{"exportType": "3mf", "requestId": id}
	if r.Plate != nil {
		fields["plate"] = strconv.Itoa(*r.Plate)
	}
	if p.arrange {
		fields["arrange"] = "true"
	}
	if r.Orient {
		fields["orient"] = "true"
	}
	for k, v := range fields {
		if e = w.WriteField(k, v); e != nil {
			return e
		}
	}
	return w.Close()
}
func (s *Service) transfer(ctx context.Context, j *job, r sliceRequest, p recipe, f *os.File, base, id string) (*http.Response, error) {
	rd, wr := io.Pipe()
	writer := multipart.NewWriter(wr)
	req, e := http.NewRequestWithContext(ctx, "POST", base+"/slice", rd)
	if e != nil {
		rd.Close()
		wr.Close()
		return nil, e
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	done := make(chan struct{})
	go func() { defer close(done); wr.CloseWithError(sendBody(writer, j, r, p, f, id)) }()
	resp, e := s.client.Do(req)
	rd.Close()
	<-done
	return resp, e
}
func (s *Service) monitor(ctx context.Context, cancel context.CancelCauseFunc, j *job, base, id string, stall time.Duration) {
	last := time.Now()
	var previous string
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c, stop := context.WithTimeout(ctx, 3*time.Second)
			var progress object
			_, e := s.jsonCall(c, "GET", base+"/slice/progress/"+id, nil, &progress)
			stop()
			if e == nil && progress != nil {
				b, _ := json.Marshal(progress)
				if string(b) != previous {
					last = time.Now()
					previous = string(b)
					s.mu.Lock()
					j.progress = progress
					s.mu.Unlock()
				}
			}
			if time.Since(last) > stall {
				cancel(errors.New("Slicer stopped reporting progress and exceeded the stall timeout"))
				return
			}
		}
	}
}
func validateOutput(f *os.File, standard bool, p recipe) error {
	st, e := f.Stat()
	if e != nil {
		return e
	}
	z, e := zip.NewReader(f, st.Size())
	if e != nil {
		return errors.New("Slicer returned an invalid 3MF container")
	}
	hasGcode := false
	for _, v := range z.File {
		if gcodeName.MatchString(v.Name) && v.UncompressedSize64 > 0 {
			g, e := v.Open()
			if e != nil {
				return e
			}
			buf := make([]byte, 1)
			_, e = io.ReadFull(g, buf)
			g.Close()
			if e != nil {
				return errors.New("Cannot read sliced plate G-code")
			}
			hasGcode = true
		}
	}
	if !hasGcode {
		return errors.New("Slicer output contains no printable plate G-code")
	}
	cfg := projectSettings(z)
	if value, ok := cfg["machine_start_gcode"]; ok && standard && !p.embedded && !strings.Contains(asText(value), "gcode_claim_action") {
		return errors.New("The sidecar failed to resolve the standard printer's machine start program")
	}
	if values, ok := cfg["filament_settings_id"].([]any); ok {
		for i, v := range values {
			if asText(v) == "" {
				log.Printf("slicer output has unresolved filament settings at slot %d", i+1)
			}
		}
	}
	return nil
}
func num(h http.Header, k string) float64 { v, _ := strconv.ParseFloat(h.Get(k), 64); return v }
func (s *Service) slice(j *job, r sliceRequest, p recipe, base string, f *os.File) (object, int, error) {
	raw, _ := access.Setting(s.ctx, s.db, "slicer_stall_timeout_minutes", "15")
	minutes, e := strconv.Atoi(raw)
	if e != nil || minutes < 1 || minutes > 10080 {
		minutes = 15
	}
	do := func(p recipe) (*http.Response, context.CancelCauseFunc, error) {
		id := idString()
		ctx, cancel := context.WithCancelCause(s.ctx)
		done := make(chan struct{})
		go func() { defer close(done); s.monitor(ctx, cancel, j, base, id, time.Duration(minutes)*time.Minute) }()
		res, e := s.transfer(ctx, j, r, p, f, base, id)
		if e != nil && context.Cause(ctx) != nil {
			e = context.Cause(ctx)
		}
		return res, func(err error) { cancel(err); <-done }, e
	}
	res, cancel, e := do(p)
	if e == nil && res.StatusCode >= 500 && p.fallback && !p.embedded {
		res.Body.Close()
		cancel(nil)
		p.embedded = true
		res, cancel, e = do(p)
	}
	defer cancel(nil)
	if e != nil {
		return nil, 503, fmt.Errorf("Cannot complete the slice request: %w", e)
	}
	defer res.Body.Close()
	if res.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return nil, res.StatusCode, fmt.Errorf("Slicer returned %s: %s", res.Status, strings.TrimSpace(string(msg)))
	}
	tmp, e := os.CreateTemp(filepath.Join(s.dir, "slices"), ".slice-")
	if e != nil {
		return nil, 500, e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	hasher := sha256.New()
	size, e := io.Copy(io.MultiWriter(tmp, hasher), io.LimitReader(res.Body, 4<<30+1))
	if e != nil {
		return nil, 502, e
	}
	if size > 4<<30 {
		return nil, 413, errors.New("Sliced file exceeds 4 GiB")
	}
	if e = validateOutput(tmp, r.Printer.Source == "standard", p); e != nil {
		return nil, 502, e
	}
	if e = tmp.Sync(); e != nil {
		return nil, 500, e
	}
	if e = tmp.Close(); e != nil {
		return nil, 500, e
	}
	stem := strings.TrimSuffix(j.source.name, filepath.Ext(j.source.name))
	stem = strings.TrimSuffix(filepath.Base(stem), ".gcode")
	name := stem + ".gcode.3mf"
	path := filepath.Join("slices", idString()+".3mf")
	abs := filepath.Join(s.dir, path)
	if e = os.Rename(tmp.Name(), abs); e != nil {
		return nil, 500, e
	}
	saved := false
	defer func() {
		if !saved {
			os.Remove(abs)
		}
	}()
	duration := int64(num(res.Header, "X-Print-Time-Seconds"))
	g := num(res.Header, "X-Filament-Used-G")
	mm := num(res.Header, "X-Filament-Used-Mm")
	hash := hex.EncodeToString(hasher.Sum(nil))
	meta := object{"print_time_seconds": duration, "filament_used_g": g, "filament_used_mm": mm, "used_embedded_settings": p.embedded, "sliced_for_model": p.target, "source_id": j.source.id}
	blob, _ := json.Marshal(meta)
	result := object{"name": name, "print_time_seconds": duration, "filament_used_g": g, "filament_used_mm": mm, "used_embedded_settings": p.embedded}
	if j.kind == "library_file" || j.libraryOutput {
		if farm, ok := s.farm.(interface {
			SaveLibrarySlice(context.Context, access.Principal, io.Reader, string, int64, int64, map[string]any) (int64, string, string, error)
		}); ok {
			output, err := os.Open(abs)
			if err != nil {
				return nil, 500, err
			}
			defer output.Close()
			k, filename, fallback, err := farm.SaveLibrarySlice(s.ctx, j.principal, output, name, j.source.folderID.Int64, j.source.projectID.Int64, meta)
			if err != nil {
				return nil, 500, err
			}
			result["library_file_id"] = k
			result["name"] = filename
			if fallback != "" {
				result["external_write_fallback"] = fallback
			}
			return result, 200, nil
		}
	}
	tx, e := s.db.BeginTx(s.ctx, nil)
	if e != nil {
		return nil, 500, e
	}
	defer tx.Rollback()
	var owner any
	if j.owner > 0 {
		var exists int64
		if e = tx.QueryRow("SELECT id FROM users WHERE id=?", j.owner).Scan(&exists); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return nil, 500, e
		}
		if e == nil {
			owner = exists
		}
	}
	var ins sql.Result
	var idkey string
	if j.kind == "library_file" || j.libraryOutput {
		ins, e = tx.Exec(`INSERT INTO library_files(filename,file_path,file_type,file_size,file_hash,file_metadata,folder_id,project_id,is_external,print_count,source_type,created_by_id) VALUES(?,?,'gcode.3mf',?,?,?,?,?,0,0,'sliced',?)`, name, path, size, hash, string(blob), j.source.folderID, j.source.projectID, owner)
		idkey = "library_file_id"
	} else {
		ins, e = tx.Exec(`INSERT INTO print_archives(filename,file_path,file_size,content_hash,print_name,status,is_favorite,quantity,printer_id,project_id,source_3mf_path,print_time_seconds,filament_used_grams,sliced_for_model,extra_data,created_by_id) VALUES(?,?,?,?,?,'archived',0,1,?,?,?,?,?,?,?,?)`, name, path, size, hash, stem, j.source.printerID, j.source.projectID, j.source.path, duration, g, blankNull(p.target), string(blob), owner)
		idkey = "archive_id"
	}
	if e != nil {
		return nil, 500, e
	}
	result[idkey], e = ins.LastInsertId()
	if e != nil {
		return nil, 500, e
	}
	if e = tx.Commit(); e != nil {
		return nil, 500, e
	}
	saved = true
	return result, 200, nil
}
