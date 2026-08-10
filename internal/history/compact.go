// Package history keeps a conversation inside a token budget before it is sent
// to the model.
//
// Without this, every turn ships the entire history: a few fetch_url calls
// (up to 8000 characters each) push a research session past the context window
// within a handful of turns. Compaction is deterministic — no summarizer model
// call — so it adds no latency, no cost, and no new failure mode.
//
// Two invariants drive the design:
//
//   - Tool-call pairing. convert.go turns a FunctionCall part into an assistant
//     message with tool_calls, and a FunctionResponse part into a tool message
//     carrying tool_call_id. Dropping one side without the other makes the
//     provider reject the whole request, so contents are dropped in groups that
//     keep a call and its responses together.
//   - No mutation of the caller's data. ADK reuses the content objects it hands
//     us; rewriting them in place would corrupt session state and the live
//     stream. Everything here copies before it changes anything.
package history

import (
	"encoding/json"
	"fmt"

	"google.golang.org/genai"

	"github.com/jelly-agent/jelly-agent/internal/tokens"
)

const (
	// DefaultMaxTokens leaves room for the system instruction, the tool
	// schemas and the reply inside a typical 64k context window.
	DefaultMaxTokens = 24000
	// DefaultKeepRecent protects the tail of the conversation — the current
	// question and the exchange leading to it are never dropped.
	DefaultKeepRecent = 6
	// DefaultToolResultTokens is the per-result cap once a tool output is
	// selected for degrading.
	DefaultToolResultTokens = 800

	// perMessageOverhead approximates the role/framing tokens each message
	// costs on the wire.
	perMessageOverhead = 4
)

// Policy configures compaction. A zero Policy means "use the defaults"; a
// MaxTokens of exactly 0 via PolicyFrom disables compaction entirely.
type Policy struct {
	MaxTokens        int
	KeepRecent       int
	ToolResultTokens int
	// CanRecall reports whether the agent actually has the load_memory tool
	// (L2 session search). The omission notice only points at it when true —
	// telling a model to use a tool it was never given invites a hallucinated
	// call. L2 is off by default, so this must not be assumed.
	CanRecall bool
}

func (p Policy) withDefaults() Policy {
	if p.MaxTokens <= 0 {
		p.MaxTokens = DefaultMaxTokens
	}
	if p.KeepRecent <= 0 {
		p.KeepRecent = DefaultKeepRecent
	}
	if p.ToolResultTokens <= 0 {
		p.ToolResultTokens = DefaultToolResultTokens
	}
	return p
}

// Result reports what compaction did, for logging and tests.
type Result struct {
	BeforeTokens int
	AfterTokens  int
	Dropped      int // contents removed outright
	Truncated    int // tool results shortened in place
}

// Changed reports whether compaction altered anything.
func (r Result) Changed() bool { return r.Dropped > 0 || r.Truncated > 0 }

// Compact returns a history that fits the policy's budget, along with a report.
// The input slice and everything reachable from it are left untouched; the
// returned slice shares only the contents that were kept verbatim.
//
// Escalation order, gentlest first:
//  1. shorten old tool results (the bulkiest, least re-readable material),
//  2. drop whole old exchanges, replacing them with one placeholder note,
//  3. as a last resort, shorten tool results inside the protected tail too.
//
// The protected tail itself is never dropped: the user's current question and
// the exchange leading to it must always reach the model.
func Compact(contents []*genai.Content, pol Policy) ([]*genai.Content, Result) {
	pol = pol.withDefaults()
	res := Result{BeforeTokens: EstimateContents(contents)}
	res.AfterTokens = res.BeforeTokens
	if res.BeforeTokens <= pol.MaxTokens || len(contents) == 0 {
		return contents, res
	}

	groups := buildGroups(contents)
	protected := protectedFrom(groups, len(contents), pol.KeepRecent)

	// Work on a copy so the caller's slice is never reordered or rewritten.
	work := make([][]*genai.Content, len(groups))
	for i, g := range groups {
		work[i] = g.items
	}

	// 1. Shorten old tool results.
	for i := 0; i < protected && res.AfterTokens > pol.MaxTokens; i++ {
		shortened, n := truncateToolResults(work[i], pol.ToolResultTokens)
		if n > 0 {
			work[i] = shortened
			res.Truncated += n
			res.AfterTokens = estimateGroups(work)
		}
	}

	// 2. Drop whole old exchanges.
	firstKept := 0
	for firstKept < protected && res.AfterTokens > pol.MaxTokens {
		res.Dropped += len(work[firstKept])
		work[firstKept] = nil
		firstKept++
		res.AfterTokens = estimateGroups(work)
	}

	// 3. Last resort: shorten tool results in the protected tail as well.
	for i := protected; i < len(work) && res.AfterTokens > pol.MaxTokens; i++ {
		shortened, n := truncateToolResults(work[i], pol.ToolResultTokens)
		if n > 0 {
			work[i] = shortened
			res.Truncated += n
			res.AfterTokens = estimateGroups(work)
		}
	}

	out := make([]*genai.Content, 0, len(contents)+1)
	if res.Dropped > 0 {
		out = append(out, placeholder(res.Dropped, pol.CanRecall))
	}
	for _, g := range work {
		out = append(out, g...)
	}
	res.AfterTokens = EstimateContents(out)
	return out, res
}

// placeholder is the note that replaces dropped exchanges, so the model knows
// the history was shortened rather than that it never happened. The recall hint
// is only added when load_memory is actually on the agent's tool list.
func placeholder(dropped int, canRecall bool) *genai.Content {
	msg := fmt.Sprintf("（系统提示：为控制上下文长度，此前 %d 条较早的对话记录已省略。", dropped)
	if canRecall {
		msg += "如需其中的信息，可用 load_memory 检索历史会话。）"
	} else {
		msg += "如果用户提到的内容不在上文中，请直接说明你看不到更早的记录，不要臆测。）"
	}
	return genai.NewContentFromText(msg, genai.RoleUser)
}

// group is one atomic unit of history: a plain message, or a tool call bundled
// with the responses that answer it.
type group struct {
	items []*genai.Content
	end   int // exclusive index into the original slice
}

// buildGroups ties each function-call content to the response contents that
// follow it, so the two can only ever be dropped together.
func buildGroups(contents []*genai.Content) []group {
	var groups []group
	for i := 0; i < len(contents); {
		items := []*genai.Content{contents[i]}
		j := i + 1
		if hasFunctionCall(contents[i]) {
			// Absorb the responses answering this call. ADK emits them as the
			// immediately following contents.
			for j < len(contents) && hasFunctionResponse(contents[j]) {
				items = append(items, contents[j])
				j++
			}
		}
		groups = append(groups, group{items: items, end: j})
		i = j
	}
	return groups
}

// protectedFrom returns the number of leading groups eligible for compaction:
// every group that ends before the keepRecent tail begins. Rounding to whole
// groups is what stops a call from being separated from its responses.
func protectedFrom(groups []group, total, keepRecent int) int {
	if keepRecent >= total {
		return 0
	}
	cutoff := total - keepRecent
	n := 0
	for _, g := range groups {
		if g.end > cutoff {
			break
		}
		n++
	}
	return n
}

// truncateToolResults returns a copy of the group with oversized function
// responses shortened, plus how many were shortened. Contents and parts that
// do not change are shared, never rewritten.
func truncateToolResults(items []*genai.Content, maxTokens int) ([]*genai.Content, int) {
	count := 0
	out := make([]*genai.Content, len(items))
	copy(out, items)
	for i, c := range items {
		if c == nil {
			continue
		}
		var parts []*genai.Part
		for j, p := range c.Parts {
			if p == nil || p.FunctionResponse == nil {
				continue
			}
			shortened, ok := shortenResponse(p.FunctionResponse, maxTokens)
			if !ok {
				continue
			}
			if parts == nil {
				parts = make([]*genai.Part, len(c.Parts))
				copy(parts, c.Parts)
			}
			parts[j] = &genai.Part{FunctionResponse: shortened}
			count++
		}
		if parts != nil {
			out[i] = &genai.Content{Role: c.Role, Parts: parts}
		}
	}
	return out, count
}

// shortenResponse rewrites an oversized tool result as head + tail with a note
// in between, returning ok=false when it already fits. The ID and Name are
// preserved: the ID is the tool_call_id the provider matches on, so losing it
// would break the request.
func shortenResponse(fr *genai.FunctionResponse, maxTokens int) (*genai.FunctionResponse, bool) {
	raw := marshal(fr.Response)
	if tokens.Estimate(raw) <= maxTokens {
		return nil, false
	}
	runes := []rune(raw)
	headTokens := maxTokens * 2 / 3
	tailTokens := maxTokens - headTokens
	head := runes[:prefixWithinTokens(runes, headTokens)]
	tail := runes[len(runes)-suffixWithinTokens(runes, tailTokens):]

	return &genai.FunctionResponse{
		ID:   fr.ID,
		Name: fr.Name,
		Response: map[string]any{
			"truncated": true,
			"note":      fmt.Sprintf("该工具结果过长，中间部分已省略（原始约 %d token）。", tokens.Estimate(raw)),
			"head":      string(head),
			"tail":      string(tail),
		},
	}, true
}

// prefixWithinTokens returns the largest rune count whose prefix fits budget.
func prefixWithinTokens(runes []rune, budget int) int {
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tokens.Estimate(string(runes[:mid])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// suffixWithinTokens returns the largest rune count whose suffix fits budget.
func suffixWithinTokens(runes []rune, budget int) int {
	lo, hi := 0, len(runes)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if tokens.Estimate(string(runes[len(runes)-mid:])) <= budget {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
}

// EstimateContents approximates the token cost of a history slice.
func EstimateContents(contents []*genai.Content) int {
	n := 0
	for _, c := range contents {
		n += estimateContent(c)
	}
	return n
}

func estimateGroups(groups [][]*genai.Content) int {
	n := 0
	for _, g := range groups {
		n += EstimateContents(g)
	}
	return n
}

func estimateContent(c *genai.Content) int {
	if c == nil {
		return 0
	}
	n := perMessageOverhead
	for _, p := range c.Parts {
		switch {
		case p == nil:
			continue
		case p.FunctionCall != nil:
			n += tokens.Estimate(p.FunctionCall.Name) + tokens.Estimate(marshal(p.FunctionCall.Args))
		case p.FunctionResponse != nil:
			n += tokens.Estimate(marshal(p.FunctionResponse.Response))
		default:
			n += tokens.Estimate(p.Text)
		}
	}
	return n
}

func hasFunctionCall(c *genai.Content) bool {
	return c != nil && anyPart(c, func(p *genai.Part) bool { return p.FunctionCall != nil })
}

func hasFunctionResponse(c *genai.Content) bool {
	return c != nil && anyPart(c, func(p *genai.Part) bool { return p.FunctionResponse != nil })
}

func anyPart(c *genai.Content, pred func(*genai.Part) bool) bool {
	for _, p := range c.Parts {
		if p != nil && pred(p) {
			return true
		}
	}
	return false
}

// marshal renders tool arguments/results the same way convert.go sends them,
// so the estimate matches what actually goes on the wire.
func marshal(v map[string]any) string {
	if len(v) == 0 {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
