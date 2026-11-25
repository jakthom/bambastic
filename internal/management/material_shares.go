package management

import (
	"context"
	"database/sql"
	"errors"
	"math"
)

func (o runEvidence) progress(complete bool) float64 {
	if complete {
		return 1
	}
	if o.Layers > 0 && o.Layer > 0 {
		return min(1, float64(o.Layer)/float64(o.Layers))
	}
	return float64(o.Percent) / 100
}
func (o runEvidence) routes(tool int, single bool) []feederInterval {
	if single && len(o.Feed) > 0 {
		return o.Feed
	}
	addr := -1
	if tool >= 0 && tool < len(o.Mapping) && o.Mapping[tool] >= 0 && o.Mapping[tool] != 255 {
		addr = o.Mapping[tool]
	}
	if addr < 0 && single {
		seen := map[int]bool{}
		for _, n := range o.Mapping {
			if n >= 0 && n != 255 {
				seen[n] = true
			}
		}
		if len(seen) == 0 {
			for n, h := range o.Holders {
				if len(h.Spools) > 0 {
					seen[n] = true
				}
			}
		}
		if len(seen) == 1 {
			for n := range seen {
				addr = n
			}
		}
	}
	if addr >= 0 {
		return []feederInterval{{0, addr}}
	}
	return nil
}

type materialShare struct {
	spool    int64
	from, to int
}

// splitTool computes the intersection of the active tray and its physical spool
// history. Unknown intervals participate in the denominator and receive no debit.
func (o runEvidence) splitTool(tool int, grams float64, end int, feeds []feederInterval, layers *layerExtrusion, charges map[int64]float64, covered map[int]bool) {
	var spans []materialShare
	for i, f := range feeds {
		covered[f.Tray] = true
		from, to := f.Layer, end
		if i+1 < len(feeds) {
			to = min(to, feeds[i+1].Layer)
		}
		if from > to {
			continue
		}
		h := o.Holders[f.Tray]
		if h == nil || len(h.Spools) == 0 {
			spans = append(spans, materialShare{0, from, to})
			continue
		}
		for j, s := range h.Spools {
			last := to
			if j+1 < len(h.Spools) {
				last = min(last, h.Spools[j+1].Layer)
			}
			start := max(from, s.Layer)
			if last > start || from == to && start == last && len(h.Spools) == 1 {
				spans = append(spans, materialShare{s.Spool, start, last})
			}
		}
	}
	if len(spans) == 1 {
		if spans[0].spool > 0 && (end == 0 || spans[0].from == 0 && spans[0].to == end) {
			charges[spans[0].spool] += grams
		}
		return
	}
	if len(spans) == 0 || end <= 0 {
		return
	}
	weights := make([]float64, len(spans))
	sum := 0.
	for i, s := range spans {
		w := math.Max(0, layers.through(tool, s.to)-layers.through(tool, s.from))
		weights[i] = w
		sum += w
	}
	if sum <= 0 {
		for i, s := range spans {
			w := float64(max(0, s.to-s.from))
			weights[i] = w
			sum += w
		}
	}
	if sum <= 0 {
		return
	}
	for i, s := range spans {
		if s.spool > 0 {
			charges[s.spool] += grams * weights[i] / sum
		}
	}
}

// AttributeMaterial freezes exact grams for evidence-backed allocations before
// SettleRun posts them. Legacy sessions that predate the evidence table keep their
// existing frozen allocations. Run totals can include confidently sliced but unassigned material.
func AttributeMaterial(ctx context.Context, tx *sql.Tx, rid int64, complete bool, plateGrams float64, tools map[int]float64, inputs MaterialCloseInputs) (*float64, error) {
	o, e := readRunEvidence(ctx, tx, rid)
	if errors.Is(e, sql.ErrNoRows) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	if inputs.RunID != rid {
		inputs = MaterialCloseInputs{}
	}
	var settled bool
	if e = tx.QueryRowContext(ctx, "SELECT settled FROM bambastic_material_runs WHERE run_id=?", rid).Scan(&settled); e != nil {
		return nil, e
	}
	if settled {
		return nil, nil
	}
	x := inputs.layers
	end := o.Layer
	if complete {
		end = max(end, o.Layers)
		if x != nil {
			end = max(end, x.end)
		}
	}
	if end == 0 && !complete && o.Layers > 0 && o.Percent > 0 {
		end = int(math.Round(float64(o.Layers*o.Percent) / 100))
	}
	stated := 0.
	for _, g := range tools {
		stated += g
	}
	if len(tools) == 0 && plateGrams > 0 {
		tools = map[int]float64{-1: plateGrams}
		stated = plateGrams
	}
	charges := map[int64]float64{}
	covered, used := map[int]bool{}, map[int]bool{}
	for _, addr := range o.Mapping {
		if addr >= 0 && addr != 255 {
			used[addr] = true
		}
	}
	for _, f := range o.Feed {
		used[f.Tray] = true
	}
	proportion := o.progress(complete)
	runGrams := max(0, plateGrams-stated) * proportion
	known := false
	for tool, full := range tools {
		if full <= 0 {
			continue
		}
		scale := proportion
		supported := complete || proportion > 0
		if !complete && x != nil && o.Layer > 0 {
			if total := x.through(tool, x.end); total > 0 {
				scale = min(1, x.through(tool, o.Layer)/total)
				supported = true
			}
		}
		grams := full * scale
		runGrams += grams
		if supported {
			known = true
			o.splitTool(tool, grams, end, o.routes(tool, len(tools) == 1), x, charges, covered)
		}
	}
	for addr, h := range o.Holders {
		if covered[addr] || len(used) > 0 && !used[addr] || h.StartRemain == nil || h.EndRemain == nil || *h.StartRemain <= *h.EndRemain || h.ChangedTag || len(h.Spools) != 1 || h.Spools[0].Spool == 0 {
			continue
		}
		sp := h.Spools[0].Spool
		net := inputs.remoteNet[sp]
		if !o.Remote {
			e = tx.QueryRowContext(ctx, "SELECT label_weight FROM spool WHERE id=?", sp).Scan(&net)
			if errors.Is(e, sql.ErrNoRows) {
				continue
			}
			if e != nil {
				return nil, e
			}
		}
		if net <= 0 {
			continue
		}
		grams := math.Round(float64(*h.StartRemain-*h.EndRemain)*net) / 100
		if grams > 0 {
			charges[sp] += grams
			known = true
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM bambastic_material_allocations WHERE run_id=? AND state IN ('tracking','attributed')", rid); e != nil {
		return nil, e
	}
	allocated := 0.
	for sp, grams := range charges {
		if grams <= 0 || math.IsNaN(grams) || math.IsInf(grams, 0) {
			continue
		}
		var native, remote any
		if o.Remote {
			remote = sp
		} else {
			n, e := count(tx, "SELECT count(*) FROM spool WHERE id=?", sp)
			if e != nil {
				return nil, e
			}
			if n == 0 {
				continue
			}
			native = sp
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO bambastic_material_allocations(run_id,spool_id,remote_spool_id,base_url,fraction,grams,state) VALUES(?,?,?,?,1,?,'attributed')`, rid, native, remote, o.Base, grams)
		if e != nil {
			return nil, e
		}
		allocated += grams
	}
	if !known {
		return nil, nil
	}
	runGrams = max(runGrams, allocated)
	return &runGrams, nil
}
