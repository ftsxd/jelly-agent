import { describe, expect, it } from 'vitest'
import {
  evidenceId,
  fmtArgs,
  fmtDuration,
  fmtTokens,
  isTruncated,
  prettyJSON,
  resultSummary,
  stepLabel,
} from '../format'

const tool = (over = {}) => ({ kind: 'tool', name: 'get_logs', status: 'ok', response: {}, error: '', ...over })

describe('resultSummary', () => {
  // The bug this replaces: the old helper returned "执行中…" whenever a
  // response was empty, so a tool that legitimately answers nothing appeared
  // to run forever. Status is the server's verdict and the only thing that may
  // decide this.
  it('calls an empty successful response finished, not running', () => {
    expect(resultSummary(tool({ status: 'ok', response: null }))).not.toContain('执行中')
  })

  it('reports a failure even when it carries a payload', () => {
    const s = resultSummary(tool({ status: 'failed', response: { partial: 'x' }, error: '连接被拒绝' }))
    expect(s).toBe('连接被拒绝')
  })

  it('says pending only while pending', () => {
    expect(resultSummary(tool({ status: 'pending', response: null }))).toBe('执行中…')
  })

  // Gatewayed tools answer {evidence_id, summary, data, truncated}. That
  // summary was written to be read; a re-serialized data blob was not.
  it('prefers the gateway’s own summary', () => {
    const s = resultSummary(tool({ response: { evidence_id: 'e1', summary: '3 个 Pod 正常', data: { pods: [1, 2, 3] } } }))
    expect(s).toBe('3 个 Pod 正常')
  })

  it('falls back to the data keys when there is no summary', () => {
    expect(resultSummary(tool({ response: { data: { lines: 3 } } }))).toBe('lines=3')
  })

  it('handles a non-object response', () => {
    expect(resultSummary(tool({ response: 'ok' }))).toBe('ok')
  })

  it('returns nothing for a non-tool step', () => {
    expect(resultSummary({ kind: 'text', text: 'hi' })).toBe('')
    expect(resultSummary(null)).toBe('')
  })

  it('truncates a long summary', () => {
    const long = 'x'.repeat(500)
    expect([...resultSummary(tool({ response: { summary: long } }))].length).toBeLessThanOrEqual(161)
  })
})

describe('fmtArgs', () => {
  it('renders arguments compactly', () => {
    expect(fmtArgs({ ns: 'prod', tail: 100 })).toBe('ns=prod, tail=100')
  })

  it('survives nested and null values', () => {
    expect(fmtArgs({ sel: { app: 'api' }, x: null })).toBe('sel={"app":"api"}, x=null')
  })

  it('returns nothing for no arguments', () => {
    expect(fmtArgs(null)).toBe('')
    expect(fmtArgs({})).toBe('')
  })

  it('truncates long values', () => {
    const out = fmtArgs({ q: '日'.repeat(200) }, 30)
    expect([...out].length).toBeLessThanOrEqual(31)
  })

  // Truncation counts code points, not string indices. A JS string is UTF-16,
  // so a BMP character like 日 is one unit and slice() cannot split it — but a
  // supplementary-plane character is a surrogate pair, and slicing at an odd
  // offset leaves half of it behind. An earlier version of this test used
  // Chinese text and therefore could not tell the two implementations apart.
  it('never leaves half a surrogate pair', () => {
    const out = fmtArgs({ q: '🔥'.repeat(50) }, 15)
    for (const ch of out) {
      const cp = ch.codePointAt(0)
      expect(cp >= 0xd800 && cp <= 0xdfff).toBe(false)
    }
  })
})

describe('evidence', () => {
  it('surfaces the evidence id when the gateway supplied one', () => {
    expect(evidenceId(tool({ response: { evidence_id: 'e7' } }))).toBe('e7')
    expect(evidenceId(tool({ response: { n: 1 } }))).toBe('')
    expect(evidenceId({ kind: 'text' })).toBe('')
  })

  it('flags a truncated payload', () => {
    expect(isTruncated(tool({ response: { truncated: true } }))).toBe(true)
    expect(isTruncated(tool({ response: {} }))).toBe(false)
  })
})

describe('numbers', () => {
  it('shortens token counts', () => {
    expect(fmtTokens(0)).toBe('0')
    expect(fmtTokens(999)).toBe('999')
    expect(fmtTokens(1234)).toBe('1.2k')
    expect(fmtTokens(45678)).toBe('46k')
  })

  it('formats durations across the ms/s/m boundaries', () => {
    expect(fmtDuration(0)).toBe('0ms')
    expect(fmtDuration(999)).toBe('999ms')
    expect(fmtDuration(1500)).toBe('1.5s')
    expect(fmtDuration(95000)).toBe('1m35s')
  })

  // A negative gap can only come from a clock problem, and a "-3ms" label
  // would be worse than nothing.
  it('refuses to render an impossible duration', () => {
    expect(fmtDuration(-5)).toBe('')
    expect(fmtDuration(NaN)).toBe('')
    expect(fmtDuration(undefined)).toBe('')
  })
})

describe('labels', () => {
  it('names each kind of step', () => {
    expect(stepLabel({ kind: 'tool', name: 'get_logs' })).toBe('get_logs')
    expect(stepLabel({ kind: 'transfer', from: 'root', to: 'k8s' })).toBe('root → k8s')
    expect(stepLabel({ kind: 'thought' })).toBe('思考')
    expect(stepLabel({ kind: 'error' })).toBe('错误')
    expect(stepLabel({ kind: 'tool', name: '' })).toBe('（未命名工具）')
  })
})

describe('prettyJSON', () => {
  it('pretty-prints and survives what it cannot', () => {
    expect(prettyJSON({ a: 1 })).toBe('{\n  "a": 1\n}')
    expect(prettyJSON(null)).toBe('')
    const cyclic = {}
    cyclic.self = cyclic
    expect(prettyJSON(cyclic)).toBeTypeOf('string')
  })
})
