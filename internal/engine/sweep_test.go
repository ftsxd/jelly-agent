package engine

import (
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/record"
)

// Zero and negative are different answers, and config holds a raw int so they
// stay different: zero is "the operator has not chosen" and takes the default,
// negative is "keep everything". A duration could not express the first.
func TestRetentionDistinguishesUnsetFromDisabled(t *testing.T) {
	for _, tc := range []struct {
		days int
		want time.Duration
	}{
		{days: 0, want: record.DefaultRetention},
		{days: -1, want: 0},
		{days: 30, want: 30 * 24 * time.Hour},
		{days: 1, want: 24 * time.Hour},
	} {
		e := New(&config.Config{Tools: config.Tools{ResultRetentionDays: tc.days}})
		if got := e.retention(); got != tc.want {
			t.Errorf("%d days → %v, want %v", tc.days, got, tc.want)
		}
		if enabled := e.RetentionEnabled(); enabled != (tc.want > 0) {
			t.Errorf("%d days → enabled=%v, want %v", tc.days, enabled, tc.want > 0)
		}
	}
}

// Sweeping with retention off must not report an error, so a caller does not
// have to ask permission before every tick.
func TestSweepWithRetentionOffIsAQuietNoop(t *testing.T) {
	e := New(&config.Config{Tools: config.Tools{ResultRetentionDays: -1}})
	n, err := e.SweepResults(t.Context())
	if err != nil || n != 0 {
		t.Errorf("sweep with retention off = %d, %v; want 0, nil", n, err)
	}
}
