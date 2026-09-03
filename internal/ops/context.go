package ops

import (
	"encoding/json"
	"time"
)

// Trigger records how a diagnosis was started. It is the routing key for
// intake policy and the grouping key for every downstream metric, so a task
// delegated by another agent can never be silently counted as an operator's.
type Trigger string

const (
	TriggerAlert    Trigger = "alert"
	TriggerUser     Trigger = "user"
	TriggerSchedule Trigger = "schedule"
	TriggerA2A      Trigger = "a2a"
	TriggerReplay   Trigger = "replay" // evaluation run against frozen fixtures
)

// Severity is the normalized urgency, independent of each alert source's own
// scale (Nightingale's P0..P3, Prometheus' critical/warning, and so on).
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityMajor    Severity = "major"
	SeverityMinor    Severity = "minor"
	SeverityInfo     Severity = "info"
)

// WindowSource says where an incident window's anchor came from. It travels
// with the window because a conclusion drawn inside a guessed window deserves
// less trust than one drawn inside a window anchored to the alert itself.
type WindowSource string

const (
	WindowFromAlert    WindowSource = "alert"    // a timestamp in the payload
	WindowFromUser     WindowSource = "user"     // explicitly stated
	WindowFromEvidence WindowSource = "evidence" // narrowed after observations
	WindowDefault      WindowSource = "default"  // nothing to anchor to
)

// TimeWindow is the half-open interval [Since, Until) that every time-aware
// tool must query.
//
// Tools must not compute their own "last 15 minutes". That clock is the
// agent's, not the incident's: an alert that fired three hours ago would put
// every such query outside the window, and the agent would confidently report
// that nothing looks wrong — using evidence from a period when nothing was.
//
// Half-open matches Prometheus and Elasticsearch range semantics, so boundary
// samples are not counted twice when two windows abut.
type TimeWindow struct {
	Since  time.Time    `json:"since"`
	Until  time.Time    `json:"until"`
	Source WindowSource `json:"source"`
	// Confidence is 1.0 when Source anchored to a real timestamp and 0.0 when
	// it fell back to a default lookback. It belongs in the report: a
	// conclusion drawn inside a guessed window is worth knowing about.
	Confidence float64 `json:"confidence"`
}

// Window budget. MaxLookback bounds what a caller may widen to, so one query
// cannot sweep a quarter of a log index.
const (
	DefaultLookback     = 2 * time.Hour
	MaxLookback         = 7 * 24 * time.Hour
	DefaultForwardSpill = 10 * time.Minute // covers alert delivery lag
)

// NewWindow builds a window anchored at an incident time.
//
// The anchor is the moment the incident began, not the moment we heard about
// it. The window opens DefaultLookback before it and closes DefaultForwardSpill
// after, so the run-up is visible and delivery lag does not truncate the tail.
func NewWindow(anchor time.Time, src WindowSource) TimeWindow {
	if anchor.IsZero() {
		return DefaultWindow()
	}
	anchor = anchor.UTC()
	return TimeWindow{
		Since:      anchor.Add(-DefaultLookback),
		Until:      anchor.Add(DefaultForwardSpill),
		Source:     src,
		Confidence: 1,
	}
}

// DefaultWindow is the fallback when nothing could be anchored: the last
// DefaultLookback, explicitly marked as a guess.
func DefaultWindow() TimeWindow {
	now := time.Now().UTC()
	return TimeWindow{
		Since:      now.Add(-DefaultLookback),
		Until:      now,
		Source:     WindowDefault,
		Confidence: 0,
	}
}

// NewWindowBetween builds an explicit window, clamping it to MaxLookback and
// falling back to the default when the interval is empty or inverted.
func NewWindowBetween(since, until time.Time, src WindowSource) TimeWindow {
	if since.IsZero() || !until.After(since) {
		return DefaultWindow()
	}
	since, until = since.UTC(), until.UTC()
	if until.Sub(since) > MaxLookback {
		since = until.Add(-MaxLookback)
	}
	conf := 1.0
	if src == WindowDefault {
		conf = 0
	}
	return TimeWindow{Since: since, Until: until, Source: src, Confidence: conf}
}

// Contains reports whether t falls inside the half-open window.
func (w TimeWindow) Contains(t time.Time) bool {
	u := t.UTC()
	return !u.Before(w.Since) && u.Before(w.Until)
}

// Duration is the window's span.
func (w TimeWindow) Duration() time.Duration { return w.Until.Sub(w.Since) }

// Guessed reports whether the window was never anchored to a real timestamp.
func (w TimeWindow) Guessed() bool { return w.Source == WindowDefault || w.Confidence == 0 }

// TargetKind is the sort of thing being diagnosed. It selects which handles a
// target is expected to carry and weights tool selection.
type TargetKind string

const (
	TargetService  TargetKind = "service"
	TargetWorkload TargetKind = "workload" // Deployment / StatefulSet / DaemonSet
	TargetDatabase TargetKind = "database"
	TargetCache    TargetKind = "cache"
	TargetHost     TargetKind = "host"
	TargetCluster  TargetKind = "cluster"
)

// Handle is one backend's exact address for a target: the arguments a tool
// needs, already resolved. Keys are backend-specific — "cluster"/"namespace"/
// "name" for kubernetes, "instance"/"schema" for mysql.
//
// Handles exist so tool arguments come from a lookup rather than from the
// model. Guessing that pay-gateway is the Deployment payment-gw-prod is exactly
// the kind of confident mistake that yields a well-argued wrong answer.
type Handle struct {
	Backend string            `json:"backend"`
	Ref     map[string]string `json:"ref"`
}

// Target is one normalized entity under diagnosis. Canonical is the identity
// used everywhere else — evidence, metrics, the case library — regardless of
// what each system calls it.
type Target struct {
	Canonical string            `json:"canonical"`
	Kind      TargetKind        `json:"kind"`
	Env       string            `json:"env"`
	Owner     string            `json:"owner,omitempty"`
	Handles   map[string]Handle `json:"handles"` // keyed by backend
	Aliases   []string          `json:"aliases,omitempty"`
}

// Handle returns the target's address for one backend.
func (t Target) Handle(backend string) (Handle, bool) {
	h, ok := t.Handles[backend]
	return h, ok
}

// SignalKind classifies a fact extracted from the raw payload.
type SignalKind string

const (
	SignalMetricThreshold SignalKind = "metric_threshold"
	SignalErrorMessage    SignalKind = "error_message"
	SignalStateChange     SignalKind = "state_change"
	SignalLogPattern      SignalKind = "log_pattern"
)

// Signal is one structured fact pulled out of the request before any model
// sees it. Extracting "container_memory_usage_bytes crossed 90%" once beats
// asking the model to re-read the payload on every turn.
type Signal struct {
	Kind      SignalKind `json:"kind"`
	Name      string     `json:"name,omitempty"`
	Value     string     `json:"value,omitempty"`
	Threshold string     `json:"threshold,omitempty"`
	Unit      string     `json:"unit,omitempty"`
	Text      string     `json:"text,omitempty"`
}

// IncidentContext is the input to everything downstream: tool selection, tool
// arguments, prompt grounding, and evaluation stratification.
//
// It is produced by deterministic code plus at most one classification call.
// Nothing downstream may re-derive a field that lives here — that is the whole
// point of resolving it once.
type IncidentContext struct {
	ID      string          `json:"id"`
	Trigger Trigger         `json:"trigger"`
	Raw     json.RawMessage `json:"raw,omitempty"` // original payload, verbatim

	Source   string   `json:"source,omitempty"` // nightingale | prometheus | user | ...
	Title    string   `json:"title,omitempty"`
	Severity Severity `json:"severity,omitempty"`

	Window  TimeWindow `json:"window"`
	Targets []Target   `json:"targets,omitempty"`
	Primary *Target    `json:"primary,omitempty"`

	Signals []Signal          `json:"signals,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`

	// Suite names the diagnosis family (k8s, mysql, redis, alert...). It
	// weights tool selection and is the stratum an evaluation suite reports
	// per-scenario accuracy against.
	Suite string `json:"suite,omitempty"`

	// Unresolved holds identifiers no mapping could resolve. Deliberately not
	// an error: the run continues degraded, and each entry is a candidate row
	// for whatever table should have known.
	Unresolved []string `json:"unresolved,omitempty"`

	CreatedAt time.Time `json:"created_at"`
}

// Env returns the primary target's environment, which is what routes a call to
// one of several servers for the same backend.
//
// Primary first, then any target that declares one: the primary is the thing
// under diagnosis, and a secondary target in another environment must not
// redirect the call.
func (c *IncidentContext) Env() string {
	if c == nil {
		return ""
	}
	if c.Primary != nil && c.Primary.Env != "" {
		return c.Primary.Env
	}
	for _, t := range c.Targets {
		if t.Env != "" {
			return t.Env
		}
	}
	return ""
}

// Backends lists the distinct backends reachable for this incident. Tool
// selection uses it to drop candidates whose backend has no handle — a
// deterministic cut that costs nothing, unlike relevance scoring.
//
// Primary is walked alongside Targets and must be: a normalizer that resolved
// one target and set it as primary need not also copy it into the slice. If
// this looked only at Targets, HandleFor would resolve a backend that Backends
// denied, and selection would silently hide tools the incident can actually
// reach — the worst kind of filtering bug, because the model simply never sees
// the tool and nothing is logged.
func (c *IncidentContext) Backends() []string {
	if c == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(t Target) {
		for b := range t.Handles {
			if !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	if c.Primary != nil {
		add(*c.Primary)
	}
	for _, t := range c.Targets {
		add(t)
	}
	return out
}

// HandleFor returns the primary target's handle for a backend, falling back to
// the first target that has one.
func (c *IncidentContext) HandleFor(backend string) (Handle, bool) {
	if c == nil {
		return Handle{}, false
	}
	if c.Primary != nil {
		if h, ok := c.Primary.Handle(backend); ok {
			return h, true
		}
	}
	for _, t := range c.Targets {
		if h, ok := t.Handle(backend); ok {
			return h, true
		}
	}
	return Handle{}, false
}
