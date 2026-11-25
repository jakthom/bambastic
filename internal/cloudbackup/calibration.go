package cloudbackup

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

var calibrationPath = regexp.MustCompile(`^kprofiles/([a-zA-Z0-9_-]{1,48})/(0\.[1-9][0-9]?|1(?:\.[1-9][0-9]?)?|2)\.json$`)

type calibrationFile struct {
	serial, nozzle string
	profiles       []obj
}

func (s *Service) collectCalibrations(ctx context.Context, docs map[string]obj, metadata obj) error {
	if s.KProfiles == nil || s.CalibrationConnected == nil {
		metadata["kprofiles_status"] = "device calibration reader unavailable"
		return nil
	}
	ps, e := rows(ctx, s.db, "SELECT id,serial_number,name FROM printers WHERE is_active ORDER BY id")
	if e != nil {
		return e
	}
	failures := 0
	for _, p := range ps {
		id := ident(p["id"])
		if !s.CalibrationConnected(id) {
			continue
		}
		serial := str(p["serial_number"])
		for _, nozzle := range []string{"0.2", "0.4", "0.6", "0.8"} {
			path := "kprofiles/" + serial + "/" + nozzle + ".json"
			if !calibrationPath.MatchString(path) {
				continue
			}
			records, e := s.KProfiles(ctx, id, nozzle)
			if e != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				failures++
				continue
			}
			if len(records) == 0 {
				continue
			}
			out := []obj{}
			for _, profile := range records {
				m := obj{}
				for _, k := range []string{"slot_id", "name", "k_value", "filament_id", "nozzle_id", "extruder_id", "setting_id", "n_coef"} {
					m[k] = profile[k]
				}
				out = append(out, m)
			}
			docs[path] = obj{"version": "1.0", "printer_name": p["name"], "printer_serial": serial, "nozzle_diameter": nozzle, "profiles": out}
		}
	}
	if failures > 0 {
		metadata["kprofiles_read_failures"] = failures
	}
	return nil
}
func (r repository) paths(ctx context.Context, ref string) ([]string, error) {
	out := []string{}
	for page := 1; page <= 100; page++ {
		list := []obj{}
		more := false
		if r.kind == "gitlab" {
			e := r.call(ctx, "GET", fmt.Sprintf("/repository/tree?recursive=true&ref=%s&per_page=100&page=%d", url.QueryEscape(ref), page), nil, &list)
			if e != nil {
				return nil, e
			}
			more = len(list) == 100
		} else {
			var tree obj
			path := fmt.Sprintf("/git/trees/%s?recursive=1&page=%d&per_page=1000", ref, page)
			if e := r.call(ctx, "GET", path, nil, &tree); e != nil {
				return nil, e
			}
			a, ok := tree["tree"].([]any)
			if !ok {
				return nil, errors.New("invalid repository tree")
			}
			for _, x := range a {
				list = append(list, sub(x))
			}
			if truth(tree["truncated"]) {
				if r.kind == "github" {
					return nil, errors.New("snapshot tree is truncated")
				}
				more = len(a) > 0
			}
		}
		for _, f := range list {
			if f["type"] == "blob" && calibrationPath.MatchString(str(f["path"])) {
				out = append(out, str(f["path"]))
			}
		}
		if !more {
			return out, nil
		}
	}
	return nil, errors.New("snapshot tree exceeds listing limit")
}
func (s *Service) restoreCalibrations(ctx context.Context, files []calibrationFile, t *tally) {
	t.note("kprofilesAlwaysOverwrite", "Calibration writes replace matching slots on the printer")
	for _, f := range files {
		n := len(f.profiles)
		if n == 0 {
			continue
		}
		var id int64
		e := s.db.QueryRowContext(ctx, "SELECT id FROM printers WHERE serial_number=? AND is_active", f.serial).Scan(&id)
		if errors.Is(e, sql.ErrNoRows) {
			t.Skipped += n
			t.note("kprofilesPrinterMissing", "A snapshot printer is absent")
			continue
		}
		if e != nil {
			t.Failed += n
			continue
		}
		if s.CalibrationConnected == nil || s.RestoreKProfiles == nil || !s.CalibrationConnected(id) {
			t.Skipped += n
			t.note("kprofilesPrinterOffline", "A snapshot printer is disconnected")
			continue
		}
		confirmed, e := s.RestoreKProfiles(ctx, id, f.nozzle, f.profiles)
		if e != nil {
			t.Failed += n
			t.note("kprofilesSendFailed", e.Error())
			continue
		}
		t.Restored += n
		if !confirmed {
			t.note("kprofilesAckUnreliable", "A submitted write has no firmware acknowledgement. Verify it on the printer")
		}
	}
}
