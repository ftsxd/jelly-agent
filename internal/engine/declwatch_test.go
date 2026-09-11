package engine

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/storage"
	"github.com/jelly-agent/jelly-agent/internal/toolreg"
)

// A declaration saved by one process reaches another one's registry.
//
// This is the reason the console's layer was moved out of console.yaml and
// into the database: two processes on one database have to agree about what a
// tool is. tool_decl_log doubles as the notice — every process polls its
// highest id — and that half was built and then connected to nothing, so a
// save in one console updated that process alone and no other. Nothing failed
// and nothing was logged; the two just disagreed until a restart.
//
// Two engines on one database here, which is the same situation as two
// processes as far as the registry is concerned: each has its own store and
// its own watcher.
func TestADeclarationSavedElsewhereReachesThisRegistry(t *testing.T) {
	prev := declPoll
	declPoll = 20 * time.Millisecond // production asks every 3s; see declPoll
	t.Cleanup(func() { declPoll = prev })

	dir := t.TempDir()
	newEngine := func() *Engine {
		cfg := &config.Config{Storage: config.Storage{DSN: filepath.Join(dir, "state.db")}}
		cfg.Tools.MetadataDir = filepath.Join(dir, "tools")
		e := New(cfg)
		t.Cleanup(e.Close)
		return e
	}
	reader, writer := newEngine(), newEngine()

	// The reader's registry exists and knows nothing about the tool yet.
	if suitesOf(t, reader, "n9e-mcp", "query_range") != nil {
		t.Fatal("这个工具本来不该有声明")
	}

	db, err := writer.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "另一个进程"); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := suitesOf(t, reader, "n9e-mcp", "query_range"); len(got) == 1 && got[0] == "promql" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("另一个进程改了声明，这个进程的注册表一直没跟上 —— " +
				"tool_decl_log 的失效通知没有消费者")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func suitesOf(t *testing.T, e *Engine, server, name string) []string {
	t.Helper()
	reg, _ := e.toolRegistry()
	for _, m := range reg.Load().All() {
		if m.Server == server && m.Name == name {
			return m.Suites
		}
	}
	return nil
}

// A change that lands between the first load and the watcher starting must
// still arrive.
//
// The watcher used to read its baseline itself, inside its own goroutine and
// therefore after the registry had already loaded. A change committed in that
// gap is in the baseline and not in the registry — so nothing ever reports
// it, because the only trigger is the version moving past a baseline it has
// already passed. The registry stays wrong until somebody makes an unrelated
// edit, and nothing says so.
func TestAChangeLandingWhileTheWatcherStartsIsNotLost(t *testing.T) {
	prev, prevSeam := declPoll, afterFirstRegistry
	declPoll = 20 * time.Millisecond
	t.Cleanup(func() { declPoll, afterFirstRegistry = prev, prevSeam })

	dir := t.TempDir()
	cfg := &config.Config{Storage: config.Storage{DSN: filepath.Join(dir, "state.db")}}
	cfg.Tools.MetadataDir = filepath.Join(dir, "tools")
	e := New(cfg)
	t.Cleanup(e.Close)

	// Exactly in the window: the registry has loaded, the watcher has not
	// started, and another process saves a declaration.
	var once sync.Once
	afterFirstRegistry = func() {
		once.Do(func() {
			db, err := e.StateDB()
			if err != nil {
				t.Error(err)
				return
			}
			if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
				Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
			}, "另一个进程"); err != nil {
				t.Error(err)
			}
		})
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := suitesOf(t, e, "n9e-mcp", "query_range"); len(got) == 1 && got[0] == "promql" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("注册表刚建好、watcher 刚起来的那一瞬间改的声明，永远没被认出来 —— " +
				"基线读晚了，把这次变更算成了已见过")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The version is only spent on a change that actually got installed.
//
// The watcher advanced its version as soon as *its* load succeeded, then
// handed the set over and the engine threw it away and loaded the table
// again. Two loads, and only the first one decided whether the change was
// handled: if the second failed, buildRegistry quietly installed a registry
// with no overlay in it — every declaration gone, back to built-in defaults —
// while the watcher had already recorded the version as done. Nothing
// retried, because the only trigger is the version moving, and it had moved.
// The process then disagreed with the database until somebody made an
// unrelated edit.
func TestAChangeIsInstalledFromWhatTheWatchAlreadyRead(t *testing.T) {
	prev, prevSeam := declPoll, beforeWatchSwap
	declPoll = 20 * time.Millisecond
	t.Cleanup(func() { declPoll, beforeWatchSwap = prev, prevSeam })

	e, db := engineOnFreshDB(t)

	// The database becomes unreadable in the instant between the change
	// being read and being installed. Installing it must not need it.
	var once sync.Once
	beforeWatchSwap = func() {
		once.Do(func() {
			if _, err := db.Exec(`UPDATE tool_decls SET use_cases = ?`, `{}`); err != nil {
				t.Error(err)
			}
		})
	}

	if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "另一个进程"); err != nil {
		t.Fatal(err)
	}

	waitForSuites(t, e, "n9e-mcp", "query_range", "promql",
		"变更是读到了，安装的时候却又去读了一遍库 —— 那一遍失败，注册表退回内置默认值，"+
			"而版本号已经算作处理过了，没有下一次变更就永远不会重来")
}

// A load that fails at startup is made up for, without a new change.
//
// The watcher used to start from whatever version it read at that moment, so
// an initial load that failed — a database briefly unreachable, a row being
// migrated — left the registry without its overlay and the watcher already
// past the change that would have filled it in. Every declaration silently
// stopped applying until somebody made another edit.
func TestAFailedFirstLoadIsMadeUpFor(t *testing.T) {
	prev := declPoll
	declPoll = 20 * time.Millisecond
	t.Cleanup(func() { declPoll = prev })

	dir := t.TempDir()
	ref := filepath.Join(dir, "state.db")
	seed := newEngineAt(t, dir, ref)
	db, err := seed.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "alice"); err != nil {
		t.Fatal(err)
	}
	// Unreadable before this process ever looks at it.
	if _, err := db.Exec(`UPDATE tool_decls SET use_cases = ?`, `{}`); err != nil {
		t.Fatal(err)
	}

	e := newEngineAt(t, t.TempDir(), ref) // its first load fails
	if got := suitesOf(t, e, "n9e-mcp", "query_range"); got != nil {
		t.Fatalf("这一次加载本来就该失败: %v", got)
	}

	// Repaired, and no new change: the log has not moved, so the only thing
	// that can fill the registry in is a watcher that never claimed to have
	// this version.
	if _, err := db.Exec(`UPDATE tool_decls SET use_cases = NULL`); err != nil {
		t.Fatal(err)
	}
	waitForSuites(t, e, "n9e-mcp", "query_range", "promql",
		"启动时那次加载失败了，watcher 却从当前版本开始 —— 声明再也补不回来")
}

func newEngineAt(t *testing.T, metaDir, ref string) *Engine {
	t.Helper()
	cfg := &config.Config{Storage: config.Storage{DSN: ref}}
	cfg.Tools.MetadataDir = filepath.Join(metaDir, "tools")
	e := New(cfg)
	t.Cleanup(e.Close)
	return e
}

func engineOnFreshDB(t *testing.T) (*Engine, *storage.DB) {
	t.Helper()
	dir := t.TempDir()
	e := newEngineAt(t, dir, filepath.Join(dir, "state.db"))
	if got := suitesOf(t, e, "n9e-mcp", "query_range"); got != nil {
		t.Fatal("这个工具本来不该有声明")
	}
	db, err := e.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	return e, db
}

func waitForSuites(t *testing.T, e *Engine, server, name, want, msg string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if got := suitesOf(t, e, server, name); len(got) == 1 && got[0] == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal(msg)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A database that will not open at startup must be made up for when it does.
//
// The registry's first build asks for the state database; if that fails there
// is no overlay and — because the handle was opened once and its failure
// remembered — nothing in the process would ever ask again. Every console
// declaration silently stopped applying for as long as the process ran, over
// a database that was down for one second at boot.
func TestADatabaseThatOpensLateIsPickedUp(t *testing.T) {
	prev := declPoll
	declPoll = 20 * time.Millisecond
	t.Cleanup(func() { declPoll = prev })

	dir := t.TempDir()
	ref := filepath.Join(dir, "state.db")
	// Not a database, so opening it fails the way an unreachable one does.
	if err := os.WriteFile(ref, []byte("这不是一个数据库文件"), 0o600); err != nil {
		t.Fatal(err)
	}

	e := newEngineAt(t, dir, ref)
	if _, err := e.StateDB(); err == nil {
		t.Fatal("这一次打开本来就该失败")
	}
	if got := suitesOf(t, e, "n9e-mcp", "query_range"); got != nil {
		t.Fatalf("库都打不开，声明是哪来的: %v", got)
	}

	// The database comes back: the file is replaced by a real one holding a
	// declaration. Nothing in this process has been told.
	if err := os.Remove(ref); err != nil {
		t.Fatal(err)
	}
	seed := newEngineAt(t, t.TempDir(), ref)
	db, err := seed.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	waitForSuites(t, e, "n9e-mcp", "query_range", "promql",
		"启动时库打不开，之后就再也没有人重试过 —— 控制台声明这一层永久失效")
}

// A rebuild that did not happen must not count as handled.
//
// The version says "the registry has this change in it". Spending it on a
// build that failed — an unreadable metadata file is enough, and the fallback
// then installs built-in defaults — leaves the registry wrong with nothing
// left to correct it: the only trigger is the version moving, and it has
// already moved.
func TestAFailedRebuildDoesNotSpendTheVersion(t *testing.T) {
	prev, prevSeam := declPoll, beforeWatchSwap
	declPoll = 20 * time.Millisecond
	t.Cleanup(func() { declPoll, beforeWatchSwap = prev, prevSeam })

	dir := t.TempDir()
	e := newEngineAt(t, dir, filepath.Join(dir, "state.db"))
	metaDir := filepath.Join(dir, "tools")
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(metaDir, "broken.yaml")

	// One of the other layers breaks in the instant before the install, so
	// the rebuild fails. Held that way until the test repairs it.
	var once sync.Once
	beforeWatchSwap = func() {
		once.Do(func() {
			if err := os.WriteFile(broken, []byte("tools: [oh: dear\n"), 0o644); err != nil {
				t.Error(err)
			}
		})
	}

	// Force the first build now, so what arrives later can only have come
	// through the watcher.
	if got := suitesOf(t, e, "n9e-mcp", "query_range"); got != nil {
		t.Fatal("这个工具本来不该有声明")
	}

	db, err := e.StateDB()
	if err != nil {
		t.Fatal(err)
	}
	if err := toolreg.SaveDecl(context.Background(), db, toolreg.Decl{
		Server: "n9e-mcp", Name: "query_range", Suites: &[]string{"promql"},
	}, "alice"); err != nil {
		t.Fatal(err)
	}

	// While the file is broken nothing may be installed.
	time.Sleep(200 * time.Millisecond)
	if got := suitesOf(t, e, "n9e-mcp", "query_range"); got != nil {
		t.Fatalf("重建失败了，却装上了一份注册表: %v", got)
	}

	// Repaired, and no new change: only a version that was never spent can
	// still deliver this one.
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	waitForSuites(t, e, "n9e-mcp", "query_range", "promql",
		"重建失败的那一轮把版本号花掉了 —— 文件修好之后再也没有人补上这次变更")
}
