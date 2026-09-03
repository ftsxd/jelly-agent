package ops

import "time"

// TerminationReason says why a run stopped.
//
// Evaluation needs this to tell a wrong answer apart from a run that never got
// to answer — the two deserve different fixes, and a single accuracy number
// conflates them. TerminatedInsufficient is a respectable outcome, not a
// failure: without that exit the model invents a root cause when evidence runs
// short, which is the main way accuracy dies.
type TerminationReason string

const (
	TerminatedConcluded    TerminationReason = "concluded"
	TerminatedInsufficient TerminationReason = "insufficient_evidence"
	TerminatedLoopCap      TerminationReason = "loop_cap"
	TerminatedStagnation   TerminationReason = "stagnation"
	TerminatedToolFailure  TerminationReason = "tool_failure"
	TerminatedContextFull  TerminationReason = "context_exhausted"
	TerminatedCancelled    TerminationReason = "cancelled"
)

// Claim is one statement with the observations it rests on.
//
// A claim citing nothing is not forbidden — much of a useful diagnosis is
// inference — it is merely reported as unvalidated, which is a different thing
// from wrong.
type Claim struct {
	Text     string   `json:"text"`
	Evidence []string `json:"evidence,omitempty"` // evidence IDs
}

// Validated reports whether this claim cites anything.
func (c Claim) Validated() bool { return len(c.Evidence) > 0 }

// RuledOut records an explanation that was tested and rejected.
//
// Publishing these is most of what makes a diagnosis trustworthy to the person
// on call: "it is not the connection pool, here is the check" saves them from
// redoing that check.
type RuledOut struct {
	Statement string   `json:"statement"`
	Why       string   `json:"why"`
	Evidence  []string `json:"evidence,omitempty"`
}

// Action is a proposed remediation, carrying a side-effect level so delivery
// can render "run this" and "get approval for this" differently without
// re-parsing prose.
type Action struct {
	Text        string          `json:"text"`
	SideEffect  SideEffectLevel `json:"side_effect,omitempty"`
	Automatable bool            `json:"automatable,omitempty"` // a registered tool could do it
	Tool        string          `json:"tool,omitempty"`
}

// RunMeta is the execution record behind a conclusion: what it cost, what the
// guardrails did, and why it stopped.
type RunMeta struct {
	Model             string        `json:"model,omitempty"`
	StartedAt         time.Time     `json:"started_at"`
	FirstConclusionAt time.Time     `json:"first_conclusion_at,omitempty"`
	EndedAt           time.Time     `json:"ended_at,omitempty"`
	Duration          time.Duration `json:"duration,omitempty"`

	Iterations   int `json:"iterations,omitempty"`
	PromptTokens int `json:"prompt_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`

	// Guardrail counters. A run that concluded correctly after hitting
	// stagnation twice is still correct, but these are the earliest signal
	// that tool descriptions or selection are drifting.
	DuplicateCalls  int `json:"duplicate_calls,omitempty"`
	StagnantRounds  int `json:"stagnant_rounds,omitempty"`
	EvictedEvidence int `json:"evicted_evidence,omitempty"`
}

// DiagnosisResult is what leaves the pipeline: the conclusion plus everything
// needed to audit it, grade it, and freeze it into the case library.
//
// One shape serves three readers, so none of them drifts from the others. The
// person on call reads RootCause, Validated and Remediation. The reviewer
// reads Validated against Evidence. The evaluation harness reads CauseClass,
// Status and Run.
type DiagnosisResult struct {
	IncidentID string     `json:"incident_id"`
	Suite      string     `json:"suite,omitempty"`
	Window     TimeWindow `json:"window"`

	CauseClass string  `json:"cause_class,omitempty"`
	RootCause  string  `json:"root_cause,omitempty"`
	Confidence float64 `json:"confidence,omitempty"`

	CausalChain []Claim    `json:"causal_chain,omitempty"`
	Validated   []Claim    `json:"validated"`
	Unvalidated []Claim    `json:"unvalidated"`
	RuledOut    []RuledOut `json:"ruled_out,omitempty"`
	Remediation []Action   `json:"remediation,omitempty"`

	// The audit trail. Evidence travels with the conclusion rather than being
	// looked up later from a log that may have rotated — it is the authority
	// every claim is checked against.
	Evidence      []Evidence `json:"evidence"`
	ToolCalls     []ToolCall `json:"tool_calls,omitempty"`
	KnowledgeHits []string   `json:"knowledge_hits,omitempty"` // case IDs

	Status TerminationReason `json:"status"`
	Run    RunMeta           `json:"run"`
}

// EvidenceByID looks up one observation.
func (d *DiagnosisResult) EvidenceByID(id string) (*Evidence, bool) {
	for i := range d.Evidence {
		if d.Evidence[i].ID == id {
			return &d.Evidence[i], true
		}
	}
	return nil, false
}

// SealReport says what sealing had to repair.
//
// DroppedRefs is a high-value signal in its own right: a steady stream of them
// means the model is inventing citations, and that shows up here long before
// it shows up as a drop in reviewed accuracy.
type SealReport struct {
	DroppedRefs   []string `json:"dropped_refs,omitempty"`
	DemotedClaims int      `json:"demoted_claims,omitempty"`
	Downgraded    bool     `json:"downgraded,omitempty"` // status fell to insufficient
}

// Seal enforces the one invariant that makes "validated" mean anything: a
// claim is validated if and only if it cites evidence that exists in this
// result.
//
// Every citation is checked against the evidence set; unknown IDs are dropped;
// any claim left in Validated citing nothing moves to Unvalidated. Call it
// once at the end of the diagnosis builder, before delivery and before the
// result is written to the case library.
//
// This is not defensive coding against a buggy caller — it is the mechanism.
// The model can and does write evidence IDs that do not exist, silently and
// with no error anywhere. Asking it to self-report which of its statements
// were verified produces a plausible list, not a checked one.
func (d *DiagnosisResult) Seal() SealReport {
	known := make(map[string]bool, len(d.Evidence))
	for _, e := range d.Evidence {
		known[e.ID] = true
	}

	var rep SealReport
	seen := map[string]bool{}
	prune := func(refs []string) []string {
		out := refs[:0:0]
		for _, id := range refs {
			if known[id] {
				out = append(out, id)
				continue
			}
			if !seen[id] {
				seen[id] = true
				rep.DroppedRefs = append(rep.DroppedRefs, id)
			}
		}
		return out
	}

	for i := range d.CausalChain {
		d.CausalChain[i].Evidence = prune(d.CausalChain[i].Evidence)
	}
	for i := range d.RuledOut {
		d.RuledOut[i].Evidence = prune(d.RuledOut[i].Evidence)
	}
	for i := range d.Unvalidated {
		d.Unvalidated[i].Evidence = prune(d.Unvalidated[i].Evidence)
	}

	kept := make([]Claim, 0, len(d.Validated))
	for _, c := range d.Validated {
		c.Evidence = prune(c.Evidence)
		if c.Validated() {
			kept = append(kept, c)
			continue
		}
		rep.DemotedClaims++
		d.Unvalidated = append(d.Unvalidated, c)
	}
	d.Validated = kept

	// A conclusion resting on nothing observed is reported as such, whatever
	// confidence the model attached to it.
	if len(d.Validated) == 0 && d.Status == TerminatedConcluded {
		d.Status = TerminatedInsufficient
		rep.Downgraded = true
	}
	return rep
}

// Coverage is the share of claims backed by evidence — a per-run quality
// signal that needs no human reviewer.
func (d *DiagnosisResult) Coverage() float64 {
	total := len(d.Validated) + len(d.Unvalidated)
	if total == 0 {
		return 0
	}
	return float64(len(d.Validated)) / float64(total)
}

// CitedKinds lists the evidence kinds the validated claims rest on. An
// evaluation rubric uses it to require that a conclusion actually looked at,
// say, a metric series — which separates reasoning from recall.
func (d *DiagnosisResult) CitedKinds() []EvidenceKind {
	seen := map[EvidenceKind]bool{}
	var out []EvidenceKind
	for _, c := range d.Validated {
		for _, id := range c.Evidence {
			if e, ok := d.EvidenceByID(id); ok && !seen[e.Kind] {
				seen[e.Kind] = true
				out = append(out, e.Kind)
			}
		}
	}
	return out
}
