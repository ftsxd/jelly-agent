import { describe, expect, it } from 'vitest'
import {
  applyFrame,
  emptyTimeline,
  finalAnswer,
  reduceFrames,
  summarize,
  timelineSteps,
} from '../timeline'

// Frame builders, matching internal/server/timeline.go.
const f = {
  session: (id) => ({ type: 'session', session_id: id, v: 2, ts: 1 }),
  user: (text) => ({ type: 'user_message', text, ts: 1 }),
  delta: (text, round = 'i1', agent = 'root') => ({ type: 'text_delta', text, round, agent }),
  text: (text, round = 'i1', agent = 'root', extra = {}) =>
    ({ type: 'text', text, round, agent, turn: 1, ts: 10, ...extra }),
  call: (call_id, name, args = {}, extra = {}) =>
    ({ type: 'tool_call', call_id, name, args, agent: 'root', round: 'i1', turn: 1, ts: 10, ...extra }),
  result: (call_id, name, response = {}, extra = {}) =>
    ({ type: 'tool_result', call_id, name, response, ok: true, error: '', agent: 'root', round: 'i1', turn: 1, ts: 20, ...extra }),
  transfer: (from, to) => ({ type: 'agent_transfer', from, to, ts: 5 }),
  llmTurn: (round, prompt, completion, total, turn = 1) =>
    ({ type: 'llm_turn', round, turn, prompt, completion, total, agent: 'root', ts: 10 }),
  usage: (prompt, completion, total) => ({ type: 'usage', prompt, completion, total }),
  done: (usage) => ({ type: 'done', session_id: 's1', ts: 30, usage }),
  error: (message) => ({ type: 'error', message, ts: 30 }),
}

const tools = (state) => state.steps.filter((s) => s.kind === 'tool')

describe('pairing', () => {
  // Two concurrent calls to the same tool, answered in reverse. Pairing by
  // name and searching backwards — what the old ChatView did — gives each
  // result to the wrong call, so the page shows one call's arguments beside
  // another's result: a call that never happened.
  it('pairs concurrent same-name calls by id', () => {
    const st = reduceFrames([
      f.call('c1', 'get_pods', { ns: 'prod' }),
      f.call('c2', 'get_pods', { ns: 'staging' }),
      f.result('c2', 'get_pods', { n: 2 }),
      f.result('c1', 'get_pods', { n: 1 }),
    ])
    const [a, b] = tools(st)
    expect(a.args).toEqual({ ns: 'prod' })
    expect(a.response).toEqual({ n: 1 })
    expect(b.args).toEqual({ ns: 'staging' })
    expect(b.response).toEqual({ n: 2 })
    expect(a.approx).toBe(false)
  })

  // Without an id the best available guess is first-in-first-answered, and the
  // step has to admit it is a guess.
  it('falls back to FIFO by name and marks it approximate', () => {
    const st = reduceFrames([
      f.call('', 'get_pods', { ns: 'prod' }),
      f.call('', 'get_pods', { ns: 'staging' }),
      { ...f.result('', 'get_pods', { n: 1 }) },
      { ...f.result('', 'get_pods', { n: 2 }) },
    ])
    const [a, b] = tools(st)
    expect(a.response).toEqual({ n: 1 })
    expect(b.response).toEqual({ n: 2 })
    expect(a.approx).toBe(true)
  })

  it('shows a result whose call it never saw', () => {
    const st = reduceFrames([f.result('gone', 'get_pods', { n: 1 })])
    expect(tools(st)).toHaveLength(1)
    expect(tools(st)[0].orphan).toBe(true)
  })

  // A call can legitimately be answered twice. Approval writes a placeholder
  // response when it suspends a tool, then the replay after approval writes
  // the real one under the same id — so the later answer has to win, or the
  // placeholder stays on screen as the result.
  it('lets the last answer for a call win, without a stray row', () => {
    const st = reduceFrames([
      f.call('c1', 'restart'),
      { ...f.result('c1', 'restart', null), ok: false, error: '需要审批' },
      f.result('c1', 'restart', { restarted: true }),
    ])
    expect(tools(st)).toHaveLength(1)
    expect(tools(st)[0].status).toBe('ok')
    expect(tools(st)[0].response).toEqual({ restarted: true })
  })
})

describe('status', () => {
  it('starts pending and settles from the server verdict', () => {
    const st = emptyTimeline()
    applyFrame(st, f.call('c1', 'get_pods'))
    expect(tools(st)[0].status).toBe('pending')
    applyFrame(st, f.result('c1', 'get_pods', { n: 1 }))
    expect(tools(st)[0].status).toBe('ok')
  })

  // A tool that legitimately answers nothing is finished, not running. The old
  // resultSummary decided by whether a response was present, so such a tool
  // displayed "执行中…" forever.
  it('treats an empty successful response as finished', () => {
    const st = reduceFrames([
      f.call('c1', 'noop'),
      { ...f.result('c1', 'noop', null), ok: true },
    ])
    expect(tools(st)[0].status).toBe('ok')
  })

  // And a failure that carries a payload is still a failure.
  it('treats a failure with a payload as failed', () => {
    const st = reduceFrames([
      f.call('c1', 'broken'),
      { ...f.result('c1', 'broken', { partial: 'x' }), ok: false, error: 'connection refused' },
    ])
    expect(tools(st)[0].status).toBe('failed')
    expect(tools(st)[0].error).toBe('connection refused')
  })
})

describe('text', () => {
  it('appends deltas into one step', () => {
    const st = reduceFrames([f.delta('副本'), f.delta('正常')])
    const texts = st.steps.filter((s) => s.kind === 'text')
    expect(texts).toHaveLength(1)
    expect(texts[0].text).toBe('副本正常')
  })

  // Replay sends the whole block. A page that already streamed the deltas must
  // not end up with the answer twice.
  it('assigns rather than appends the replay block', () => {
    const st = reduceFrames([f.delta('副本'), f.delta('正常'), f.text('副本正常')])
    const texts = st.steps.filter((s) => s.kind === 'text')
    expect(texts).toHaveLength(1)
    expect(texts[0].text).toBe('副本正常')
  })

  // The round identity is what makes deltas and the final block fold together.
  // A hardcoded value on one side and a counter on the other — the bug this
  // replaces — produced two separate steps.
  it('folds by round even across a rebuilt state', () => {
    const st = reduceFrames([f.delta('a', 'inv-7'), f.text('ab', 'inv-7')])
    expect(st.steps.filter((s) => s.kind === 'text')).toHaveLength(1)
  })

  // A sub-agent's prose is not the parent's. The old model streamed every
  // author into one string and displayed whichever spoke last, so a handoff
  // produced one bubble attributed to the wrong agent.
  it('keeps each agent’s text in its own step', () => {
    const st = reduceFrames([
      f.delta('父说', 'i1', 'root'),
      f.transfer('root', 'k8s'),
      f.delta('子说', 'i1', 'k8s'),
    ])
    const texts = st.steps.filter((s) => s.kind === 'text')
    expect(texts).toHaveLength(2)
    expect(texts.map((s) => s.agent)).toEqual(['root', 'k8s'])
  })

  // Control returning from a sub-agent emits no transfer frame — the handoff
  // is one-way. So two authors' text can be adjacent with nothing between
  // them, and only the author can separate them. An earlier version of the
  // test above put a transfer between the two blocks, which meant the author
  // check was never exercised: the scan stopped at the transfer either way.
  it('separates adjacent blocks from different agents', () => {
    const st = reduceFrames([
      f.transfer('root', 'k8s'),
      f.delta('子 agent 的结论', 'i1', 'k8s'),
      f.delta('父 agent 汇总', 'i1', 'root'),
    ])
    const texts = st.steps.filter((s) => s.kind === 'text')
    expect(texts.map((s) => s.text)).toEqual(['子 agent 的结论', '父 agent 汇总'])
    expect(texts.map((s) => s.agent)).toEqual(['k8s', 'root'])
  })

  it('starts a new block after a tool call', () => {
    const st = reduceFrames([
      f.delta('先查一下'),
      f.call('c1', 'get_pods'),
      f.result('c1', 'get_pods'),
      f.delta('结果正常'),
    ])
    const texts = st.steps.filter((s) => s.kind === 'text')
    expect(texts.map((s) => s.text)).toEqual(['先查一下', '结果正常'])
  })
})

describe('nesting', () => {
  it('takes depth from branch when present', () => {
    const st = reduceFrames([f.call('c1', 'get_pods', {}, { branch: 'root.k8s' })])
    expect(tools(st)[0].depth).toBe(1)
  })

  // Single-agent runs carry no branch, so the transfer stack fills in.
  it('falls back to the transfer stack without a branch', () => {
    const st = reduceFrames([f.transfer('root', 'k8s'), f.call('c1', 'get_pods')])
    expect(tools(st)[0].depth).toBe(1)
  })

  // Branch wins, because it survives concurrent sub-agents while a stack does
  // not.
  it('prefers branch over the stack', () => {
    const st = reduceFrames([
      f.transfer('root', 'a'),
      f.transfer('a', 'b'),
      f.call('c1', 'x', {}, { branch: 'root.a' }),
    ])
    expect(tools(st)[0].depth).toBe(1)
  })
})

describe('tokens', () => {
  // llm_turn and the legacy usage frame carry the same numbers from the same
  // source. Counting both doubles every total.
  it('counts llm_turn and ignores the legacy usage frame', () => {
    const st = reduceFrames([
      f.llmTurn('i1', 100, 10, 110),
      f.usage(100, 10, 110),
      f.llmTurn('i1', 150, 20, 170, 2),
      f.usage(150, 20, 170),
    ])
    expect(st.usage.total).toBe(280)
  })

  // The turn total is the server's, computed over every round. The old page
  // assigned the per-event usage frame instead, so a turn with tool rounds
  // showed only the last round while the sessions page showed the sum — the
  // same session, two pages, two numbers.
  it('takes the final total from done', () => {
    const st = reduceFrames([
      f.llmTurn('i1', 100, 10, 110),
      f.done({ prompt: 250, completion: 30, total: 280 }),
    ])
    expect(st.usage).toEqual({ prompt: 250, completion: 30, total: 280 })
  })

  it('survives a done without usage', () => {
    const st = reduceFrames([f.llmTurn('i1', 100, 10, 110), { type: 'done', session_id: 's1' }])
    expect(st.usage.total).toBe(110)
    expect(st.done).toBe(true)
  })
})

describe('robustness', () => {
  // The server adds frame types without a handshake, so an older page has to
  // ignore what it does not know rather than break on it.
  it('ignores unknown frames and malformed input', () => {
    const st = reduceFrames([
      { type: 'something_new', whatever: 1 },
      { noType: true },
      null,
      undefined,
      f.delta('ok'),
    ])
    expect(st.steps.filter((s) => s.kind === 'text')[0].text).toBe('ok')
  })

  it('records an error as a step and on the state', () => {
    const st = reduceFrames([f.error('provider timed out')])
    expect(st.error).toBe('provider timed out')
    expect(st.steps.some((s) => s.kind === 'error')).toBe(true)
  })

  it('gives every step a distinct id for keying', () => {
    const st = reduceFrames([
      f.call('c1', 'a'), f.call('c2', 'b'), f.delta('x'), f.transfer('root', 'k'),
    ])
    const ids = st.steps.map((s) => s.id)
    expect(new Set(ids).size).toBe(ids.length)
  })
})

describe('presentation split', () => {
  // The answer stays a single markdown bubble; the working notes go to the
  // timeline. Without the split they render as one undifferentiated wall.
  it('separates the final root answer from the timeline', () => {
    const st = reduceFrames([
      f.user('看下日志'),
      f.call('c1', 'get_logs'),
      f.result('c1', 'get_logs', { lines: 3 }),
      f.text('没有异常。'),
    ])
    expect(finalAnswer(st).text).toBe('没有异常。')
    const shown = timelineSteps(st)
    expect(shown.map((s) => s.kind)).toEqual(['tool'])
  })

  it('does not treat a sub-agent block as the answer', () => {
    const st = reduceFrames([
      f.transfer('root', 'k8s'),
      f.text('子 agent 的中间结论', 'i1', 'k8s', { branch: 'root.k8s' }),
    ])
    expect(finalAnswer(st)).toBe(null)
  })

  it('summarizes what happened', () => {
    const st = reduceFrames([
      f.transfer('root', 'k8s'),
      f.call('c1', 'get_logs', {}, { agent: 'k8s' }),
      { ...f.result('c1', 'get_logs'), ok: false, error: 'boom' },
      f.call('c2', 'get_pods', {}, { agent: 'k8s' }),
      f.llmTurn('i1', 10, 2, 12),
      f.text('结论'),
    ])
    const s = summarize(st)
    expect(s.tools).toBe(2)
    expect(s.failed).toBe(1)
    expect(s.pending).toBe(1)
    expect(s.rounds).toBe(1)
  })
})
