package server

// Projecting an agent run into a timeline.
//
// One vocabulary of frames, produced by one function, consumed by two entry
// points: the live SSE stream and the after-the-fact replay endpoint. The sink
// is an interface so that "write to the client" and "collect into a slice" are
// the only difference between them.
//
// The alternative — having the replay endpoint return an already-assembled
// tree — was rejected because the live path has to fold frames incrementally
// no matter what (they arrive one at a time), so a server-side assembly would
// be a second implementation of the same folding. Two implementations of one
// rule drift, and this codebase already shows what that looks like: ChatView
// and SessionsView each grew their own fmtArgs, their own call/result pairing,
// and their own token accounting, and the two pages disagree about the same
// session today.
//
// Everything here reads fields ADK already emits. Nothing new is collected and
// no schema changes: events.content stores the whole genai.Content as JSON, so
// FunctionCall.ID, Actions.TransferToAgent and Branch come back out of the
// database exactly as they went in.

import (
	"slices"
	"strings"

	adksession "google.golang.org/adk/session"
	"google.golang.org/adk/tool/toolconfirmation"
	"google.golang.org/genai"
)

// Frame types. The value goes in each frame's "type" field.
//
// Adding a type is backwards compatible: the browser's parser is
// type-agnostic and ChatView's switch has no default branch, so a client that
// predates a frame ignores it. That is why there is no protocol negotiation
// here — only frameVersion, which exists so a bug report can say which
// protocol the reporter was running, and which no code branches on.
const (
	frameSession       = "session"
	frameUserMessage   = "user_message"
	frameTextDelta     = "text_delta"
	frameText          = "text"
	frameThought       = "thought"
	frameToolCall      = "tool_call"
	frameToolResult    = "tool_result"
	frameAgentTransfer = "agent_transfer"
	frameLLMTurn       = "llm_turn"
	frameUsage         = "usage" // deprecated: superseded by done.usage
	frameDone          = "done"
	frameError         = "error"
)

// frameVersion is diagnostic only. The frontend ships embedded in this binary
// (//go:embed all:dist), so a version mismatch can only happen in a tab left
// open across an upgrade — a tab that already breaks today. Branching on this
// would be designing for a case that does not exist.
const frameVersion = 2

// sink receives frames. The SSE writer sends them; the replay collector
// appends them.
type sink interface {
	frame(typ string, payload map[string]any)
}

// collector is the replay sink.
type collector struct{ frames []map[string]any }

func (c *collector) frame(typ string, payload map[string]any) {
	if payload == nil {
		payload = map[string]any{}
	}
	payload["type"] = typ
	c.frames = append(c.frames, payload)
}

// turnState carries what a frame needs but a single event does not hold.
//
// It exists because the interesting facts about a run are relationships
// between events — which call this result answers, how long it took, which
// agent handed off to which — and an event on its own knows none of them.
type turnState struct {
	// invocation is the id of the invocation being projected, and turn counts
	// model responses within it.
	//
	// The round identity has to be ADK's invocation id rather than a counter
	// of our own, because the same projector runs over two different scopes: a
	// live request sees one invocation, a replay walks every invocation in the
	// session. A counter therefore numbered the same answer 1 live and 3 on
	// replay — so continuing a stored session collided with itself, and a
	// refresh renumbered everything. The invocation id is the same value in
	// both paths, and every event of one response carries it, partial and
	// final alike.
	invocation string
	turn       int

	// totalPrompt and friends accumulate token counts. What they total depends
	// on scope, and both meanings are the right one for their caller: live,
	// this is the turn the user just took; on replay, it is the whole session,
	// which is what the sessions page has always shown.
	//
	// They exist because the per-event "usage" frame is assigned rather than
	// added by the browser, so a turn with three tool rounds displayed only
	// the last round's tokens while the sessions page displayed the sum — the
	// same session, two pages, two numbers.
	totalPrompt, totalCompletion, totalTokens int32
}

func newTurnState() *turnState { return &turnState{} }

// enter moves the state onto an event's invocation, resetting the per-response
// counter when the invocation changes.
func (s *turnState) enter(invocation string) {
	if invocation != s.invocation {
		s.invocation = invocation
		s.turn = 0
	}
}

// usage returns the turn's accumulated token counts.
func (s *turnState) usage() map[string]any {
	return map[string]any{
		"prompt":     s.totalPrompt,
		"completion": s.totalCompletion,
		"total":      s.totalTokens,
	}
}

// project turns one ADK event into frames.
//
// Events it does not recognise produce nothing. That is deliberate and is the
// one place in this file that defaults to refusal: the old code dropped every
// event without Content, which silently threw away agent transfers, and the
// obvious fix — forward anything — would put whatever internal bookkeeping
// event ADK adds next straight onto the user's screen. A whitelist keeps the
// transfers and keeps the noise out.
func project(ev *adksession.Event, out sink, st *turnState) {
	if ev == nil {
		return
	}
	ts := ev.Timestamp.UnixMilli()
	st.enter(ev.InvocationID)

	// A transfer is carried by Actions, not by Content, so it has to be read
	// before the Content check below.
	if to := strings.TrimSpace(ev.Actions.TransferToAgent); to != "" {
		out.frame(frameAgentTransfer, map[string]any{
			"from": ev.Author, "to": to, "ts": ts, "branch": ev.Branch,
		})
	}
	if ev.Content == nil {
		return
	}

	if ev.Partial {
		projectPartial(ev, out, st)
		return
	}
	projectFinal(ev, out, st, ts)
}

// projectPartial forwards streaming text.
//
// One frame arrives per token and the browser yields to the event loop between
// frames, so every field here is paid thousands of times per answer. It
// carries the round identity and nothing else derived: timestamps and branch
// live on the round-boundary frames, which the reducer finds by round.
//
// The round is what makes the deltas foldable into the same step as the final
// text. An earlier version put a constant here and a counter on the final
// text, so the two never matched and the folding the comment described could
// not work.
func projectPartial(ev *adksession.Event, out sink, st *turnState) {
	for _, p := range ev.Content.Parts {
		if p == nil || p.Thought || p.Text == "" {
			continue
		}
		out.frame(frameTextDelta, map[string]any{
			"text": p.Text, "agent": ev.Author, "round": st.invocation,
		})
	}
}

func projectFinal(ev *adksession.Event, out sink, st *turnState, ts int64) {
	role := ""
	if ev.Content != nil {
		role = strings.ToLower(ev.Content.Role)
	}

	// A model response opens a new turn within the invocation. Function
	// responses come back with role "user", so they do not.
	if role == "model" {
		st.turn++
	}

	for _, p := range ev.Content.Parts {
		if p == nil {
			continue
		}
		switch {
		case p.FunctionCall != nil:
			projectCall(ev, p.FunctionCall, out, st, ts)
		case p.FunctionResponse != nil:
			projectResponse(ev, p.FunctionResponse, out, st, ts)
		case p.Text != "" && role == "user":
			out.frame(frameUserMessage, map[string]any{"text": p.Text, "ts": ts})
		case p.Text != "" && p.Thought:
			out.frame(frameThought, map[string]any{
				"text": p.Text, "agent": ev.Author, "round": st.invocation, "turn": st.turn, "ts": ts,
			})
		case p.Text != "":
			// Replay only in practice: the live path already streamed this as
			// deltas. The reducer folds both into one text step — delta
			// appends, text sets — so the two paths converge.
			out.frame(frameText, map[string]any{
				"text": p.Text, "agent": ev.Author, "branch": ev.Branch,
				"round": st.invocation, "turn": st.turn, "ts": ts,
			})
		}
	}

	if ev.UsageMetadata != nil {
		u := ev.UsageMetadata
		st.totalPrompt += u.PromptTokenCount
		st.totalCompletion += u.CandidatesTokenCount
		st.totalTokens += u.TotalTokenCount
		out.frame(frameLLMTurn, map[string]any{
			"round": st.invocation, "turn": st.turn,
			"agent": ev.Author, "branch": ev.Branch, "ts": ts,
			"prompt": u.PromptTokenCount, "completion": u.CandidatesTokenCount,
			"total": u.TotalTokenCount, "finish_reason": string(ev.FinishReason),
		})
		// Kept for the frontend that predates llm_turn. Remove once no
		// consumer reads it.
		out.frame(frameUsage, map[string]any{
			"prompt": u.PromptTokenCount, "completion": u.CandidatesTokenCount,
			"total": u.TotalTokenCount,
		})
	}
}

// projectCall emits a tool call, except for the two calls that are not work.
func projectCall(ev *adksession.Event, fc *genai.FunctionCall, out sink, st *turnState, ts int64) {
	switch {
	case fc.Name == transferToAgentTool:
		// The transfer already went out as its own frame, from Actions, which
		// is the authoritative signal. Letting it through here as well would
		// show a handoff as an ordinary tool, and "this turn called five
		// tools" would stop being true.
		return
	case fc.Name == toolconfirmation.FunctionCallName:
		// Approval requests are their own thing; batch three gives them a
		// frame. Until then they are not a tool call.
		return
	}

	out.frame(frameToolCall, map[string]any{
		"name": fc.Name, "args": fc.Args, "agent": ev.Author,
		"call_id": fc.ID, "branch": ev.Branch, "round": st.invocation, "turn": st.turn, "ts": ts,
	})
}

func projectResponse(ev *adksession.Event, fr *genai.FunctionResponse, out sink, st *turnState, ts int64) {
	if fr.Name == transferToAgentTool || fr.Name == toolconfirmation.FunctionCallName {
		return
	}

	// No duration. ADK waits for every parallel tool and then emits one merged
	// response event that reuses the first tool's event, timestamp included
	// (base_flow.go:1346, "reuse events[0]"). So a difference between the call
	// and result timestamps is neither how long the tool took nor when the
	// frame was delivered: with a tool that returns at once and one that takes
	// 100ms, both results report the same 1ms.
	//
	// A number that looks like a duration and is not one is worse than no
	// number, because nobody re-checks a number that renders. Real per-call
	// timing already exists in tool_calls (duration_ms, keyed by call id) and
	// gets wired into both paths together, so live and replay will never show
	// two different figures for the same call.
	out.frame(frameToolResult, map[string]any{
		"name": fr.Name, "response": fr.Response, "agent": ev.Author,
		"call_id": fr.ID, "branch": ev.Branch, "ts": ts,
		"round": st.invocation, "turn": st.turn,
		// Success is the server's judgement, from the same helper the metrics
		// recorder uses. The browser must not re-derive it: a tool that
		// legitimately answers null is not still running.
		"ok": !toolFailed(fr.Response), "error": toolError(fr.Response),
	})
}

// transferToAgentTool is the name ADK gives its built-in handoff tool.
const transferToAgentTool = "transfer_to_agent"

// projectAll runs a stored event list through the projector, for replay.
func projectAll(events []*adksession.Event) ([]map[string]any, map[string]any) {
	c := &collector{}
	st := newTurnState()
	for _, ev := range events {
		project(ev, c, st)
	}
	return c.frames, st.usage()
}

// pendingApprovals reports the confirmation calls in a session that have no
// answer yet.
//
// Derived from the event stream rather than tracked in a table because the
// stream is the only authoritative record: an approval that was granted is a
// FunctionResponse sitting next to its request, and a second store would only
// be able to disagree with it. Unused until batch three; defined here because
// it is the same projection.
func pendingApprovals(events []*adksession.Event) []string {
	var requested []string
	answered := map[string]bool{}
	for _, ev := range events {
		if ev == nil || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p == nil {
				continue
			}
			if p.FunctionCall != nil && p.FunctionCall.Name == toolconfirmation.FunctionCallName {
				requested = append(requested, p.FunctionCall.ID)
			}
			if p.FunctionResponse != nil && p.FunctionResponse.Name == toolconfirmation.FunctionCallName {
				answered[p.FunctionResponse.ID] = true
			}
		}
	}
	return slices.DeleteFunc(requested, func(id string) bool { return answered[id] })
}
