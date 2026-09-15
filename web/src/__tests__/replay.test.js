import { describe, expect, it } from 'vitest'
import { replayMessages } from '../replay'
import { finalAnswer, summarize, timelineSteps } from '../timeline'

// Frame builders, matching internal/server/timeline.go.
const f = {
  user: (text) => ({ type: 'user_message', text, ts: 1 }),
  text: (text, extra = {}) => ({ type: 'text', text, round: 'i1', agent: 'root', turn: 1, ts: 10, ...extra }),
  call: (call_id, name, args = {}) =>
    ({ type: 'tool_call', call_id, name, args, agent: 'root', round: 'i1', turn: 1, ts: 10 }),
  result: (call_id, name, response = {}) =>
    ({ type: 'tool_result', call_id, name, response, ok: true, error: '', agent: 'root', round: 'i1', turn: 20 }),
}

describe('replayMessages', () => {
  it('splits a session into alternating user and agent turns', () => {
    const out = replayMessages([
      f.user('巡检 k8s 测试集群'),
      f.text('扫描到 k8s-test。', { final: true }),
      f.user('再看一眼事件'),
      f.text('没有异常事件。', { final: true }),
    ])
    expect(out.map((m) => m.role)).toEqual(['user', 'agent', 'user', 'agent'])
    expect(out[0].text).toBe('巡检 k8s 测试集群')
    expect(finalAnswer(out[1].timeline).text).toBe('扫描到 k8s-test。')
    expect(finalAnswer(out[3].timeline).text).toBe('没有异常事件。')
  })

  // The regression this module exists to prevent. The session browser rendered
  // the transcript DTO, which carries a turn's calls and its results as two
  // separate arrays — so a run of six list_pods calls printed as six calls in
  // one block and six bare "已返回" lines in the next, with nothing saying
  // which answered which. Folding the frames pairs them back into one step.
  it('pairs each call with its own result instead of listing them apart', () => {
    const [, turn] = replayMessages([
      f.user('列一下 pod'),
      f.call('c1', 'k8s_list_pods', { namespace: 'default' }),
      f.call('c2', 'k8s_list_pods', { namespace: 'test' }),
      f.result('c2', 'k8s_list_pods', { items: 2 }),
      f.result('c1', 'k8s_list_pods', { items: 1 }),
    ])
    const tools = timelineSteps(turn.timeline).filter((s) => s.kind === 'tool')
    expect(tools).toHaveLength(2)
    expect(tools.map((s) => s.args.namespace)).toEqual(['default', 'test'])
    // Answered out of order, so pairing by name alone would swap the payloads.
    expect(tools.map((s) => s.response.items)).toEqual([1, 2])
    expect(summarize(turn.timeline).pending).toBe(0)
  })

  it('keeps frames that arrive before any user message', () => {
    const out = replayMessages([f.call('c1', 'k8s_list_nodes'), f.user('继续')])
    expect(out.map((m) => m.role)).toEqual(['agent', 'user', 'agent'])
    expect(timelineSteps(out[0].timeline)).toHaveLength(1)
  })

  it('reports a call still waiting for its result as pending', () => {
    const [, turn] = replayMessages([f.user('巡检'), f.call('c1', 'k8s_diagnose_cluster')])
    expect(summarize(turn.timeline).pending).toBe(1)
  })

  it('tolerates an empty session', () => {
    expect(replayMessages([])).toEqual([])
    expect(replayMessages(undefined)).toEqual([])
  })
})
