package engine

// Expiring stored deliveries.
//
// One sweep, driven from outside. The loop lives on the server rather than
// here because the server swaps the engine on a config hot-reload: a ticker
// owned by an engine would keep running against a store that had been closed
// under it, and each reload would leave another one behind.

import (
	"context"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/record"
)

// retention resolves the configured retention window.
//
// Negative keeps everything; zero means unset and takes the default. The two
// have to stay distinct, which is why config holds a raw int rather than a
// duration — a duration cannot express "the operator has not chosen".
func (e *Engine) retention() time.Duration {
	d := e.cfg.Tools.ResultRetentionDays
	switch {
	case d < 0:
		return 0 // keep everything
	case d == 0:
		return record.DefaultRetention
	default:
		return time.Duration(d) * 24 * time.Hour
	}
}

// SweepResults expires deliveries past their retention, reporting how many.
//
// A zero count with a nil error is the normal answer, and also the answer when
// retention is off — the caller does not need to ask first.
func (e *Engine) SweepResults(ctx context.Context) (int64, error) {
	keep := e.retention()
	if keep <= 0 {
		return 0, nil
	}
	store, err := e.records()
	if err != nil {
		return 0, err
	}
	return store.Sweep(ctx, keep)
}

// RetentionEnabled reports whether anything will ever be expired, so a caller
// can say so once at startup instead of ticking silently forever.
func (e *Engine) RetentionEnabled() bool { return e.retention() > 0 }
