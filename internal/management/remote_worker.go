package management

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
	"sync"
	"time"
)

// Start delivers committed remote material usage. Spoolman's use API is additive and has no
// idempotency token. A persisted sending record preserves an uncertain delivery for reconciliation.
func (s *Service) Start(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		seen := map[slotKey]trayObservation{}
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			if e := s.observeAMS(ctx, time.Now(), seen); e != nil && ctx.Err() == nil {
				log.Printf("AMS inventory reconciliation: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	go func() { workers.Wait(); close(done) }()
	go func() {
		defer workers.Done()
		tick := time.NewTicker(3 * time.Second)
		defer tick.Stop()
		for {
			if e := s.deliverStationUpdates(ctx); e != nil && ctx.Err() == nil {
				log.Printf("SpoolBuddy updates: %v", e)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	go func() {
		defer workers.Done()
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			if err := s.deliverUsage(ctx); err != nil && ctx.Err() == nil {
				log.Printf("Spoolman usage delivery: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	return done
}
func (s *Service) deliverUsage(ctx context.Context) error {
	// A pending run is only eligible against the original inventory installation.
	rows, e := s.db.QueryContext(ctx, `SELECT id FROM bambastic_material_allocations
 WHERE state='pending' AND trim(base_url,'/')=trim((SELECT value FROM settings WHERE key='spoolman_url'),'/')
 AND coalesce((SELECT value FROM settings WHERE key='bambastic_spoolman_disconnected'),'false')!='true' AND lower((SELECT value FROM settings WHERE key='spoolman_enabled'))='true' ORDER BY run_id LIMIT 64`)
	if e != nil {
		return e
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if e = rows.Scan(&id); e != nil {
			rows.Close()
			return e
		}
		ids = append(ids, id)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, id := range ids {
		if e = s.deliverRun(ctx, id); e != nil {
			return e
		}
	}
	return nil
}
func (s *Service) deliverRun(ctx context.Context, id int64) error {
	var spool int64
	var base string
	var grams float64
	e := s.db.QueryRowContext(ctx, "SELECT remote_spool_id,base_url,grams FROM bambastic_material_allocations WHERE id=? AND state='pending'", id).Scan(&spool, &base, &grams)
	if errors.Is(e, sql.ErrNoRows) {
		return nil
	}
	if e != nil {
		return e
	}
	path := fmt.Sprintf("spool/%d", spool)
	var v row
	// Check connectivity and resolve cost without an additive write. A failed read can safely retry.
	if e = remoteCall(ctx, base, "GET", path, nil, &v); e != nil {
		return e
	}
	if intID(v["id"]) != spool {
		return fmt.Errorf("Spoolman returned an unexpected spool for run %d", id)
	}
	res, e := s.db.ExecContext(ctx, "UPDATE bambastic_material_allocations SET state='sending',attempted_at=CURRENT_TIMESTAMP WHERE id=? AND state='pending'", id)
	if e != nil {
		return e
	}
	n, e := res.RowsAffected()
	if e != nil || n == 0 {
		return e
	}
	if e = remoteCall(ctx, base, "PUT", path+"/use", row{"use_weight": grams}, nil); e != nil {
		// Timeouts can occur after the remote system commits. An automatic retry could charge twice.
		if _, writeErr := s.db.ExecContext(ctx, "UPDATE bambastic_material_allocations SET state='review',last_error=? WHERE id=?", e.Error(), id); writeErr != nil {
			return writeErr
		}
		return e
	}
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	mapped := remoteSpool(v)
	net, _, _ := remoteWeights(v)
	pct := 0.
	if net > 0 {
		pct = math.Round(grams / net * 100)
	}
	var cost any
	if rate := mapped["effective_cost_per_kg"]; rate != nil {
		cost = cents(num(rate) * grams / 1000)
	}
	if _, e = tx.Exec("UPDATE spool_usage_history SET percent_used=?,cost=? WHERE id=(SELECT usage_id FROM bambastic_material_allocations WHERE id=?)", pct, cost, id); e != nil {
		return e
	}
	if _, e = tx.Exec("UPDATE bambastic_material_allocations SET state='delivered',last_error=NULL WHERE id=?", id); e != nil {
		return e
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	if s.emit != nil {
		s.emit(row{"type": "inventory_changed"})
	}
	return nil
}
