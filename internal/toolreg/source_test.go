package toolreg

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFileSourceLoadsYAMLIncludingTheNamingFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "kubernetes.yaml", `
tools:
  - name: k8s_get_pods
    remote_name: get_pods
    server: kubernetes-prod
    aliases: [get_pods, list_pods]
    backend: kubernetes
    suites: [k8s]
    description: 列出 Pod 状态；不要用它查节点资源
    anti_examples: ["需要节点级指标时"]
    side_effect: read_only
    idempotent: true
    injected_params: [cluster, namespace]
    window_params: [start, end]
    arg_aliases:
      namespace: [ns, kubernetes_namespace]
    max_result_bytes: 4000
`)

	metas, err := NewFileSource(dir).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 {
		t.Fatalf("loaded %d entries, want 1", len(metas))
	}
	m := metas[0]

	if m.Name != "k8s_get_pods" || m.Remote() != "get_pods" || m.Server != "kubernetes-prod" {
		t.Errorf("identity fields wrong: %+v", m)
	}
	if len(m.Names()) != 3 {
		t.Errorf("Names = %v, want canonical plus two aliases", m.Names())
	}
	if !m.Injects("cluster") || !m.Injects("start") {
		t.Error("injected_params / window_params did not survive the parse")
	}
	if got := m.CanonicalArg("ns"); got != "namespace" {
		t.Errorf("CanonicalArg(ns) = %q, want namespace", got)
	}
	if !m.ReadOnly() || !m.Idempotent || m.MaxResultBytes != 4000 {
		t.Errorf("governance fields wrong: %+v", m)
	}
	if len(m.AntiExamples) != 1 {
		t.Error("anti_examples did not survive the parse")
	}
}

// Files load in a fixed order, so a conflict between two files is reproducible
// rather than depending on directory iteration.
func TestFileSourceLoadsInDeterministicOrder(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "b.yaml", "tools:\n  - name: b_tool\n    backend: b\n")
	writeFile(t, dir, "a.yaml", "tools:\n  - name: a_tool\n    backend: a\n")
	writeFile(t, dir, "notes.txt", "ignored")

	src := NewFileSource(dir)
	for i := 0; i < 3; i++ {
		metas, err := src.Load(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(metas) != 2 || metas[0].Name != "a_tool" {
			t.Fatalf("load %d: %v", i, metas)
		}
	}
}

// A tool that is configured but absent is far harder to trace than a parse
// error naming the file, so a bad file fails the load.
func TestFileSourceFailsLoudlyOnAMalformedFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "good.yaml", "tools:\n  - name: ok_tool\n")
	writeFile(t, dir, "bad.yaml", "tools:\n  - name: [this is not a string\n")

	_, err := NewFileSource(dir).Load(context.Background())
	if err == nil {
		t.Fatal("a malformed file was skipped silently")
	}
	if !strings.Contains(err.Error(), "bad.yaml") {
		t.Errorf("error %q does not name the offending file", err)
	}
}

// Metadata is optional; a deployment with none must still start.
func TestFileSourceTreatsAMissingDirectoryAsEmpty(t *testing.T) {
	metas, err := NewFileSource(filepath.Join(t.TempDir(), "absent")).Load(context.Background())
	if err != nil {
		t.Fatalf("a missing directory was an error: %v", err)
	}
	if len(metas) != 0 {
		t.Errorf("loaded %d entries from nothing", len(metas))
	}
}

func TestFileSourceWatchReportsAnEdit(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "tools.yaml", "tools:\n  - name: v1_tool\n")

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	ch := NewFileSource(dir).Watch(ctx)

	// Sleep past the poll interval so the mtime differs on coarse filesystems.
	time.Sleep(filePollInterval + 500*time.Millisecond)
	writeFile(t, dir, "tools.yaml", "tools:\n  - name: v2_tool\n")

	select {
	case metas, ok := <-ch:
		if !ok {
			t.Fatal("watch channel closed without delivering the edit")
		}
		if len(metas) != 1 || metas[0].Name != "v2_tool" {
			t.Errorf("delivered %v, want the edited entry", metas)
		}
	case <-ctx.Done():
		t.Fatal("watch did not report the edit")
	}
}

func TestWatchChannelClosesWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ch := NewFileSource(t.TempDir()).Watch(ctx)
	cancel()
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel yielded after cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("channel did not close after cancellation")
	}

	sctx, scancel := context.WithCancel(context.Background())
	sch := StaticSource{Label: "t"}.Watch(sctx)
	scancel()
	select {
	case _, ok := <-sch:
		if ok {
			t.Error("static channel yielded a value")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("static channel did not close")
	}
}

// Order is significance: built-in metadata goes first, overlays follow, and a
// later entry that clashes loses — visibly, via Build's conflicts.
func TestMergeConcatenatesInOrderAndSurfacesLaterClashes(t *testing.T) {
	builtin := StaticSource{Label: "builtin", Metas: []ops.ToolMetadata{
		{Name: "web_search"},
		{Name: "fetch_url"},
	}}
	overlay := StaticSource{Label: "overlay", Metas: []ops.ToolMetadata{
		{Name: "k8s_get_pods", Backend: "kubernetes"},
		{Name: "web_search", Server: "some-mcp", RemoteName: "search"}, // clashes
	}}

	metas, err := Merge(context.Background(), builtin, overlay)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 4 || metas[0].Name != "web_search" {
		t.Fatalf("merged %v", metas)
	}

	r, conflicts := Build(metas)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want the overlay's clash reported", conflicts)
	}
	if conflicts[0].Key != "some-mcp/search" {
		t.Errorf("blamed %q, want the later entry", conflicts[0].Key)
	}
	// The built-in kept the name.
	m, _ := r.Lookup("web_search")
	if m.Server != "" {
		t.Errorf("web_search resolves to %q, want the built-in", m.Server)
	}
}

func TestMergeNamesTheFailingSource(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", "tools: [oops\n")
	_, err := Merge(context.Background(), NewFileSource(dir))
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %q does not identify the source", err)
	}
}
