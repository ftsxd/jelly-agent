package server

import (
	"context"
	"log/slog"
	"time"
)

// Pins the live engine per pass, including when execution has been disabled:
// revocation stops new work but must not strand previously created resources.
func (s *Server) StartExecutionRecovery(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			eng, release := s.pin()
			journal, err := eng.ExecutionJournal()
			if err == nil {
				pass, done := context.WithTimeout(ctx, 25*time.Second)
				var n int
				n, err = journal.Recover(pass)
				done()
				if n > 0 {
					slog.Info("已回收遗留执行资源", "count", n)
				}
			}
			release()
			if err != nil && ctx.Err() == nil {
				slog.Warn("执行资源恢复未完成，已保留记录", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
