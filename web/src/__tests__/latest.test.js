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
