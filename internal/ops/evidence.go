package ops

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// EvidenceKind is what an observation *is*, independent of which vendor
// produced it. Nightingale and Prometheus both yield KindMetricSeries; making
// that true is the semantic layer's job.
//
// The kind drives three things: how the observation renders into a prompt, how
// the context budget ranks it for eviction, and what an evaluation rubric can
// require a conclusion to have cited.
type EvidenceKind string

const (
	KindWorkloadStatus EvidenceKind = "workload_status" // pods, replicas, restarts
	KindEvents         EvidenceKind = "events"          // k8s events, deploy records
	KindLogExcerpt     EvidenceKind = "log_excerpt"
	KindMetricSeries   EvidenceKind = "metric_series"
	KindTableRows      EvidenceKind = "table_rows" // SQL / INFO output
	KindConfig         EvidenceKind = "config"     // manifests, variables
	KindTopology       EvidenceKind = "topology"   // dependencies, routing
	KindKnowledge      EvidenceKind = "knowledge"  // a retrieved past case
	KindText           EvidenceKind = "text"       // anything not yet classified
)

// Origin says which execution path produced an observation.
//
// Baseline workflow results and model-chosen results are both evidence, but
// only the second says anything about how well the model is reasoning — so
// tool-selection quality is measured over OriginModel alone. Counting the
// deterministic baseline would flatter the number for free.
type Origin string

const (
	OriginWorkflow Origin = "workflow" // deterministic baseline check
	OriginModel    Origin = "model"    // the model chose this call
	OriginPreload  Origin = "preload"  // knowledge retrieved before turn zero
	OriginReplay   Origin = "replay"   // served from the gateway's dedup cache
)

// Source is the provenance triple: which backend, through which tool, on which
// registered server. An observation without a Source is not evidence, it is a
// sentence.
type Source struct {
	Backend string `json:"backend"`
	Tool    string `json:"tool"`             // our canonical name
	Server  string `json:"server,omitempty"` // MCP server id; empty when built in
}

// Evidence is one observation the gateway has stamped, bounded and cleaned.
//
// The agent never constructs one. It receives them, reasons over them, and
// cites them by ID. That indirection is what makes a validated claim
// mechanically checkable instead of self-reported.
type Evidence struct {
	ID   string       `json:"id"` // short and stable within a run: e1, e2, ...
	Kind EvidenceKind `json:"kind"`

	Source     Source         `json:"source"`
	Args       map[string]any `json:"args,omitempty"`
	Window     TimeWindow     `json:"window"` // the span this observation covers
	ObservedAt time.Time      `json:"observed_at"`
	Origin     Origin         `json:"origin"`

	// Summary is the one line that goes into the prompt and the report. Data is
	// the structured payload, already shaped to the tool's ceiling.
	Summary   string          `json:"summary"`
	Data      json.RawMessage `json:"data,omitempty"`
	Truncated bool            `json:"truncated,omitempty"`
	Redacted  bool            `json:"redacted,omitempty"`
}

// Pinned reports whether the context budget must keep this observation even
// under pressure. Seeded and preloaded evidence is what the run started from;
// evicting it would leave the model reasoning about a premise it can no longer
// see.
func (e Evidence) Pinned() bool {
	return e.Origin == OriginWorkflow || e.Origin == OriginPreload
}

// ToolCall is the audit record of one attempt to reach the outside world. It
// is written whether the call succeeded, failed, timed out or was served from
// cache — a success rate computed over anything less is not a success rate.
type ToolCall struct {
	ID        string         `json:"id"`
	Tool      string         `json:"tool"` // our canonical name
	Args      map[string]any `json:"args,omitempty"`
	Origin    Origin         `json:"origin"`
	StartedAt time.Time      `json:"started_at"`
	Duration  time.Duration  `json:"duration"`

	OK      bool   `json:"ok"`
	ErrKind string `json:"err_kind,omitempty"`
	Err     string `json:"err,omitempty"`

	// Replayed marks a call the gateway answered from its dedup cache. These
	// are excluded from the success-rate denominator: nothing was attempted,
	// so counting them would move the number for no reason. Their count is a
	// stagnation signal instead.
	Replayed bool `json:"replayed,omitempty"`

	ResultBytes int      `json:"result_bytes"`
	Truncated   bool     `json:"truncated,omitempty"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

// Fingerprint identifies a call by tool name and arguments.
//
// One function serves two callers, which is the whole trick behind offline
// evaluation: the gateway uses it to de-duplicate within a run, and the
// evaluation harness uses the same value as the key for a frozen fixture. So
// record and replay need no adapter between them.
//
// encoding/json marshals map keys in sorted order, so the digest does not
// depend on argument insertion order.
func Fingerprint(tool string, args map[string]any) string {
	payload, err := json.Marshal(struct {
		Tool string         `json:"tool"`
		Args map[string]any `json:"args"`
	}{tool, args})
	if err != nil {
		payload = []byte(tool)
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:12])
}

// Fingerprint is the call's own digest.
func (c ToolCall) Fingerprint() string { return Fingerprint(c.Tool, c.Args) }
