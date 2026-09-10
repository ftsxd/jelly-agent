package server

// Running the delivery store's expiry on a timer.
//
// Started once and resolves the live engine on every tick, so a config
// hot-reload changes the retention this obeys without leaving the previous
// engine's sweeper running against a closed store.
//
// The interval is deliberately much shorter than the retention it enforces. A
// sweep that ran as rarely as the retention period would make the real
// lifetime anything between one and two retention periods depending on when
// the process happened to start — a promise nobody could reason about.

import (
	"context"
	"log/slog"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

const sweepInterval = time.Hour

// StartResultSweeper expires overdue tool results until ctx is done.
func (s *Server) StartResultSweeper(ctx context.Context) {
	sweep := func() {
		eng, unpin := s.pin()
		defer unpin()
		if !eng.RetentionEnabled() {
			return
		}
		n, err := eng.SweepResults(ctx)
		if err != nil {
			// Logged and dropped. A sweep that cannot run is a storage-growth
			// problem, not a correctness one — nothing the model reads depends
			// on it, so failing louder would trade a slow leak for an outage.
			slog.Warn("清理过期结果失败", logging.Err(err))
			return
		}
		if n > 0 {
			slog.Info("已清理过期结果", "条数", n)
		}
	}

	// At startup as well as on the tick: a process that ran for a week and
	// restarted would otherwise carry a week of overdue rows until the first
	// tick fired.
	sweep()

	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				sweep()
			}
		}
	}()
}
