package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/config"
	"github.com/jelly-agent/jelly-agent/internal/gateway"
	jellytool "github.com/jelly-agent/jelly-agent/internal/tool"
)

// The rule the whole re-read loop rests on: a handle is published only if the
// bytes behind it were committed. If a failed store returned a label anyway,
// the model would be handed a reference it can never resolve — and it would
// reason from the empty answer rather than treat it as a fault.
//
// Tested here rather than only at the gateway, because the gateway's tests use
// a fake keeper: they prove the gateway honours a failure, not that this
// keeper reports one.
func TestKeeperPublishesNoHandleWhenStorageFails(t *testing.T) {
	e := New(&config.Config{})
	// A directory where the database file should be: Open fails, and it fails
	// the way a real disk problem does rather than through an injected fake.
	dir := t.TempDir()
	if err := writeDirAt(filepath.Join(dir, "state.db")); err != nil {
		t.Fatal(err)
	}
	e.SetSessionDBPath(filepath.Join(dir, "state.db"))

	label, err := e.keepToolResult(t.Context(),
		gateway.CallMeta{SessionID: "s1", InvocationID: "inv1", CallID: "c1"},
		"get_logs", map[string]any{"output": "anything"})
	if err == nil {
		t.Fatal("a failed store reported success; the gateway would publish a dangling handle")
	}
	if label != "" {
		t.Errorf("label = %q, want empty — a handle was minted for bytes that were never stored", label)
	}
}

// The other half of the same rule, and the one that actually bites: the store
// opened fine and the write is what failed. An open failure is caught early
// and loudly; a write failure happens mid-turn, with a tool that has already
// run, and is exactly where a handle could be minted for bytes that are not
// there.
func TestKeeperPublishesNoHandleWhenTheWriteFails(t *testing.T) {
	e := New(&config.Config{})
	e.SetSessionDBPath(filepath.Join(t.TempDir(), "state.db"))

	store, err := e.Records()
	if err != nil {
		t.Fatal(err)
	}
	// Closed under it: the engine caches the store, so every later write goes
	// to this handle and fails the way a dead database does.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	label, err := e.keepToolResult(t.Context(),
		gateway.CallMeta{SessionID: "s1", InvocationID: "inv1", CallID: "c1"},
		"get_logs", map[string]any{"output": "anything"})
	if err == nil {
		t.Fatal("a failed write reported success; the gateway would publish a dangling handle")
	}
	if label != "" {
		t.Errorf("label = %q, want empty — a handle was minted for bytes that were never stored", label)
	}
}

func TestKeeperPublishesAHandleWhenStorageSucceeds(t *testing.T) {
	e := New(&config.Config{})
	e.SetSessionDBPath(filepath.Join(t.TempDir(), "state.db"))

	label, err := e.keepToolResult(t.Context(),
		gateway.CallMeta{SessionID: "s1", InvocationID: "inv1", CallID: "c1"},
		"get_logs", map[string]any{"output": strings.Repeat("x", 100)})
	if err != nil {
		t.Fatal(err)
	}
	if label == "" {
		t.Fatal("a successful store published no handle, so the result is unrecoverable")
	}
}

// The readers are exempt, and the exemption must be silent rather than an
// error: their output already came out of the store, so there is nothing new
// to keep and nothing wrong either.
func TestKeeperExemptsItsOwnReaders(t *testing.T) {
	e := New(&config.Config{})
	e.SetSessionDBPath(filepath.Join(t.TempDir(), "state.db"))

	for _, name := range []string{jellytool.ReadResultName, jellytool.SearchResultName} {
		label, err := e.keepToolResult(t.Context(),
			gateway.CallMeta{SessionID: "s1", InvocationID: "inv1", CallID: "c1"},
			name, map[string]any{"data": "x"})
		if err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if label != "" {
			t.Errorf("%s: handed out handle %q for a read of a read", name, label)
		}
	}
}

// writeDirAt makes the given path a directory, which is the simplest way to
// make opening it as a database fail for a filesystem reason.
func writeDirAt(path string) error { return os.MkdirAll(path, 0o755) }
