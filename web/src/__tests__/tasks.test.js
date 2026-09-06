import { describe, expect, it } from 'vitest'
import {
  anyLive, artifactState, artifactsOfStep, emptyReason, isLive, needsAttention,
  selectionStore, statusOf, stepOfArtifact, stepSummary, toggleStep, typeLabel,
} from '../tasks'

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
