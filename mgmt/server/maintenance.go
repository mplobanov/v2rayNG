package main

import (
	"context"
	"log"
	"time"
)

// maintenanceLoop expires undelivered commands every minute and applies retention hourly.
func (s *Server) maintenanceLoop(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	lastRetention := time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		now := time.Now().Unix()
		if _, err := s.st.Exec(`UPDATE commands SET status='expired' WHERE status IN ('queued','sent','running') AND expires<?`, now); err != nil {
			log.Printf("expire commands: %v", err)
		}
		if time.Since(lastRetention) < time.Hour {
			continue
		}
		lastRetention = time.Now()
		s.retention()
	}
}

func (s *Server) retention() {
	r := s.cfg.Retention
	now := time.Now().Unix()
	day := int64(86400)
	steps := []struct {
		q    string
		args []any
	}{
		{`DELETE FROM logs WHERE ts<?`, []any{now - int64(r.LogsDays)*day}},
		{`DELETE FROM logs WHERE id <= (SELECT id FROM logs ORDER BY id DESC LIMIT 1 OFFSET ?)`, []any{r.LogsMaxRows}},
		{`DELETE FROM states WHERE ts<?`, []any{now - int64(r.StatesDays)*day}},
		{`DELETE FROM events WHERE ts<?`, []any{now - int64(r.EventsDays)*day}},
		{`DELETE FROM measurements WHERE ts<?`, []any{now - int64(r.MeasurementsDays)*day}},
		{`DELETE FROM commands WHERE created<?`, []any{now - 60*day}},
		{`DELETE FROM audit WHERE ts<?`, []any{now - 365*day}},
	}
	for _, st := range steps {
		res, err := s.st.Exec(st.q, st.args...)
		if err != nil {
			log.Printf("retention %q: %v", st.q, err)
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			log.Printf("retention: %d rows (%s)", n, st.q)
		}
	}
	if _, err := s.st.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		log.Printf("checkpoint: %v", err)
	}
}
