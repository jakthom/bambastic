package production

import (
	"context"
	"time"
)

func (s *Service) flushEvents(ctx context.Context) {
	s.mu.RLock()
	sink := s.sink
	s.mu.RUnlock()
	if sink == nil {
		return
	}
	rs, e := records(ctx, s.db, "SELECT * FROM bb_production_events ORDER BY id LIMIT 128")
	if e != nil {
		return
	}
	for _, v := range rs {
		event := row{"type": v["kind"], "event_id": v["id"], "printer_id": v["printer_id"], "queue_item_id": v["queue_item_id"], "run_id": v["run_id"], "archive_id": v["archive_id"]}
		if v["awaiting_plate_clear"] != nil {
			event["awaiting_plate_clear"] = flag(v["awaiting_plate_clear"])
		}
		event["data"] = row{"printer_id": v["printer_id"], "queue_item_id": v["queue_item_id"], "run_id": v["run_id"], "archive_id": v["archive_id"], "awaiting_plate_clear": event["awaiting_plate_clear"]}
		sink(event)
		if _, e = s.db.ExecContext(ctx, "DELETE FROM bb_production_events WHERE id=?", v["id"]); e != nil {
			return
		}
	}
}
func (s *Service) eventLoop(ctx context.Context) {
	t := time.NewTicker(250 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.flushEvents(ctx)
		}
	}
}
