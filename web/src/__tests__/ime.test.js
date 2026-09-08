import { describe, expect, it, vi } from 'vitest'
import { sendOnEnter } from '../ime'

const key = (over = {}) => ({
  isComposing: false,
  keyCode: 13,
  preventDefault: vi.fn(),
  ...over,
})

describe('sendOnEnter', () => {
  it('sends on a plain Enter, and swallows the keystroke', () => {
    const action = vi.fn()
    const h = sendOnEnter(action)
    const e = key()
    expect(h.keydown(e)).toBe(true)
    expect(action).toHaveBeenCalledOnce()
    expect(e.preventDefault).toHaveBeenCalledOnce()
  })

  // The reported bug: the candidate bar is open, Enter picks 湿答答, and the
  // box still holds "s da da". Sending that is unrecoverable.
  it('does not send while the input method is composing', () => {
    const action = vi.fn()
    const h = sendOnEnter(action)
    const e = key({ isComposing: true })
    expect(h.keydown(e)).toBe(false)
    expect(action).not.toHaveBeenCalled()
    // And the keystroke is left to the IME, not swallowed.
    expect(e.preventDefault).not.toHaveBeenCalled()
  })

  // Older WebKit reports the legacy "IME is processing" keyCode instead.
  it('honours the legacy keyCode 229', () => {
    const action = vi.fn()
    const h = sendOnEnter(action)
    expect(h.keydown(key({ keyCode: 229 }))).toBe(false)
    expect(action).not.toHaveBeenCalled()
  })

  // WebKit has dispatched compositionend before the keydown of the same
  // keypress, so isComposing is already false when the handler runs.
  it('ignores the Enter paired with a compositionend in the same turn', () => {
    const action = vi.fn()
    const h = sendOnEnter(action)
    h.compositionend()
    expect(h.keydown(key())).toBe(false)
    expect(action).not.toHaveBeenCalled()
  })

  // But only that one. A deliberate Enter after composing must still send —
  // a time window would have swallowed it.
  it('sends a deliberate Enter after the composition settles', async () => {
    vi.useFakeTimers()
    try {
      const action = vi.fn()
      const h = sendOnEnter(action)
      h.compositionend()
      vi.advanceTimersByTime(0)
      expect(h.keydown(key())).toBe(true)
      expect(action).toHaveBeenCalledOnce()
    } finally {
      vi.useRealTimers()
    }
  })

  it('survives a keydown with no event object', () => {
    const action = vi.fn()
    expect(() => sendOnEnter(action).keydown(undefined)).not.toThrow()
    expect(action).toHaveBeenCalledOnce()
  })
})
