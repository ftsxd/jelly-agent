import { describe, expect, it, vi } from 'vitest'
import { latestOnly } from '../latest'

const defer = () => {
  let resolve, reject
  const promise = new Promise((res, rej) => { resolve = res; reject = rej })
  return { promise, resolve, reject }
}

describe('latestOnly', () => {
  // The exact shape that rendered session A under session B: open a slow one,
  // then a fast one, and let the slow one land last.
  it('discards a slower earlier result', async () => {
    const gate = latestOnly()
    const slow = defer()
    const fast = defer()

    const a = gate.run(() => slow.promise)
    const b = gate.run(() => fast.promise)

    fast.resolve('B')
    slow.resolve('A')

    expect(await b).toEqual({ owned: true, value: 'B' })
    expect(await a).toEqual({ owned: false })
  })

  it('discards a superseded failure, so it cannot overwrite the current page', async () => {
    const gate = latestOnly()
    const slow = defer()
    const fast = defer()

    const a = gate.run(() => slow.promise)
    const b = gate.run(() => fast.promise)

    fast.resolve('B')
    slow.reject(new Error('会话不存在'))

    expect(await b).toEqual({ owned: true, value: 'B' })
    expect(await a).toEqual({ owned: false })
  })

  it('reports a genuine failure of the current call', async () => {
    const gate = latestOnly()
    const r = await gate.run(() => Promise.reject(new Error('boom')))
    expect(r.owned).toBe(true)
    expect(r.error.message).toBe('boom')
  })

  // An abort is the mechanism, not something the user should be shown.
  it('treats an abort as "not mine" rather than as an error', async () => {
    const gate = latestOnly()
    const err = new Error('aborted')
    err.name = 'AbortError'
    expect(await gate.run(() => Promise.reject(err))).toEqual({ owned: false })
  })

  it('aborts the request it replaces', async () => {
    const gate = latestOnly()
    let firstSignal
    const never = defer()
    gate.run((signal) => { firstSignal = signal; return never.promise })
    expect(firstSignal.aborted).toBe(false)

    gate.run(() => Promise.resolve('B'))
    expect(firstSignal.aborted).toBe(true)
  })

  // A caller that awaits again after its first result — scrolling, a second
  // fetch — must be able to re-check ownership.
  it('lets a caller re-check ownership across its own awaits', async () => {
    const gate = latestOnly()
    const first = defer()
    const p = gate.run(async () => {
      const mine = gate.current()
      await first.promise
      expect(gate.owns(mine)).toBe(false) // a newer call started meanwhile
      return 'A'
    })
    gate.run(() => Promise.resolve('B'))
    first.resolve()
    expect(await p).toEqual({ owned: false })
  })

  it('survives an environment without AbortController', async () => {
    const saved = globalThis.AbortController
    // eslint-disable-next-line no-global-assign
    globalThis.AbortController = undefined
    try {
      const gate = latestOnly()
      expect(await gate.run((signal) => Promise.resolve(signal))).toEqual({ owned: true, value: undefined })
    } finally {
      globalThis.AbortController = saved
    }
  })
})

describe('abandon', () => {
  // The gate only invalidated when a new call started, so it covered "switch
  // to another readable product" and nothing else. Switching to an expired
  // one, or to another task, fetches nothing — and the previous request stayed
  // the owner, landing later under a heading that was no longer its own.
  it('disowns an in-flight call without starting one', async () => {
    const gate = latestOnly()
    let release
    const held = new Promise((r) => { release = r })

    const first = gate.run(async () => { await held; return 'e1 的内容' })
    gate.abandon()
    release('e1 的内容')

    const got = await first
    expect(got.owned).toBe(false)
    expect(got.value).toBeUndefined()
  })

  it('aborts the request it disowns', async () => {
    const gate = latestOnly()
    let seen = null
    const first = gate.run((signal) => new Promise((resolve) => {
      seen = signal
      signal?.addEventListener('abort', () => resolve('never used'))
    }))
    gate.abandon()
    expect(seen?.aborted).toBe(true)
    expect((await first).owned).toBe(false)
  })

  // And it does not poison the next one: abandoning is about what is in
  // flight, not about closing the gate.
  it('leaves the next call owned', async () => {
    const gate = latestOnly()
    gate.abandon()
    const got = await gate.run(async () => 'e2 的内容')
    expect(got.owned).toBe(true)
    expect(got.value).toBe('e2 的内容')
  })
})

// The shape the task list needs: an abandoned request must not clear the
// loading flag or write an error, because a newer one owns both.
describe('an abandoned call touches nothing', () => {
  it('reports neither a value nor an error when superseded', async () => {
    const gate = latestOnly()
    let failSlow
    const slow = gate.run(() => new Promise((_, reject) => { failSlow = reject }))
    const fast = await gate.run(async () => ({ tasks: [1, 2] }))
    failSlow(new Error('慢的那个失败了'))

    const stale = await slow
    expect(stale.owned).toBe(false)
    expect(stale.error).toBeUndefined()
    expect(fast.owned).toBe(true)
    expect(fast.value.tasks).toEqual([1, 2])
  })
})
