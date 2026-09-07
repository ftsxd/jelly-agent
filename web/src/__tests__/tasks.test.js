import { describe, expect, it } from 'vitest'
import {
  anyLive, artifactState, artifactsOfStep, emptyReason, isLive, loadTaskList,
  needsAttention, resultKey, resultOf, selectionStore, statusOf, stepOfArtifact,
  stepSummary, taskOfSession, toggleStep, typeLabel,
} from '../tasks'
import { latestOnly } from '../latest'

describe('statusOf', () => {
  it('names every status the backend produces', () => {
    for (const s of ['running', 'completed', 'failed', 'cancelled']) {
      expect(statusOf(s).label).toBeTruthy()
      expect(statusOf(s).icon).toBeTruthy()
    }
  })

  // The three the backend cannot produce yet are still rendered, so the day it
  // can, the page already shows them rather than a blank cell.
  it('names the states that are declared but not yet produced', () => {
    expect(statusOf('waiting_input').label).toBe('等待输入')
    expect(statusOf('blocked').label).toBe('已阻塞')
    expect(statusOf('pending').label).toBe('等待执行')
  })

  // A status nobody has heard of means the backend learned something new. A
  // blank badge would hide that; showing the raw value at least reports it.
  it('falls back legibly rather than blank', () => {
    expect(statusOf('brand_new').label).toBe('brand_new')
    expect(statusOf('').label).toBe('未知')
    expect(statusOf(undefined).label).toBe('未知')
  })

  // Colour is never the only signal: every state carries an icon and a word.
  it('gives every state a word and an icon, not just a colour', () => {
    for (const s of Object.keys({ running: 1, completed: 1, failed: 1, cancelled: 1, pending: 1, waiting_input: 1, blocked: 1 })) {
      const v = statusOf(s)
      expect(v.label).not.toBe('')
      expect(v.icon).not.toBe('')
    }
  })
})

describe('typeLabel', () => {
  it('names the four types and defaults sanely', () => {
    expect(typeLabel('monitor')).toBe('监控查询')
    expect(typeLabel('log')).toBe('日志分析')
    expect(typeLabel('inspection')).toBe('日常巡检')
    expect(typeLabel('other')).toBe('其他任务')
    expect(typeLabel('nonsense')).toBe('其他任务')
  })
})

describe('liveness', () => {
  it('only running counts as live, so an idle page sends no requests', () => {
    expect(isLive({ status: 'running' })).toBe(true)
    for (const s of ['completed', 'failed', 'cancelled', 'waiting_input']) {
      expect(isLive({ status: s })).toBe(false)
    }
    expect(anyLive([{ status: 'completed' }, { status: 'running' }])).toBe(true)
    expect(anyLive([{ status: 'completed' }])).toBe(false)
    expect(anyLive([])).toBe(false)
    expect(anyLive(undefined)).toBe(false)
  })

  // A running task needs nothing from anyone; marking it would make the
  // signal useless on a busy page.
  it('flags only what a person has to act on', () => {
    expect(needsAttention({ status: 'waiting_input' })).toBe(true)
    expect(needsAttention({ status: 'blocked' })).toBe(true)
    expect(needsAttention({ status: 'failed' })).toBe(true)
    expect(needsAttention({ status: 'running' })).toBe(false)
    expect(needsAttention({ status: 'completed' })).toBe(false)
  })
})

// Switching away and back must return to where you were, or comparing two runs
// means starting over on every switch.
//
// It holds no reactive state on purpose. The first version kept the current
// selection in a Map that the component read through a computed — and a plain
// Map is not reactive, so clicking a step did nothing at all. The component
// owns the refs; this owns only the memory.
describe('selectionStore', () => {
  it('keeps a selection per task', () => {
    const sel = selectionStore()
    sel.set('a/1', { step: 's2', artifact: 'e3' })
    sel.set('b/1', { step: 's1' })

    expect(sel.get('a/1')).toEqual({ step: 's2', artifact: 'e3' })
    expect(sel.get('b/1')).toEqual({ step: 's1', artifact: '' })
    expect(sel.get('never-opened')).toEqual({ step: '', artifact: '' })
  })

  it('forgets a task that is gone', () => {
    const sel = selectionStore()
    sel.set('a/1', { step: 's2' })
    sel.forget('a/1')
    expect(sel.get('a/1')).toEqual({ step: '', artifact: '' })
  })
})

// Clicking the open step closes it; clicking another opens that one.
describe('toggleStep', () => {
  it('toggles', () => {
    expect(toggleStep('s1', 's1')).toBe('')
    expect(toggleStep('s1', 's2')).toBe('s2')
    expect(toggleStep('', 's1')).toBe('s1')
  })
})

// The link between a step and its products is the id the server put on each
// artifact — two steps can produce artifacts in the same millisecond, so a
// timestamp guess would cross them.
describe('step ↔ artifact', () => {
  const artifacts = [
    { label: 'e1', step: 't1' },
    { label: 'e2', step: 't1' },
    { label: 'e3', step: 't2' },
    { label: 'report', step: '' },
  ]
  const steps = [{ id: 't1', label: '查询指标' }, { id: 't2', label: '关联日志' }]

  it('finds what a step produced', () => {
    expect(artifactsOfStep(artifacts, 't1').map((a) => a.label)).toEqual(['e1', 'e2'])
    expect(artifactsOfStep(artifacts, 't2').map((a) => a.label)).toEqual(['e3'])
    expect(artifactsOfStep(artifacts, '')).toEqual([])
    expect(artifactsOfStep(undefined, 't1')).toEqual([])
  })

  it('finds where a product came from', () => {
    expect(stepOfArtifact(steps, { step: 't2' }).label).toBe('关联日志')
    expect(stepOfArtifact(steps, { step: '' })).toBe(null)
    expect(stepOfArtifact(steps, undefined)).toBe(null)
  })
})

// Complete, retrievable and expired are three separate facts and one is
// routinely mistaken for another. A truncated-but-retrievable result is normal
// and fine; an expired one is not, and must not read as an empty preview.
describe('artifactState', () => {
  it('an expired product says so instead of previewing nothing', () => {
    const st = artifactState({ expired: true, retrievable: false, complete: false })
    expect(st.readable).toBe(false)
    expect(st.badge).toBe('已过期')
    expect(st.note).toContain('重新查询')
  })

  it('a truncated but retrievable product is readable and says where the rest is', () => {
    const st = artifactState({ complete: false, retrievable: true })
    expect(st.readable).toBe(true)
    expect(st.badge).toBe('部分进入上下文')
    expect(st.note).toContain('可搜索')
  })

  // retrievable is the store's answer, not the old event's. Deriving it from
  // what the model was told marked every result recorded before that flag
  // existed as unsaved, while its bytes sat in the database readable.
  it('a product that is not in the store cannot be read back, and says why', () => {
    const st = artifactState({ complete: false, retrievable: false })
    expect(st.readable).toBe(false)
    expect(st.badge).toBe('未保存')
    expect(st.note).toContain('摘要')
  })

  it('a complete product needs no explanation', () => {
    const st = artifactState({ complete: true, retrievable: true })
    expect(st.readable).toBe(true)
    expect(st.badge).toBe('完整')
    expect(st.note).toBe('')
  })

  // The reply is no longer an artifact at all — it has its own place in the
  // task, so it cannot be confused with a tool product.
  it('an ordinary stored product is readable', () => {
    expect(artifactState({ complete: true, retrievable: true }).readable).toBe(true)
  })

  it('survives nothing at all', () => {
    expect(artifactState(undefined).readable).toBe(false)
  })
})

// "No artifacts" and "not there yet" are different facts; one empty state for
// both tells the reader nothing.
describe('emptyReason', () => {
  it('distinguishes why there is nothing to show', () => {
    expect(emptyReason(null, [])).toContain('选择左侧任务')
    expect(emptyReason({ status: 'running' }, [])).toContain('进行中')
    expect(emptyReason({ status: 'failed' }, [])).toContain('失败')
    expect(emptyReason({ status: 'completed' }, [])).toContain('步骤详情')
    expect(emptyReason({ status: 'completed' }, [{ label: 'e1' }])).toBe('')
  })
})


// The collapsed step row says what happened, preferring the model's own words
// over a description invented here.
describe('stepSummary', () => {
  it('prefers the narration, then the error, then the count', () => {
    expect(stepSummary({ note: '现在拉近 24h 的曲线', calls: 2 })).toBe('现在拉近 24h 的曲线')
    expect(stepSummary({ error: 'i/o timeout', calls: 1 })).toBe('i/o timeout')
    expect(stepSummary({ calls: 3 })).toBe('3 次工具调用')
    expect(stepSummary({})).toBe('')
    expect(stepSummary(null)).toBe('')
  })
})

describe('resultOf', () => {
  // The task centre folds a follow-up run into the task that opened it, so one
  // detail holds several runs — and every run numbers its calls from the start.
  const results = {
    'inv-1/c1': { label: 'e1', call_id: 'c1', round: 'inv-1', retrievable: true },
    'inv-2/c1': { label: 'e4', call_id: 'c1', round: 'inv-2', retrievable: true },
  }

  it('tells two runs\' identically numbered calls apart', () => {
    expect(resultOf(results, { call_id: 'c1', round: 'inv-1' }).label).toBe('e1')
    expect(resultOf(results, { call_id: 'c1', round: 'inv-2' }).label).toBe('e4')
  })

  it('has no answer for a call with no run, rather than a wrong one', () => {
    expect(resultOf(results, { call_id: 'c1' })).toBeNull()
    expect(resultOf(results, null)).toBeNull()
    expect(resultOf(null, { call_id: 'c1', round: 'inv-1' })).toBeNull()
  })

  it('keys on both halves', () => {
    expect(resultKey('inv-1', 'c1')).toBe('inv-1/c1')
    expect(resultKey('inv-1', 'c1')).not.toBe(resultKey('inv-2', 'c1'))
  })
})

describe('taskOfSession', () => {
  // The chat view holds the attachment in a ref that outlives a session
  // switch. The server refuses a task id from another conversation, and the
  // ref was only cleared after a turn succeeded — so one switch made every
  // following message fail the same way, with nothing on screen saying why.
  it('drops an attachment that belongs to another conversation', () => {
    expect(taskOfSession('web-a/inv-1', 'web-b')).toBe('')
  })

  it('keeps the one that belongs to this conversation', () => {
    expect(taskOfSession('web-a/inv-1', 'web-a')).toBe('web-a/inv-1')
  })

  // A session id that is a prefix of another must not match: web-1 is not
  // web-10, and ids are generated with a shared prefix.
  it('matches on the whole session, not a prefix of it', () => {
    expect(taskOfSession('web-10/inv-1', 'web-1')).toBe('')
  })

  it('has nothing to send before a session exists', () => {
    expect(taskOfSession('web-a/inv-1', '')).toBe('')
    expect(taskOfSession('', 'web-a')).toBe('')
  })
})

describe('loadTaskList', () => {
  // What the view actually calls. A test of latestOnly alone does not cover
  // this: it stays green the day the view stops using the gate, which is the
  // regression worth guarding — filters are two selects and a poll fires every
  // three seconds, so a slow 全部 landing after a fast 失败 repopulates the
  // board with rows the filter excludes while the filter still reads 失败.
  const sinkOf = () => {
    const seen = { loading: [], errors: [], data: [] }
    return [seen, {
      loading: (v) => seen.loading.push(v),
      error: (m) => seen.errors.push(m),
      data: (d) => seen.data.push(d),
    }]
  }

  it('applies only the newest response', async () => {
    const gate = latestOnly()
    const [seen, sink] = sinkOf()

    let finishSlow
    const slow = loadTaskList(gate, () => new Promise((r) => { finishSlow = r }), sink)
    await loadTaskList(gate, async () => ({ tasks: ['新的'] }), sink)
    finishSlow({ tasks: ['旧的'] })
    await slow

    expect(seen.data).toEqual([{ tasks: ['新的'] }])
  })

  it('lets an abandoned failure touch neither the error nor the spinner', async () => {
    const gate = latestOnly()
    const [seen, sink] = sinkOf()

    let failSlow
    const slow = loadTaskList(gate, () => new Promise((_, reject) => { failSlow = reject }), sink)
    await loadTaskList(gate, async () => ({ tasks: [] }), sink)
    failSlow(new Error('旧请求失败了'))
    await slow

    expect(seen.errors).toEqual([])
    // Two starts, one stop: the newest call owns the spinner and turned it off.
    expect(seen.loading).toEqual([true, true, false])
  })

  it('reports a failure that is still the newest', async () => {
    const gate = latestOnly()
    const [seen, sink] = sinkOf()
    await loadTaskList(gate, async () => { throw new Error('后端挂了') }, sink)
    expect(seen.errors).toEqual(['后端挂了'])
    expect(seen.data).toEqual([])
    expect(seen.loading).toEqual([true, false])
  })
})
