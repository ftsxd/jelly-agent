package server

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/jelly-agent/jelly-agent/internal/memory"
)

// Two paths to one file, and only one of them is the model's.
//
// What protects ENVIRONMENT.md is not that it is read-only — an operator has
// to be able to edit it, and the console is where they will. It is that the
// console's path and the agent's path are different: the agent's remember and
// forget resolve through memory.Target, and Target does not map to this file.
// Read-only would have locked out the one caller who is supposed to write.
func TestOnlyTheConsoleCanWriteTheEnvironmentFile(t *testing.T) {
	s := newTestServer(t)
	dir := t.TempDir()
	s.engine().Config().Memory.Core.Dir = dir
	core, err := s.engine().Core()
	if err != nil {
		t.Fatal(err)
	}

	// The console writes it.
	const truth = "- 腾讯云 Redis 未接入本平台"
	if w := do(t, s, "POST", "/api/memory/core",
		`{"target":"environment","content":"`+truth+`"}`); w.Code != http.StatusOK {
		t.Fatalf("console save: status = %d: %s", w.Code, w.Body.String())
	}
	onDisk, err := os.ReadFile(filepath.Join(core.Dir(), memory.EnvironmentFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(onDisk); got != truth+"\n" {
		t.Errorf("文件内容 = %q，想要 %q", got, truth+"\n")
	}

	// And it reads back on the same endpoint, so the page shows what it wrote.
	w := do(t, s, "GET", "/api/memory/core", "")
	var body struct {
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Environment != truth {
		t.Errorf("读回 %q", body.Environment)
	}

	// The agent's own write path cannot reach it, whatever target it names.
	for _, target := range []memory.Target{
		memory.TargetMemory, memory.TargetUser, "", "environment", "env",
		memory.Target(memory.EnvironmentFile),
	} {
		_ = core.Remember(target, "- 腾讯云 Redis 已经接入了")
		_, _ = core.Forget(target, "腾讯云")
	}
	if got := core.Environment(); got != truth {
		t.Errorf("模型改写了运维声明的事实: %q", got)
	}
}
