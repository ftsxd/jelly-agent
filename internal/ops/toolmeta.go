package ops

import (
	"encoding/json"
	"strings"
	"time"
)

// SideEffectLevel classifies what a tool does to the world. Approval, dry-run
// and audit policy are all derived from this one field, so the permission
// model is a property of the tool rather than a subsystem bolted on beside it.
type SideEffectLevel string

const (
	SideEffectReadOnly SideEffectLevel = "read_only"
	SideEffectMutating SideEffectLevel = "mutating"       // reversible: scale, restart
	SideEffectRisky    SideEffectLevel = "mutating_risky" // destructive or hard to undo
)

// Valid reports whether the level is one this code knows.
//
// It exists because this value arrives from a YAML file, and a permission
// check that silently accepts an unrecognized level fails open: "mutatting"
// would compare as weaker than read_only and let a mutating tool through. A
// misspelt level must be treated as the most dangerous thing it could be, not
// the safest.
func (l SideEffectLevel) Valid() bool {
	switch l {
	case SideEffectReadOnly, SideEffectMutating, SideEffectRisky:
		return true
	default:
		return false
	}
}

// LatencyClass is the expected cost of a call, used to order equally relevant
// candidates so a cheap check runs before an expensive scan.
type LatencyClass string

const (
	LatencyFast   LatencyClass = "fast"   // sub-second, local or cached
	LatencyMedium LatencyClass = "medium" // a normal API round trip
	LatencySlow   LatencyClass = "slow"   // log scans, large aggregations
)

// ToolMetadata is everything about a tool that is not its JSON schema.
//
// It exists because an MCP server's own advertisement is not enough to select
// with: it gives a name, a description and an input schema, but not what the
// tool is for, when it is the wrong choice, what it costs, whether it may run
// concurrently, or what kind of evidence it yields. The registry therefore
// keeps our own metadata keyed by (Server, RemoteName) and overlays it on
// whatever the server advertises. Third-party servers reword their
// descriptions without telling us; selection must not be at their mercy.
//
// Every field is plain data with a yaml tag: this can be loaded from a file,
// diffed in review, pushed from a config service, and frozen into an
// evaluation fixture. Behaviour lives in the gateway.
type ToolMetadata struct {
	// ── Identity ────────────────────────────────────────────────────────
	//
	// Name is ours and is what the model sees. RemoteName is what the server
	// calls it and is the routing key back to that server. Keeping them apart
	// is what makes two servers exposing get_pods addressable at all: ADK's
	// mcpTool uses one field for both (tool/mcptoolset/tool.go), so without
	// this split there is nothing to tell the two apart.
	//
	// Name defaults to RemoteName. Do not rename to avoid a hypothetical
	// clash: the name is a hint the model reads, and a mangled one costs
	// selection accuracy and tokens for nothing.
	Name       string `json:"name" yaml:"name"`
	RemoteName string `json:"remote_name,omitempty" yaml:"remote_name,omitempty"`

	// Server is the MCP server id, or empty for a built-in tool.
	//
	// It is deliberately not part of Name. Environment belongs in the incident
	// context, not in the tool's identity: one tool routed to N servers keeps
	// the schema cost flat and — more importantly — removes the model's
	// opportunity to pick the wrong environment, which is a far worse error
	// than picking the wrong tool.
	Server string `json:"server,omitempty" yaml:"server,omitempty"`

	// Aliases are additional names that resolve to this tool. Many-to-one, so
	// renaming never breaks a stored case or a prompt that learned the old
	// name — both keep working.
	Aliases []string `json:"aliases,omitempty" yaml:"aliases,omitempty"`

	// ── What the model reads ────────────────────────────────────────────
	Description string   `json:"description" yaml:"description"`
	UseCases    []string `json:"use_cases,omitempty" yaml:"use_cases,omitempty"`
	Examples    []string `json:"examples,omitempty" yaml:"examples,omitempty"`
	// AntiExamples say when *not* to reach for this tool. A negative example
	// removes more wrong calls than three more positive ones add right ones,
	// because the hard cases are the tools that merely look relevant.
	AntiExamples []string `json:"anti_examples,omitempty" yaml:"anti_examples,omitempty"`

	// ── What selection scores against ───────────────────────────────────
	Backend  string       `json:"backend" yaml:"backend"`
	Suites   []string     `json:"suites,omitempty" yaml:"suites,omitempty"`
	Produces EvidenceKind `json:"produces,omitempty" yaml:"produces,omitempty"`
	Latency  LatencyClass `json:"latency,omitempty" yaml:"latency,omitempty"`
	Tags     []string     `json:"tags,omitempty" yaml:"tags,omitempty"`
	// Baseline marks a tool the workflow stage runs unconditionally for its
	// suite, before the model gets a turn. Its result is free context.
	Baseline bool `json:"baseline,omitempty" yaml:"baseline,omitempty"`
	// Fallback keeps a cheap, broadly useful tool in the candidate set even
	// when it scores below the cut, so a narrow shortlist cannot strand the
	// model with nothing generic to fall back on.
	Fallback bool `json:"fallback,omitempty" yaml:"fallback,omitempty"`

	// ── What the gateway enforces ───────────────────────────────────────
	SideEffect     SideEffectLevel `json:"side_effect,omitempty" yaml:"side_effect,omitempty"`
	NeedsApproval  bool            `json:"needs_approval,omitempty" yaml:"needs_approval,omitempty"`
	ApprovalReason string          `json:"approval_reason,omitempty" yaml:"approval_reason,omitempty"`
	// Idempotent permits the dedup cache to answer a repeat call. A tool that
	// samples a moving value (current connections, live QPS) is not idempotent
	// even though it is read-only.
	Idempotent     bool          `json:"idempotent,omitempty" yaml:"idempotent,omitempty"`
	ParallelSafe   bool          `json:"parallel_safe,omitempty" yaml:"parallel_safe,omitempty"`
	Timeout        time.Duration `json:"timeout,omitempty" yaml:"timeout,omitempty"`
	MaxResultBytes int           `json:"max_result_bytes,omitempty" yaml:"max_result_bytes,omitempty"`
	Redactors      []string      `json:"redactors,omitempty" yaml:"redactors,omitempty"`

	// ── Arguments ───────────────────────────────────────────────────────
	InputSchema json.RawMessage `json:"input_schema,omitempty" yaml:"-"`
	// InjectedParams are filled by the host and stripped from the schema the
	// model sees. Cluster, namespace, instance and the incident window belong
	// here: every parameter the model cannot see is a parameter it cannot get
	// wrong.
	InjectedParams []string `json:"injected_params,omitempty" yaml:"injected_params,omitempty"`
	// WindowParams name the arguments that carry the incident window, so the
	// gateway injects it rather than each tool deriving its own time range.
	// Two entries: the start and end argument names.
	WindowParams [2]string `json:"window_params,omitempty" yaml:"window_params,omitempty"`
	// ArgAliases maps a canonical argument name to alternative spellings, so
	// the model may write ns, namespace or kubernetes_namespace and reach the
	// same parameter. Different servers name the same concept differently;
	// this is where that difference stops.
	ArgAliases map[string][]string `json:"arg_aliases,omitempty" yaml:"arg_aliases,omitempty"`
}

// Key identifies a metadata entry by where the tool actually lives. Two
// servers may both expose get_pods, so RemoteName alone is not a key.
func (m ToolMetadata) Key() string {
	remote := m.RemoteName
	if remote == "" {
		remote = m.Name
	}
	return m.Server + "/" + remote
}

// Remote returns the name to call the server with.
func (m ToolMetadata) Remote() string {
	if m.RemoteName != "" {
		return m.RemoteName
	}
	return m.Name
}

// Names lists every name that must resolve to this tool: its own plus aliases.
func (m ToolMetadata) Names() []string {
	out := make([]string, 0, 1+len(m.Aliases))
	out = append(out, m.Name)
	for _, a := range m.Aliases {
		if a = strings.TrimSpace(a); a != "" && a != m.Name {
			out = append(out, a)
		}
	}
	return out
}

// Injects reports whether an argument is host-supplied.
func (m ToolMetadata) Injects(arg string) bool {
	for _, p := range m.InjectedParams {
		if p == arg {
			return true
		}
	}
	if m.WindowParams[0] == arg || m.WindowParams[1] == arg {
		return true
	}
	return false
}

// CanonicalArg resolves an argument alias to its canonical name, returning the
// input unchanged when it is not an alias.
func (m ToolMetadata) CanonicalArg(arg string) string {
	for canonical, aliases := range m.ArgAliases {
		if canonical == arg {
			return arg
		}
		for _, a := range aliases {
			if a == arg {
				return canonical
			}
		}
	}
	return arg
}

// ReadOnly reports whether this tool cannot change anything. An empty
// SideEffect is treated as read-only only for built-ins; an MCP tool with no
// declared level is not assumed safe.
func (m ToolMetadata) ReadOnly() bool {
	if m.SideEffect == "" {
		return m.Server == ""
	}
	return m.SideEffect == SideEffectReadOnly
}

// Candidate is one scored tool from selection, carrying why it was chosen.
//
// The rationale goes into the run record: when a diagnosis fails with "the
// model never looked at X", the first question is whether X was in the
// candidate set at all, and at what score. Candidates are not sent to the
// model — only the selected tools' schemas are.
type Candidate struct {
	Tool       string  `json:"tool"`
	Score      float64 `json:"score"`
	Reason     string  `json:"reason,omitempty"`
	Baseline   bool    `json:"baseline,omitempty"`
	Fallback   bool    `json:"fallback,omitempty"`
	Suppressed string  `json:"suppressed,omitempty"` // set when dropped, says why
}
