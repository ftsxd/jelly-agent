package toolreg

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

func meta(server, remote, name string, aliases ...string) ops.ToolMetadata {
	return ops.ToolMetadata{Server: server, RemoteName: remote, Name: name, Aliases: aliases}
}

// The case this package exists for: two servers exposing the same tool name.
// ADK would fail while assembling a request, several turns later, without
// naming either side.
func TestTwoServersExposingTheSameNameConflictAtRegistration(t *testing.T) {
	r, conflicts := Build([]ops.ToolMetadata{
		meta("kubernetes-prod", "get_pods", "get_pods"),
		meta("kubernetes-staging", "get_pods", "get_pods"),
	})

	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly 1", conflicts)
	}
	c := conflicts[0]
	if c.Name != "get_pods" {
		t.Errorf("Name = %q, want the contested name", c.Name)
	}
	// Both sides must be named, or an operator cannot act on the message.
	if !strings.Contains(c.Error(), "kubernetes-staging") || !strings.Contains(c.Error(), "kubernetes-prod") {
		t.Errorf("error %q does not name both entries", c.Error())
	}
	if c.Kind != ConflictName {
		t.Errorf("Kind = %q, want %q", c.Kind, ConflictName)
	}
	if c.AsAlias || c.ExistingIsAlias {
		t.Error("a canonical/canonical clash was reported as involving an alias")
	}
	// The first entry survives; the loser is simply not registered.
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1", r.Len())
	}
}

// Giving each one a distinct name is the fix, and both must then be reachable
// while still routing to their own server.
func TestDistinctNamesLetBothServersCoexist(t *testing.T) {
	r, conflicts := Build([]ops.ToolMetadata{
		meta("kubernetes-prod", "get_pods", "k8s_prod_get_pods"),
		meta("kubernetes-staging", "get_pods", "k8s_stg_get_pods"),
	})
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	for name, wantServer := range map[string]string{
		"k8s_prod_get_pods": "kubernetes-prod",
		"k8s_stg_get_pods":  "kubernetes-staging",
	} {
		m, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) failed", name)
		}
		if m.Server != wantServer {
			t.Errorf("%q routes to %q, want %q", name, m.Server, wantServer)
		}
		if m.Remote() != "get_pods" {
			t.Errorf("%q calls the server with %q, want get_pods", name, m.Remote())
		}
	}
}

// Renaming must not break a prompt or a stored case that learned the old name.
func TestAliasesResolveToTheSameEntry(t *testing.T) {
	r, conflicts := Build([]ops.ToolMetadata{
		meta("kubernetes-prod", "get_pods", "k8s_get_pods", "get_pods", "list_pods"),
	})
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	for _, name := range []string{"k8s_get_pods", "get_pods", "list_pods"} {
		m, ok := r.Lookup(name)
		if !ok {
			t.Fatalf("Lookup(%q) failed", name)
		}
		if m.Name != "k8s_get_pods" {
			t.Errorf("%q resolved to %q, want the canonical name", name, m.Name)
		}
	}
	if r.Len() != 1 {
		t.Errorf("Len = %d, want 1 — aliases are not separate tools", r.Len())
	}
}

// The two kinds of clash need different fixes, so the error has to say which.
func TestConflictDistinguishesAliasFromCanonical(t *testing.T) {
	_, conflicts := Build([]ops.ToolMetadata{
		meta("prometheus", "query", "prom_query"),
		meta("victoriametrics", "query", "vm_query", "prom_query"), // alias hits a canonical
	})
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want 1", conflicts)
	}
	c := conflicts[0]
	if !c.AsAlias {
		t.Error("AsAlias = false; the offending name was an alias")
	}
	if c.ExistingIsAlias {
		t.Error("ExistingIsAlias = true; it collided with a canonical name")
	}
	if !strings.Contains(c.Error(), "别名") {
		t.Errorf("error %q does not say the offender was an alias", c.Error())
	}

	// And the mirror case: an alias colliding with another alias.
	_, conflicts = Build([]ops.ToolMetadata{
		meta("a", "x", "a_x", "shared"),
		meta("b", "y", "b_y", "shared"),
	})
	if len(conflicts) != 1 || !conflicts[0].ExistingIsAlias {
		t.Errorf("alias/alias clash misreported: %+v", conflicts)
	}
}

// A half-registered entry must not exist: if any of its names clashes, none of
// them is claimed.
func TestAClashingEntryClaimsNoNamesAtAll(t *testing.T) {
	r, conflicts := Build([]ops.ToolMetadata{
		meta("a", "x", "a_x"),
		meta("b", "y", "b_y", "fresh_alias", "a_x"), // second alias clashes
	})
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want 1", conflicts)
	}
	if _, ok := r.Lookup("fresh_alias"); ok {
		t.Error("a rejected entry left one of its earlier aliases registered")
	}
	if _, ok := r.Lookup("b_y"); ok {
		t.Error("a rejected entry left its canonical name registered")
	}
}

// This is what an API handler calls before saving: the clash is reported at
// the moment of the change, not during someone's next conversation.
func TestCheckAgainstBlamesOnlyTheIncomingBatch(t *testing.T) {
	base, _ := Build([]ops.ToolMetadata{meta("kubernetes-prod", "get_pods", "get_pods")})

	conflicts := base.CheckAgainst([]ops.ToolMetadata{
		meta("kubernetes-staging", "get_pods", "get_pods"),
	})
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want 1", conflicts)
	}
	if conflicts[0].Key != "kubernetes-staging/get_pods" {
		t.Errorf("blamed %q, want the incoming entry", conflicts[0].Key)
	}
	// Nothing was modified.
	if base.Len() != 1 {
		t.Errorf("CheckAgainst mutated the registry: Len = %d", base.Len())
	}

	if got := base.CheckAgainst([]ops.ToolMetadata{meta("mysql", "slow_log", "mysql_slow_log")}); len(got) != 0 {
		t.Errorf("a non-conflicting addition reported conflicts: %+v", got)
	}
}

// The lossless cut before any scoring: a tool whose backend has no handle in
// this incident cannot be called, so dropping it removes no option.
func TestAvailableDropsBackendsTheIncidentCannotReach(t *testing.T) {
	r, _ := Build([]ops.ToolMetadata{
		{Name: "k8s_get_pods", Backend: "kubernetes"},
		{Name: "prom_query", Backend: "prometheus"},
		{Name: "mysql_slow_log", Backend: "mysql"},
		{Name: "redis_info", Backend: "redis"},
		{Name: "web_search"}, // no backend: always available
	})

	c := &ops.IncidentContext{Targets: []ops.Target{{
		Handles: map[string]ops.Handle{
			"kubernetes": {Backend: "kubernetes"},
			"prometheus": {Backend: "prometheus"},
		},
	}}}

	got := map[string]bool{}
	for _, m := range r.Available(c) {
		got[m.Name] = true
	}
	for _, want := range []string{"k8s_get_pods", "prom_query", "web_search"} {
		if !got[want] {
			t.Errorf("%q was dropped but is reachable", want)
		}
	}
	for _, unwanted := range []string{"mysql_slow_log", "redis_info"} {
		if got[unwanted] {
			t.Errorf("%q survived but no handle exists for its backend", unwanted)
		}
	}
}

// Registry size is not what the model pays for, so a large catalogue must be
// cheap to hold and stable to list.
func TestRegistryHoldsManyToolsWithStableOrder(t *testing.T) {
	var metas []ops.ToolMetadata
	for _, srv := range []string{"a", "b", "c", "d", "e"} {
		for i := 0; i < 30; i++ {
			suffix := string(rune('a' + i))
			metas = append(metas, ops.ToolMetadata{
				Server: srv, RemoteName: "tool_" + suffix, Name: srv + "_tool_" + suffix,
			})
		}
	}
	r, conflicts := Build(metas)
	if len(conflicts) != 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if r.Len() != 150 {
		t.Fatalf("Len = %d, want 150", r.Len())
	}
	first := r.All()
	for i := 0; i < 3; i++ {
		for j, m := range r.All() {
			if m.Key() != first[j].Key() {
				t.Fatal("All() order is not stable across calls")
			}
		}
	}
	if got := len(r.ForServer("c")); got != 30 {
		t.Errorf("ForServer(c) = %d, want 30", got)
	}
}

// A diagnosis that started under one snapshot must finish under it: hot
// reloading metadata must not change a tool's definition mid-run.
func TestStoreSwapsAtomicallyAndKeepsOldSnapshotsUsable(t *testing.T) {
	s := NewStore()
	if s.Load().Len() != 0 {
		t.Fatal("a new store should hold an empty registry, not nil")
	}

	v1, _ := Build([]ops.ToolMetadata{{Name: "k8s_get_pods", Backend: "kubernetes", Timeout: 5}})
	s.Swap(v1)
	held := s.Load()

	v2, _ := Build([]ops.ToolMetadata{{Name: "k8s_get_pods", Backend: "kubernetes", Timeout: 30}})
	s.Swap(v2)

	m, _ := held.Lookup("k8s_get_pods")
	if m.Timeout != 5 {
		t.Errorf("the held snapshot changed under the caller: Timeout = %v", m.Timeout)
	}
	m, _ = s.Load().Lookup("k8s_get_pods")
	if m.Timeout != 30 {
		t.Errorf("the new snapshot was not installed: Timeout = %v", m.Timeout)
	}
}

func TestNilRegistryAccessorsAreSafe(t *testing.T) {
	var r *Registry
	if _, ok := r.Lookup("x"); ok {
		t.Error("nil registry resolved a name")
	}
	if r.Len() != 0 || r.All() != nil || r.Names() != nil || r.Available(nil) != nil {
		t.Error("nil registry returned values")
	}
}

func TestFormatConflictsJoinsMessages(t *testing.T) {
	if got := FormatConflicts(nil); got != "" {
		t.Errorf("FormatConflicts(nil) = %q, want empty", got)
	}
	msg := FormatConflicts([]Conflict{
		{Name: "get_pods", Key: "b/get_pods", ExistingKey: "a/get_pods"},
		{Name: "query", Key: "d/query", ExistingKey: "c/query", AsAlias: true},
	})
	if !strings.Contains(msg, "get_pods") || !strings.Contains(msg, "query") {
		t.Errorf("message lost a conflict: %q", msg)
	}
}

// The three conflict kinds need different fixes, so each must be reported as
// itself rather than folded into a generic "duplicate".
func TestConflictKindsAreDistinguished(t *testing.T) {
	// Same server, same remote tool, listed twice: a malformed config, not a
	// naming decision. The old message said the entry conflicted with itself,
	// which told an operator nothing.
	_, conflicts := Build([]ops.ToolMetadata{
		meta("kubernetes-prod", "get_pods", "a"),
		meta("kubernetes-prod", "get_pods", "b"),
	})
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictDuplicateKey {
		t.Fatalf("conflicts = %+v, want one %q", conflicts, ConflictDuplicateKey)
	}
	if strings.Contains(conflicts[0].Error(), "与 kubernetes-prod/get_pods 的名称冲突") {
		t.Errorf("message says the entry conflicts with itself: %q", conflicts[0].Error())
	}

	// No name at all.
	_, conflicts = Build([]ops.ToolMetadata{{Server: "broken"}})
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictNoName {
		t.Fatalf("conflicts = %+v, want one %q", conflicts, ConflictNoName)
	}
	if !strings.Contains(conflicts[0].Error(), "broken") {
		t.Errorf("message does not name the server: %q", conflicts[0].Error())
	}
}

// Regression: the pre-save check must report a nameless entry.
//
// ConflictNoName had no Key, and CheckAgainst filters conflicts by the
// incoming keys — so the one conflict a save-time check exists to catch was
// the one it dropped.
func TestCheckAgainstReportsNamelessEntries(t *testing.T) {
	base, _ := Build([]ops.ToolMetadata{meta("kubernetes-prod", "get_pods", "k8s_get_pods")})

	conflicts := base.CheckAgainst([]ops.ToolMetadata{{Server: "broken"}})
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want the nameless entry reported", conflicts)
	}
	if conflicts[0].Kind != ConflictNoName {
		t.Errorf("Kind = %q, want %q", conflicts[0].Kind, ConflictNoName)
	}
	if !strings.Contains(conflicts[0].Error(), "broken") {
		t.Errorf("error %q does not name the server", conflicts[0].Error())
	}
	// A valid addition alongside a broken one is still reported once, for the
	// broken one only.
	conflicts = base.CheckAgainst([]ops.ToolMetadata{
		meta("mysql", "slow_log", "mysql_slow_log"),
		{Server: "broken2"},
	})
	if len(conflicts) != 1 || conflicts[0].Kind != ConflictNoName {
		t.Errorf("conflicts = %+v, want only the nameless entry", conflicts)
	}
}

// The store exists so a hot metadata reload cannot change a tool's definition
// mid-run. Readers and a swapper therefore race by construction, and the claim
// is only worth as much as a -race run proving it.
func TestStoreIsSafeUnderConcurrentReadAndSwap(t *testing.T) {
	s := NewStore()
	v1, _ := Build([]ops.ToolMetadata{{Name: "k8s_get_pods", Backend: "kubernetes"}})
	s.Swap(v1)

	var wg sync.WaitGroup

	// Readers hold a snapshot and keep using it, which is what a diagnosis in
	// flight does. A fixed number of rounds rather than a stop channel, so the
	// test cannot outlive its own signalling.
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 100; round++ {
				held := s.Load()
				for j := 0; j < 20; j++ {
					if _, ok := held.Lookup("k8s_get_pods"); !ok {
						t.Error("a held snapshot lost its entry")
						return
					}
					_ = held.Len()
					_ = held.All()
				}
			}
		}()
	}

	// A swapper installs new snapshots underneath them.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			r, _ := Build([]ops.ToolMetadata{
				{Name: "k8s_get_pods", Backend: "kubernetes", Timeout: time.Duration(i)},
			})
			s.Swap(r)
		}
	}()

	wg.Wait()
}
