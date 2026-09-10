package engine

import (
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// The n9e declarations reach the registry with no deployment step.
//
// They used to live at configs/tools/n9e.yaml and be loaded only if somebody
// pointed tools.metadata_dir at it. Nobody did, and configs/ is not in the
// container image at all, so six of these tools ran with no produces, no side
// effect and no suites — which degrades every path that scores or groups by
// them, silently, on the deployment this agent was built for.
//
// Asserted through the registry rather than through the source, because what
// matters is the value that survives merging, conflict resolution and the
// overlay — not that a file parses.
func TestBundledDeclarationsReachTheRegistry(t *testing.T) {
	e := New(&config.Config{Storage: config.Storage{
		DSN: filepath.Join(t.TempDir(), "state.db"),
	}})
	t.Cleanup(e.Close)
	reg := e.ToolRegistry()
	if reg == nil {
		t.Fatal("no registry")
	}

	// The six that had nothing, and the produces each was written for. Read
	// from the file rather than assumed: an assertion that agreed with a
	// guess would pass against a file that says something else.
	for name, want := range map[string]ops.EvidenceKind{
		"list_active_alerts":  "events",
		"list_history_alerts": "events",
		"list_alert_rules":    "config",
		"list_busi_groups":    "config",
		"list_dashboards":     "config",
		"get_dashboard_pure":  "config",
	} {
		m, ok := reg.Lookup(name)
		if !ok {
			t.Errorf("%s 不在注册表里", name)
			continue
		}
		if m.Produces != want {
			t.Errorf("%s produces = %q, want %q", name, m.Produces, want)
		}
		if m.SideEffect == "" {
			t.Errorf("%s 没有声明副作用", name)
		}
	}
}

// A bundled declaration must not carry a backend.
//
// Registry.Available filters by it, and the only incident this process ever
// builds (incidentFor) has no targets — so Backends() is empty and anything
// carrying a backend is filtered out of every path, including the prompt page
// that counts tools. Shipping the file with `backend: n9e` on ten entries
// would have hidden all ten.
func TestBundledDeclarationsCarryNoBackend(t *testing.T) {
	e := New(&config.Config{Storage: config.Storage{
		DSN: filepath.Join(t.TempDir(), "state.db"),
	}})
	t.Cleanup(e.Close)
	reg := e.ToolRegistry()

	visible := map[string]bool{}
	for _, m := range reg.Available(nil) {
		visible[m.Name] = true
	}
	// Every tool the bundled file declares. query_logs is deliberately absent
	// from that list: it was only ever declared in one operator's own file,
	// which is the layer above this one.
	for _, name := range []string{
		"query_instant", "query_range", "list_targets", "list_datasources",
		"list_active_alerts", "list_history_alerts", "list_alert_rules",
		"list_busi_groups", "list_dashboards", "get_dashboard_pure",
	} {
		if m, ok := reg.Lookup(name); ok && m.Backend != "" {
			t.Errorf("%s 带了 backend %q —— 它会从每一个无 incident 的视图里消失", name, m.Backend)
		}
		if !visible[name] {
			t.Errorf("%s 对 Available(nil) 不可见", name)
		}
	}
}
