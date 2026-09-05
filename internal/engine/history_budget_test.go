package engine

import (
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
)

func budgetEngine(t *testing.T, historyMax *int) *Engine {
	t.Helper()
	cfg := &config.Config{}
	cfg.History.MaxTokens = historyMax
	return New(cfg)
}

func ptr(n int) *int { return &n }

// A history budget above the context window cannot be meant.
//
// It does not say "compact later"; it says "never compact", and the
// conversation then grows until the provider rejects the request outright.
// "Never compact" already has a spelling — an explicit zero — so a number
// above the window is a silent mistake, showing up only as a 400 on a long
// session. Found in a real deployment: ten million against a one-million
// window, which would have made adding context_window look like it fixed
// nothing.
func TestAHistoryBudgetAboveTheWindowIsNotObeyed(t *testing.T) {
	e := budgetEngine(t, ptr(10_000_000))
	got := e.historyBudgetFor(1_000_000)
	want := int(float64(1_000_000) * historyShare)
	if got != want {
		t.Errorf("budget = %d, want the derived %d — ten million against a one-million window was taken at face value", got, want)
	}
}

// Everything else about the setting still holds: the operator's number wins,
// and an explicit zero still means "off".
func TestExplicitHistoryBudgetsAreStillObeyed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		max    *int
		window int
		want   int
	}{
		{"显式值在窗口以内", ptr(24_000), 1_000_000, 24_000},
		{"显式 0 表示关闭压缩", ptr(0), 1_000_000, 0},
		{"未设置则按窗口推导", nil, 1_000_000, 600_000},
		{"未设置且窗口未知则交给 history 包的默认值", nil, 0, 0},
		{"窗口未知时无法判断超限，尊重显式值", ptr(10_000_000), 0, 10_000_000},
		{"正好等于窗口不算超限", ptr(1_000_000), 1_000_000, 1_000_000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := budgetEngine(t, tc.max).historyBudgetFor(tc.window); got != tc.want {
				t.Errorf("budget = %d, want %d", got, tc.want)
			}
		})
	}
}
