package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/config"
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
