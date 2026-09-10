package engine

import (
	"context"
	adksession "google.golang.org/adk/session"
	"os"
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

// The console.yaml import must not touch a directory this process did not
// configure.
//
// ToolMetadataDir falls back to ~/.jelly-agent/tools when nothing is set, so
// a process pointed at some other database — a test with a temp file, a
// one-off run against a copy — would import the real deployment's file into
// its own database and rename the original out of the way. Reading it there
// is harmless; renaming it is not, and it happened: a test run moved this
// developer's console.yaml to console.yaml.imported while writing the rows
// into a temporary database.
func TestTheImportOnlyTouchesAConfiguredDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want bool
	}{
		{"什么都没配", &config.Config{}, false},
		{"只配了库", &config.Config{Storage: config.Storage{DSN: "/tmp/x.db"}}, false},
		{"显式配了元数据目录", func() *config.Config {
			c := &config.Config{}
			c.Tools.MetadataDir = "/tmp/tools"
			return c
		}(), true},
		{"配置来自文件（目录随它推导）", &config.Config{SourcePath: "/etc/jelly/config.yaml"}, true},
		{"配置来自环境变量", &config.Config{SourcePath: "(env)"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := New(tc.cfg)
			if got := e.importableMetadataDir() != ""; got != tc.want {
				t.Errorf("importable = %v, want %v (dir=%q)", got, tc.want, e.importableMetadataDir())
			}
		})
	}
}

// And the fallback still applies to reading, which was never the problem.
func TestReadingStillFallsBackToTheDefaultDirectory(t *testing.T) {
	e := New(&config.Config{})
	if e.ToolMetadataDir() == "" {
		t.Error("默认目录没了 —— 这会让不配置任何东西的部署读不到自己的声明")
	}
}

// The state database sits beside the config, the same way the metadata
// directory does.
//
// They used to disagree: config.ToolMetadataDir derives from where the config
// came from, while DefaultDBPath was hardcoded at ~/.jelly-agent/state.db. A
// deployment whose config lived anywhere else read its declarations from one
// place and kept its database in another — and the console.yaml import, which
// reads one and writes the other, then moved a file out of one deployment and
// put its rows in another.
func TestTheStateDatabaseSitsBesideTheConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	elsewhere := t.TempDir()

	e := New(&config.Config{SourcePath: filepath.Join(elsewhere, "config.yaml")})
	got, err := e.stateReference()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(elsewhere, "state.db"); got != want {
		t.Errorf("state db = %q, want %q", got, want)
	}

	// With no config file it is still the home default, which is what an
	// untouched deployment gets.
	bare := New(&config.Config{})
	got, err = bare.stateReference()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".jelly-agent", "state.db"); got != want {
		t.Errorf("no-config state db = %q, want %q", got, want)
	}
}

// A process that did not configure a metadata directory must not rename a
// file in the default one.
//
// This is the failure as it happened: a test run, whose engine had a temp
// database and no metadata directory, imported this developer's real
// ~/.jelly-agent/tools/console.yaml into that temp database and renamed the
// original to console.yaml.imported. The rows went nowhere useful; the
// deployment's declarations stopped applying.
func TestOpeningWithNoConfiguredDirectoryLeavesTheDefaultOneAlone(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	tools := filepath.Join(home, ".jelly-agent", "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	console := filepath.Join(tools, "console.yaml")
	const body = "tools:\n  - name: query_range\n    server: n9e-mcp\n    suites: [promql]\n"
	if err := os.WriteFile(console, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	// A database somewhere else entirely, and no metadata directory named.
	e := New(&config.Config{Storage: config.Storage{
		DSN: filepath.Join(t.TempDir(), "state.db"),
	}})
	t.Cleanup(e.Close)
	if _, err := e.StateDB(); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(console); err != nil {
		t.Errorf("没有配置过的目录里的 console.yaml 被动了: %v", err)
	}
	if got, err := os.ReadFile(console); err == nil && string(got) != body {
		t.Error("文件内容被改了")
	}
	if _, err := os.Stat(console + ".imported"); err == nil {
		t.Error("文件被改名了 —— 这正是那次事故")
	}
}

// The session store lives in the same database as everything that joins
// against it.
//
// NewSessionService took the raw field rather than the resolved reference, so
// a deployment with a config file but no DSN put sessions and events in
// ~/.jelly-agent/state.db while tool results, call records, task links and
// the console's declarations went beside the config. Every join in the
// console is (session_id, invocation_id), so the two halves being apart means
// the console shows a session with no work in it and a task with no session.
func TestTheSessionStoreUsesTheSameDatabaseAsEverythingElse(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	elsewhere := t.TempDir()

	e := New(&config.Config{SourcePath: filepath.Join(elsewhere, "config.yaml")})
	t.Cleanup(e.Close)

	svc, err := e.NewSessionService()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(context.Background(), &adksession.CreateRequest{
		AppName: AppName, UserID: UserID, SessionID: "s1",
	}); err != nil {
		t.Fatal(err)
	}

	// The session must be visible through the shared handle — that is what
	// "same database" means, and it is what every join needs.
	db, err := e.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE id = ?`, "s1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("会话不在共享库里 —— 会话服务写到别处去了")
	}
	// And not in the home default, which is where it used to go.
	if _, err := os.Stat(filepath.Join(home, ".jelly-agent", "state.db")); err == nil {
		t.Error("会话服务在 home 下建了第二个库")
	}
}
