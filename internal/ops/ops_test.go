package ops

import (
	"testing"
	"time"
)

// Seal is the mechanism behind "validated", so its behaviour on invented
// citations is the single most important thing in this package.
func TestSealDemotesClaimsCitingNothingThatExists(t *testing.T) {
	d := &DiagnosisResult{
		Status: TerminatedConcluded,
		Evidence: []Evidence{
			{ID: "e1", Kind: KindTableRows},
			{ID: "e2", Kind: KindWorkloadStatus},
		},
		Validated: []Claim{
			{Text: "活跃连接 198/200", Evidence: []string{"e1"}},
			{Text: "批任务持有了这些连接", Evidence: []string{"e7"}}, // invented
			{Text: "完全没有引用"},
		},
	}

	rep := d.Seal()

	if len(d.Validated) != 1 || d.Validated[0].Text != "活跃连接 198/200" {
		t.Fatalf("validated = %+v, want only the cited claim", d.Validated)
	}
	if len(d.Unvalidated) != 2 {
		t.Fatalf("unvalidated = %d claims, want 2", len(d.Unvalidated))
	}
	if rep.DemotedClaims != 2 {
		t.Errorf("DemotedClaims = %d, want 2", rep.DemotedClaims)
	}
	if len(rep.DroppedRefs) != 1 || rep.DroppedRefs[0] != "e7" {
		t.Errorf("DroppedRefs = %v, want [e7] — the invented citation", rep.DroppedRefs)
	}
	if got := d.Coverage(); got != 1.0/3.0 {
		t.Errorf("Coverage = %v, want 1/3", got)
	}
}

// A conclusion resting on nothing observed must say so, whatever confidence
// the model attached to it.
func TestSealDowngradesAConclusionRestingOnNothing(t *testing.T) {
	d := &DiagnosisResult{
		Status:     TerminatedConcluded,
		Confidence: 0.94,
		Validated:  []Claim{{Text: "是网络问题", Evidence: []string{"ghost"}}},
	}
	rep := d.Seal()
	if d.Status != TerminatedInsufficient {
		t.Fatalf("Status = %q, want %q", d.Status, TerminatedInsufficient)
	}
	if !rep.Downgraded {
		t.Error("SealReport.Downgraded = false, want true")
	}
}

// Sealing an already-honest result must change nothing — the mechanism has to
// be safe to run unconditionally.
func TestSealIsANoOpOnAValidResult(t *testing.T) {
	d := &DiagnosisResult{
		Status:      TerminatedConcluded,
		Evidence:    []Evidence{{ID: "e1"}},
		Validated:   []Claim{{Text: "有据", Evidence: []string{"e1"}}},
		Unvalidated: []Claim{{Text: "推测"}},
	}
	rep := d.Seal()
	if len(rep.DroppedRefs) != 0 || rep.DemotedClaims != 0 || rep.Downgraded {
		t.Errorf("Seal altered a valid result: %+v", rep)
	}
	if len(d.Validated) != 1 || len(d.Unvalidated) != 1 {
		t.Errorf("claim counts changed: %d validated, %d unvalidated", len(d.Validated), len(d.Unvalidated))
	}
}

func TestCitedKindsReportsWhatValidatedClaimsRestOn(t *testing.T) {
	d := &DiagnosisResult{
		Evidence: []Evidence{
			{ID: "e1", Kind: KindMetricSeries},
			{ID: "e2", Kind: KindWorkloadStatus},
			{ID: "e3", Kind: KindLogExcerpt},
		},
		Validated: []Claim{
			{Text: "a", Evidence: []string{"e1", "e2"}},
			{Text: "b", Evidence: []string{"e1"}}, // duplicate kind
		},
		// An unvalidated claim's evidence must not count: the rubric asks what
		// the conclusion actually rests on.
		Unvalidated: []Claim{{Text: "c", Evidence: []string{"e3"}}},
	}
	kinds := d.CitedKinds()
	if len(kinds) != 2 {
		t.Fatalf("CitedKinds = %v, want 2 distinct kinds", kinds)
	}
	for _, k := range kinds {
		if k == KindLogExcerpt {
			t.Error("an unvalidated claim's evidence leaked into CitedKinds")
		}
	}
}

// The window must anchor to when the incident began, not when we heard about
// it — an alert that fired hours ago would otherwise be queried outside its
// own window.
func TestWindowAnchorsAtTheIncidentNotTheClock(t *testing.T) {
	fired := time.Now().Add(-3 * time.Hour)
	w := NewWindow(fired, WindowFromAlert)

	if !w.Contains(fired) {
		t.Error("the window does not contain its own anchor")
	}
	if w.Contains(time.Now()) {
		t.Error("the window reaches the wall clock; it should sit around the incident")
	}
	if w.Guessed() {
		t.Error("Guessed = true for a window anchored to a real timestamp")
	}
	if w.Since.After(fired) || !w.Until.After(fired) {
		t.Errorf("window %v..%v does not straddle the anchor %v", w.Since, w.Until, fired)
	}
}

func TestWindowFallsBackWhenNothingCanBeAnchored(t *testing.T) {
	for name, w := range map[string]TimeWindow{
		"zero anchor": NewWindow(time.Time{}, WindowFromAlert),
		"inverted":    NewWindowBetween(time.Now(), time.Now().Add(-time.Hour), WindowFromUser),
		"empty":       NewWindowBetween(time.Time{}, time.Time{}, WindowFromUser),
	} {
		if !w.Guessed() {
			t.Errorf("%s: Guessed = false, want true — a guessed window must be labelled", name)
		}
		if w.Source != WindowDefault {
			t.Errorf("%s: Source = %q, want %q", name, w.Source, WindowDefault)
		}
	}
}

func TestWindowClampsToMaxLookback(t *testing.T) {
	until := time.Now()
	w := NewWindowBetween(until.Add(-90*24*time.Hour), until, WindowFromUser)
	if w.Duration() > MaxLookback {
		t.Errorf("Duration = %v, want at most %v — one query must not sweep a quarter of an index", w.Duration(), MaxLookback)
	}
}

// Half-open, so two abutting windows do not both claim the boundary sample.
func TestWindowIsHalfOpen(t *testing.T) {
	since := time.Now().Add(-time.Hour).UTC()
	until := time.Now().UTC()
	w := NewWindowBetween(since, until, WindowFromUser)
	if !w.Contains(since) {
		t.Error("Since must be inside the window")
	}
	if w.Contains(until) {
		t.Error("Until must be outside the window")
	}
}

// Two servers exposing get_pods must be addressable, which only works if the
// name the model sees is separable from the name the server is called with.
func TestMetadataSeparatesModelNameFromRemoteName(t *testing.T) {
	prod := ToolMetadata{Name: "k8s_get_pods", RemoteName: "get_pods", Server: "kubernetes-prod"}
	stg := ToolMetadata{Name: "k8s_get_pods", RemoteName: "get_pods", Server: "kubernetes-staging"}

	if prod.Key() == stg.Key() {
		t.Fatal("two servers' entries share a key; the registry could not hold both")
	}
	if prod.Remote() != "get_pods" || stg.Remote() != "get_pods" {
		t.Error("Remote() must return the server's own name")
	}

	// Name defaults to RemoteName so nothing is renamed without a reason.
	plain := ToolMetadata{Name: "web_search"}
	if plain.Remote() != "web_search" || plain.Key() != "/web_search" {
		t.Errorf("built-in tool: Remote = %q, Key = %q", plain.Remote(), plain.Key())
	}
}

// Renaming must not break a stored case or a prompt that learned the old name.
func TestMetadataNamesIncludeAliasesWithoutDuplicates(t *testing.T) {
	m := ToolMetadata{Name: "k8s_get_pods", Aliases: []string{"get_pods", "", "k8s_get_pods", "list_pods"}}
	names := m.Names()
	want := map[string]bool{"k8s_get_pods": true, "get_pods": true, "list_pods": true}
	if len(names) != len(want) {
		t.Fatalf("Names = %v, want %d entries (no blanks, no self-duplicate)", names, len(want))
	}
	for _, n := range names {
		if !want[n] {
			t.Errorf("unexpected name %q", n)
		}
	}
}

// Different servers spell the same concept differently; that difference has to
// stop at the registry rather than reach the model.
func TestCanonicalArgResolvesAliases(t *testing.T) {
	m := ToolMetadata{
		ArgAliases: map[string][]string{
			"namespace": {"ns", "kubernetes_namespace"},
			"selector":  {"label_selector"},
		},
	}
	for in, want := range map[string]string{
		"ns":                   "namespace",
		"kubernetes_namespace": "namespace",
		"namespace":            "namespace",
		"label_selector":       "selector",
		"unknown":              "unknown", // passes through untouched
	} {
		if got := m.CanonicalArg(in); got != want {
			t.Errorf("CanonicalArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestInjectsCoversWindowParams(t *testing.T) {
	m := ToolMetadata{
		InjectedParams: []string{"cluster", "namespace"},
		WindowParams:   [2]string{"start", "end"},
	}
	for _, arg := range []string{"cluster", "namespace", "start", "end"} {
		if !m.Injects(arg) {
			t.Errorf("Injects(%q) = false; a host-supplied arg must not reach the model's schema", arg)
		}
	}
	if m.Injects("selector") {
		t.Error("Injects(selector) = true; that one is the model's to fill")
	}
}

// An MCP tool with no declared side effect is not assumed safe.
func TestReadOnlyDoesNotAssumeSafetyForRemoteTools(t *testing.T) {
	if (ToolMetadata{Server: "kubernetes-prod"}).ReadOnly() {
		t.Error("an MCP tool with no declared level was treated as read-only")
	}
	if !(ToolMetadata{}).ReadOnly() {
		t.Error("a built-in with no declared level should be read-only")
	}
	if (ToolMetadata{Server: "s", SideEffect: SideEffectRisky}).ReadOnly() {
		t.Error("a risky tool reported itself read-only")
	}
}

// One function serves the dedup cache and the evaluation fixture key, so
// record and replay need no adapter between them.
func TestFingerprintIsStableAcrossArgumentOrder(t *testing.T) {
	a := Fingerprint("k8s_get_pods", map[string]any{"ns": "payment", "cluster": "prod-a"})
	b := Fingerprint("k8s_get_pods", map[string]any{"cluster": "prod-a", "ns": "payment"})
	if a != b {
		t.Fatal("fingerprint depends on map literal order; replay would miss its own recording")
	}
	if a == Fingerprint("k8s_get_pods", map[string]any{"ns": "payment"}) {
		t.Error("different arguments produced the same fingerprint")
	}
	if a == Fingerprint("k8s_logs", map[string]any{"ns": "payment", "cluster": "prod-a"}) {
		t.Error("different tools produced the same fingerprint")
	}
}

// Seeded and preloaded evidence is the premise the run started from; evicting
// it would leave the model reasoning about something it can no longer see.
func TestPinnedEvidenceSurvivesTheContextBudget(t *testing.T) {
	for _, o := range []Origin{OriginWorkflow, OriginPreload} {
		if !(Evidence{Origin: o}).Pinned() {
			t.Errorf("Origin %q should be pinned", o)
		}
	}
	for _, o := range []Origin{OriginModel, OriginReplay} {
		if (Evidence{Origin: o}).Pinned() {
			t.Errorf("Origin %q should be evictable", o)
		}
	}
}

func TestIncidentContextBackendsAndHandles(t *testing.T) {
	c := &IncidentContext{
		Primary: &Target{
			Canonical: "payment-gateway", Env: "prod",
			Handles: map[string]Handle{
				"kubernetes": {Backend: "kubernetes", Ref: map[string]string{"namespace": "payment"}},
			},
		},
		Targets: []Target{
			{Canonical: "payment-gateway", Env: "prod", Handles: map[string]Handle{
				"kubernetes": {Backend: "kubernetes"},
			}},
			{Canonical: "rds-pay-01", Env: "prod", Handles: map[string]Handle{
				"mysql": {Backend: "mysql", Ref: map[string]string{"instance": "rds-pay-01"}},
			}},
		},
	}

	if got := c.Env(); got != "prod" {
		t.Errorf("Env = %q, want prod", got)
	}
	if bs := c.Backends(); len(bs) != 2 {
		t.Errorf("Backends = %v, want kubernetes and mysql", bs)
	}
	// The primary's handle wins; a backend only the secondary has still resolves.
	if h, ok := c.HandleFor("kubernetes"); !ok || h.Ref["namespace"] != "payment" {
		t.Errorf("HandleFor(kubernetes) = %+v, %v — want the primary's", h, ok)
	}
	if h, ok := c.HandleFor("mysql"); !ok || h.Ref["instance"] != "rds-pay-01" {
		t.Errorf("HandleFor(mysql) = %+v, %v", h, ok)
	}
	if _, ok := c.HandleFor("redis"); ok {
		t.Error("HandleFor(redis) resolved a backend nothing has a handle for")
	}
}

// Selection drops candidates whose backend has no handle — a deterministic cut
// that costs nothing, unlike relevance scoring.
func TestNilContextAccessorsAreSafe(t *testing.T) {
	var c *IncidentContext
	if c.Env() != "" || c.Backends() != nil {
		t.Error("nil context accessors returned values")
	}
	if _, ok := c.HandleFor("kubernetes"); ok {
		t.Error("nil context resolved a handle")
	}
}

// Regression: a handle present only on Primary must still be reachable.
//
// A normalizer that resolved one target and set it as primary need not also
// copy it into Targets. When Backends looked only at the slice, HandleFor
// resolved a backend that Backends denied — and selection silently hid tools
// the incident could actually reach. Nothing is logged when that happens; the
// model simply never sees the tool.
func TestBackendsIncludesThePrimaryTarget(t *testing.T) {
	c := &IncidentContext{
		Primary: &Target{
			Canonical: "payment-gateway", Env: "prod",
			Handles: map[string]Handle{"kubernetes": {Backend: "kubernetes"}},
		},
	}
	bs := c.Backends()
	if len(bs) != 1 || bs[0] != "kubernetes" {
		t.Fatalf("Backends = %v, want [kubernetes]", bs)
	}
	// The two must agree: anything HandleFor can resolve, Backends must list.
	if _, ok := c.HandleFor("kubernetes"); !ok {
		t.Fatal("HandleFor cannot resolve what Backends lists")
	}

	// And no duplicates when the primary also appears in the slice, which is
	// the other way a normalizer may fill this in.
	c.Targets = []Target{*c.Primary, {Handles: map[string]Handle{"mysql": {Backend: "mysql"}}}}
	bs = c.Backends()
	if len(bs) != 2 {
		t.Errorf("Backends = %v, want kubernetes and mysql exactly once each", bs)
	}
}
