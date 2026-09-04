// Package selector decides which tools reach the model.
//
// Every registered tool's schema costs prompt tokens on every turn, and the
// cost is paid whether or not the tool is relevant. With a handful of built-ins
// that is not worth managing; with a dozen MCP servers it is the difference
// between a prompt that fits and one that does not — and long before the window
// fills, a catalogue of ninety tools makes the model worse at picking from it.
//
// So this ranks and cuts. Two properties matter more than the ranking quality:
//
//   - Cutting is opt-in and bounded. Below the cap nothing is dropped, so a
//     small deployment behaves exactly as it did before selection existed.
//   - Every decision is explainable. The failure this introduces is "the model
//     never looked at X", and the first question is whether X was a candidate
//     at all and at what score. Select returns that, scores and suppression
//     reasons included, rather than only the survivors.
package selector

import (
	"sort"
	"strings"

	"github.com/jelly-agent/jelly-agent/internal/ops"
)

// Field weights.
//
// Ordered by how deliberate the signal is. A tool's name and its use cases
// were written to say what it is for; a description is prose that may mention
// anything; an anti-example was written specifically to say "not this", and is
// the only negative signal, which is why it outweighs a positive match — the
// tools that need excluding are the ones that look relevant.
const (
	wName        = 6.0
	wUseCase     = 3.0
	wTag         = 2.5 // tags, suites, backend, server
	wExample     = 1.5
	wDescription = 1.0
	wAntiExample = -4.0
)

// Latency is a tiebreaker only. Two tools that match a question equally are
// not equally good to offer, but a slow tool that answers the question still
// beats a fast one that does not — so this reorders within a tier and must
// never move a tool between tiers.
//
// It did, once: the adjustment went into the same total the tiering read, so a
// fast tool that matched nothing scored +0.4, counted as "matched", and
// outranked the fallback tools that exist precisely to cover the case where
// nothing matched. Being quick is not evidence of being relevant.
const (
	bonusFast   = 0.4
	penaltySlow = -0.4
)

// Config bounds a selection.
type Config struct {
	// MaxTools caps how many *scored* tool schemas reach the model. Zero
	// means no cap: every available tool goes in, which is the behaviour
	// before selection existed and the right one for a deployment with few
	// tools.
	//
	// Baseline tools are unconditional and land on top of this number, so a
	// selection can legitimately return more than MaxTools entries. That is
	// deliberate and worth the arithmetic surprise: a baseline tool is one a
	// workflow stage runs before the model gets a turn, so dropping it breaks
	// the stage, while exceeding a token target costs tokens. The two are not
	// comparable, and there are few baseline tools by design.
	MaxTools int
}

// Result is one selection.
type Result struct {
	// Selected names the tools whose schemas go to the model, in the
	// catalogue's declared order — not in rank order.
	//
	// Ranking decides *which* tools go; it has no business deciding how they
	// are laid out. Emitting them ranked meant the same catalogue produced a
	// different tool block for every question, so a provider's prefix cache
	// missed on a prompt that was otherwise identical, and a deployment below
	// its budget — where selection is supposed to change nothing — still had
	// its prompt reshuffled on every turn.
	//
	// The ranking is not lost: Candidates carries it, in rank order.
	Selected []string
	// Candidates covers everything that was considered, ranked, including the
	// ones that were cut and why. This is the record that answers "was X even
	// offered?" — the question that a wrong cut turns into a debugging dead
	// end without it.
	Candidates []ops.Candidate
	// Capped reports whether the cap actually removed anything, so a caller
	// can tell "selection ran" from "selection changed the outcome".
	Capped bool
}

// Select ranks tools against a question and applies the cap.
//
// query is the user's message for this turn. An empty query is not an error
// and not a reason to guess: with nothing to rank against, everything scores
// zero, the declared order stands, and the cap still applies — which is a
// deliberate choice to keep the prompt bounded rather than to keep it relevant.
func Select(query string, tools []ops.ToolMetadata, cfg Config) Result {
	q := tokenize(query)

	// order preserves the declared sequence as the last tiebreaker, so an
	// identical catalogue and an identical question always produce an
	// identical prompt — otherwise a cache miss or a changed answer could come
	// from map iteration rather than from anything real.
	order := make(map[string]int, len(tools))
	cands := make([]ops.Candidate, 0, len(tools))
	tiers := make(map[string]int, len(tools))
	for i, m := range tools {
		sc, relevance, reason := score(q, m)
		order[m.Name] = i
		tiers[m.Name] = tierOf(m, relevance)
		cands = append(cands, ops.Candidate{
			Tool: m.Name, Score: sc, Reason: reason,
			Baseline: m.Baseline, Fallback: m.Fallback,
		})
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if ta, tb := tiers[a.Tool], tiers[b.Tool]; ta != tb {
			return ta < tb
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return order[a.Tool] < order[b.Tool]
	})

	budget := cfg.MaxTools
	if budget <= 0 {
		budget = len(cands)
	}
	picked := make([]string, 0, len(cands))
	for i := range cands {
		// Baseline does not spend budget. Letting it do so would mean a
		// deployment that declares one loses a slot for a tool that actually
		// answers the question — the budget would be enforced by removing the
		// relevant tool, which is the one outcome selection must not produce.
		if cands[i].Baseline {
			picked = append(picked, cands[i].Tool)
			continue
		}
		if budget > 0 {
			picked = append(picked, cands[i].Tool)
			budget--
			continue
		}
		cands[i].Suppressed = "超出本次工具预算"
	}

	// Back to the declared order for the prompt. See Result.Selected.
	sort.SliceStable(picked, func(i, j int) bool { return order[picked[i]] < order[picked[j]] })
	return Result{Selected: picked, Candidates: cands, Capped: len(picked) < len(cands)}
}

// tier orders the four kinds of candidate.
//
// Baseline is unconditional: a workflow stage runs it before the model gets a
// turn, so dropping it breaks the stage rather than merely narrowing a choice.
//
// A tool that matched the question comes next, ahead of Fallback — this is the
// part worth stating, because the obvious arrangement is wrong. Ranking
// Fallback above everything scored would let a generic tool displace one that
// actually answers the question whenever the budget is tight, which is the
// opposite of what Fallback is for. Its job is to survive against tools that
// matched nothing, so that a narrow shortlist cannot strand the model with no
// general-purpose option — not to outrank a hit.
// relevance is the net lexical score — positive matches minus anti-example
// penalties, with the latency tiebreaker excluded. Both exclusions matter and
// both were bugs:
//
// Latency, because a tool is not relevant for being fast. A quick tool that
// matched nothing used to score +0.4, count as a match, and outrank the
// fallback tools that exist for exactly that case.
//
// Anti-examples, because a tool whose anti-example fires harder than its
// positive match is the metadata author saying "this looks relevant and is
// not" — the whole reason the field exists. Tiering on "matched anything at
// all" kept such a tool in the matched tier at −18 points, where it still beat
// an unmatched fallback at 0 and took the last budget slot.
func tierOf(m ops.ToolMetadata, relevance float64) int {
	switch {
	case m.Baseline:
		return 0
	case relevance > 0:
		return 1
	case m.Fallback:
		return 2
	default:
		return 3
	}
}

// score rates one tool against the question and says why, in words an operator
// reading a run record can act on.
//
// It returns two numbers because they answer different questions. total ranks
// within a tier and includes the latency tiebreaker; relevance decides which
// tier the tool is in and is purely lexical — see tierOf for why conflating
// them was wrong twice over.
func score(q map[string]bool, m ops.ToolMetadata) (total, relevance float64, reason string) {
	if len(q) == 0 {
		return 0, 0, ""
	}
	var hits []string

	add := func(weight float64, label string, texts ...string) {
		n := 0
		for _, t := range texts {
			n += overlap(q, tokenize(t))
		}
		if n == 0 {
			return
		}
		relevance += weight * float64(n)
		hits = append(hits, label)
	}

	add(wName, "名称", m.Names()...)
	add(wUseCase, "适用场景", m.UseCases...)
	add(wTag, "标签", append(append([]string{m.Backend, m.Server}, m.Tags...), m.Suites...)...)
	add(wExample, "示例", m.Examples...)
	add(wDescription, "描述", m.Description)

	if n := countOverlap(q, m.AntiExamples); n > 0 {
		relevance += wAntiExample * float64(n)
		hits = append(hits, "反例命中")
	}

	total = relevance
	switch m.Latency {
	case ops.LatencyFast:
		total += bonusFast
	case ops.LatencySlow:
		total += penaltySlow
	}

	if len(hits) == 0 {
		return total, relevance, ""
	}
	return total, relevance, "匹配 " + strings.Join(hits, "、")
}

func countOverlap(q map[string]bool, texts []string) int {
	n := 0
	for _, t := range texts {
		n += overlap(q, tokenize(t))
	}
	return n
}
