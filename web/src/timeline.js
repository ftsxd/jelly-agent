// Folding a run's frames into a timeline.
//
// A pure reducer, deliberately outside any component. The live stream feeds it
// frames as they arrive; replay feeds it the stored array. There is no second
// code path, which is the whole point: the previous arrangement had ChatView
// and SessionsView each assemble a run their own way, and the two pages
// disagreed about the same session — different pairing, different token
// totals, different notion of what "still running" meant.
//
// Keeping it here rather than in the .vue file is also what makes it testable
// without a DOM or a component harness. The component receives finished steps
// and renders them; it is given no frames, so putting logic back into it would
// be inconvenient rather than merely discouraged.
//
// The frame vocabulary is defined by internal/server/timeline.go. That file is
// the authority; this one follows it.

/** A fresh, empty timeline. */
export function emptyTimeline() {
  return {
    steps: [],
    /** call_id → tool step, for pairing results with their call. */
    byCallId: new Map(),
    /** round id → { turn, prompt, completion, total } */
    rounds: new Map(),
    /**
     * How many times the model was called.
     *
     * This is the number worth showing, not the number of distinct rounds: a
     * round is an invocation, so a single chat turn always has exactly one and
     * the count carries no information. What tells you a run went in circles
     * is how often the model had to be asked again.
     */
    llmCalls: 0,
    usage: { prompt: 0, completion: 0, total: 0 },
    /** Set once the run reports it finished. */
    done: false,
    error: '',
    sessionId: '',
    /** Agent stack, pushed on transfer, so steps nest without a branch. */
    stack: [],
  }
}

let nextId = 0
function stepId() {
  nextId += 1
  return `s${nextId}`
}

// Depth comes from branch when there is one. Branch is "agent_1.agent_2..." —
// authoritative, and it keeps working when sub-agents run concurrently, where
// tracking "the current agent" would interleave and mis-nest. The transfer
// stack is only the fallback for single-agent runs, which carry no branch.
function depthOf(frame, state) {
  if (frame.branch) return Math.max(0, frame.branch.split('.').length - 1)
  return state.stack.length
}

/**
 * Fold one frame into the timeline. Mutates and returns state.
 *
 * An unknown frame type is ignored, not an error: the server adds frame types
 * without a protocol handshake, so a page that predates one has to keep
 * working rather than break on the first unrecognised name.
 */
export function applyFrame(state, frame) {
  if (!frame || typeof frame.type !== 'string') return state
  switch (frame.type) {
    case 'session':
      state.sessionId = frame.session_id || ''
      break

    case 'user_message':
      // Carried so replay starts where the conversation started. ChatView
      // renders user turns itself, so it filters these out; SessionsView shows
      // them.
      state.steps.push({
        id: stepId(), kind: 'user', text: frame.text || '', ts: frame.ts || 0, depth: 0,
      })
      break

    case 'text_delta':
      textStep(state, frame).text += frame.text || ''
      break

    case 'text':
      // Replay's whole-block form. Assigned rather than appended so that a
      // page which already streamed the deltas does not end up with the answer
      // twice.
      textStep(state, frame).text = frame.text || ''
      break

    case 'thought':
      state.steps.push({
        id: stepId(), kind: 'thought', text: frame.text || '',
        agent: frame.agent || '', round: frame.round || '', turn: frame.turn || 0,
        ts: frame.ts || 0, depth: depthOf(frame, state),
      })
      break

    case 'tool_call': {
      const step = {
        id: stepId(), kind: 'tool', status: 'pending',
        name: frame.name || '', args: frame.args ?? null,
        response: null, error: '',
        callId: frame.call_id || '', agent: frame.agent || '',
        branch: frame.branch || '', round: frame.round || '', turn: frame.turn || 0,
        ts: frame.ts || 0, depth: depthOf(frame, state), approx: false,
      }
      state.steps.push(step)
      if (step.callId) state.byCallId.set(step.callId, step)
      break
    }

    case 'tool_result':
      settleTool(state, frame)
      break

    case 'agent_transfer':
      state.stack.push(frame.to || '')
      state.steps.push({
        id: stepId(), kind: 'transfer', from: frame.from || '', to: frame.to || '',
        ts: frame.ts || 0, depth: Math.max(0, state.stack.length - 1),
      })
      break

    case 'llm_turn': {
      // The authority for per-round tokens. The legacy "usage" frame carries
      // the same numbers from the same source, so exactly one of the two may
      // be counted — adding both doubles every total.
      state.llmCalls += 1
      const r = frame.round || ''
      state.rounds.set(r, {
        turn: frame.turn || 0,
        prompt: frame.prompt || 0,
        completion: frame.completion || 0,
        total: frame.total || 0,
        finishReason: frame.finish_reason || '',
      })
      state.usage.prompt += frame.prompt || 0
      state.usage.completion += frame.completion || 0
      state.usage.total += frame.total || 0
      break
    }

    case 'usage':
      // Deliberately ignored. See llm_turn.
      break

    case 'done':
      state.done = true
      if (frame.session_id) state.sessionId = frame.session_id
      // The turn's own total, computed server-side over every round. Trusted
      // over the running sum because it is the one number both this page and
      // the sessions page read.
      if (frame.usage) state.usage = { ...state.usage, ...frame.usage }
      break

    case 'error':
      state.error = frame.message || '未知错误'
      state.steps.push({
        id: stepId(), kind: 'error', text: state.error,
        agent: frame.agent || '', ts: frame.ts || 0, depth: 0,
      })
      break

    default:
      break // unknown frame types are ignored on purpose
  }
  return state
}

/** Fold a whole array of frames, for replay. */
export function reduceFrames(frames, state = emptyTimeline()) {
  for (const f of frames || []) applyFrame(state, f)
  return state
}

// textStep finds the text step a frame belongs to, or starts one.
//
// Keyed by (round, agent) so that a sub-agent's prose does not merge into its
// parent's. That was the old behaviour: every author streamed into one string
// and the displayed author was whichever spoke last, so a handoff produced one
// bubble attributed to the wrong agent.
function textStep(state, frame) {
  const round = frame.round || ''
  const agent = frame.agent || ''
  for (let i = state.steps.length - 1; i >= 0; i -= 1) {
    const s = state.steps[i]
    if (s.kind === 'text' && s.round === round && s.agent === agent) return s
    // Only the tail is a candidate: once something else has happened, the
    // model has moved on and later text is a new block.
    if (s.kind === 'tool' || s.kind === 'transfer') break
  }
  const step = {
    id: stepId(), kind: 'text', text: '',
    agent, round, turn: frame.turn || 0,
    branch: frame.branch || '', ts: frame.ts || 0, depth: depthOf(frame, state),
  }
  state.steps.push(step)
  return step
}

// settleTool attaches a result to the call it answers.
//
// By call id, which the server guarantees on both frames (ADK fills any empty
// id before the event is emitted). The name-based fallback below exists only
// for a frame that somehow arrives without one, and says so: pairing two
// concurrent calls to the same tool by name hands each result to the wrong
// call, so the display shows one call's arguments beside another's result — a
// call that never happened.
function settleTool(state, frame) {
  const status = frame.ok === false ? 'failed' : 'ok'
  const fill = (step, approx) => {
    step.response = frame.response ?? null
    step.error = frame.error || ''
    step.status = status
    if (approx) step.approx = true
    if (!step.name) step.name = frame.name || ''
  }

  const byId = frame.call_id ? state.byCallId.get(frame.call_id) : null
  if (byId) {
    // The last answer for a call wins, and the index entry is kept so a later
    // one can find it. A call can legitimately be answered twice: an approval
    // that suspends a tool writes a placeholder response, and the replay after
    // the approval writes the real one under the same id. Treating the second
    // as an orphan would leave the placeholder on screen as the result.
    fill(byId, false)
    return
  }

  // Oldest pending call of the same name — first in, first answered. The
  // earlier implementation searched backwards, which pairs a burst of calls in
  // reverse.
  const pending = state.steps.find(
    (s) => s.kind === 'tool' && s.status === 'pending' && s.name === frame.name,
  )
  if (pending) {
    fill(pending, true)
    return
  }

  // An answer to a call this timeline never saw — a truncated session, or a
  // replay that starts mid-run. Shown rather than dropped.
  state.steps.push({
    id: stepId(), kind: 'tool', status, name: frame.name || '',
    args: null, response: frame.response ?? null, error: frame.error || '',
    callId: frame.call_id || '', agent: frame.agent || '',
    branch: frame.branch || '', round: frame.round || '', turn: frame.turn || 0,
    ts: frame.ts || 0, depth: depthOf(frame, state), approx: true, orphan: true,
  })
}

/**
 * The reply to show as the main answer bubble: the last root-level text.
 *
 * Sub-agent prose stays in the timeline as nested blocks. Without this split
 * the answer and its working notes render as one undifferentiated wall.
 */
export function finalAnswer(state) {
  for (let i = state.steps.length - 1; i >= 0; i -= 1) {
    const s = state.steps[i]
    if (s.kind === 'text' && s.depth === 0) return s
  }
  return null
}

/** Steps the timeline should render, i.e. everything except the main answer. */
export function timelineSteps(state) {
  const answer = finalAnswer(state)
  return state.steps.filter((s) => s !== answer && s.kind !== 'user')
}

/** A one-line summary for the timeline's header. */
export function summarize(state) {
  const steps = timelineSteps(state)
  const tools = steps.filter((s) => s.kind === 'tool')
  return {
    steps: steps.length,
    tools: tools.length,
    failed: tools.filter((s) => s.status === 'failed').length,
    pending: tools.filter((s) => s.status === 'pending').length,
    agents: new Set(steps.map((s) => s.agent).filter(Boolean)).size,
    llmCalls: state.llmCalls,
    usage: state.usage,
  }
}
