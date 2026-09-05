import { describe, expect, it } from 'vitest'
import { cacheShare, fmtBytes, isRetrievable, isTruncated, isWithheld, overviewOf } from '../format'

// The invariant the whole cache-metric chain rests on: a provider that says
// nothing about caching and one that says "nothing was cached" are different
// answers, and rendering both as 0% would send someone tuning a cache that was
// never observable in the first place.
describe('cacheShare', () => {
  it('returns null when the provider reported nothing', () => {
    expect(cacheShare({ prompt: 4595, completion: 4, total: 4599, cached: null })).toBe(null)
    expect(cacheShare({ prompt: 4595 })).toBe(null)
    expect(cacheShare(null)).toBe(null)
  })

  it('returns 0 when the provider reported a genuine miss', () => {
    expect(cacheShare({ prompt: 4595, cached: 0 })).toBe(0)
  })

  it('computes the share of the prompt that was cached', () => {
    // The real figures from the provider probe: 4480 of 4595.
    expect(cacheShare({ prompt: 4595, cached: 4480 })).toBe(97)
  })

  it('does not divide by a zero prompt', () => {
    expect(cacheShare({ prompt: 0, cached: 0 })).toBe(null)
  })
})

// Withheld and truncated are different things: one means this tool's own
// ceiling was hit, the other means the round had no room left. They had one
// label between them, which pointed the reader at max_result_bytes — a setting
// that has nothing to do with the second.
describe('isWithheld', () => {
  const withheld = {
    response: {
      evidence_id: 'e9', summary: 's', truncated: true, retrievable: true,
      preview: 'first lines…',
      overview: { bytes: 4816496, lines: 60000, note: '完整内容未进入本次上下文（本轮剩余预算不足），已保存，可搜索与分段读取。' },
    },
  }
  const truncated = {
    response: { evidence_id: 'e8', summary: 's', truncated: true, retrievable: true, data: { partial: true } },
  }

  it('tells a withheld payload from a truncated one', () => {
    expect(isWithheld(withheld)).toBe(true)
    expect(isWithheld(truncated)).toBe(false)
    // Both are still incomplete, so the truncated flag stays true on each.
    expect(isTruncated(withheld)).toBe(true)
    expect(isTruncated(truncated)).toBe(true)
  })

  it('reports the scale of what was withheld', () => {
    expect(overviewOf(withheld)).toEqual({ bytes: 4816496, lines: 60000 })
    expect(overviewOf(truncated)).toBe(null)
  })

  it('reports whether the rest can still be fetched', () => {
    expect(isRetrievable(withheld)).toBe(true)
    expect(isRetrievable({ response: { evidence_id: 't3', retrievable: false } })).toBe(false)
    // Absent is not the same as false, but both mean "do not try".
    expect(isRetrievable({ response: { evidence_id: 't3' } })).toBe(false)
  })

  it('survives a response that is not an object', () => {
    for (const r of [null, undefined, 'text', 42]) {
      expect(isWithheld({ response: r })).toBe(false)
      expect(overviewOf({ response: r })).toBe(null)
      expect(isRetrievable({ response: r })).toBe(false)
    }
  })
})

describe('fmtBytes', () => {
  it('scales', () => {
    expect(fmtBytes(500)).toBe('500 B')
    expect(fmtBytes(4816496)).toBe('4.6 MB')
    expect(fmtBytes(0)).toBe('')
    expect(fmtBytes(NaN)).toBe('')
  })
})
