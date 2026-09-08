/**
 * An Enter that belongs to the input method is not an Enter that sends.
 *
 * Typing Chinese means composing: keystrokes go to the IME, a candidate bar
 * opens, and Enter picks the highlighted candidate. That Enter still reaches
 * the page as a keydown, so a handler that only asks "was the key Enter?"
 * sends whatever half-composed pinyin is in the box — "s da da" instead of
 * 湿答答. It is unrecoverable in the chat: the message is gone.
 *
 * Three signals, because no single one covers every browser:
 *
 *   - event.isComposing is the standard answer and is what Chrome and Firefox
 *     set while the candidate bar is open.
 *   - keyCode 229 is the legacy "the IME is processing this" value, still what
 *     some older WebKit builds report instead of setting isComposing.
 *   - A compositionend in the same event-loop turn. WebKit has historically
 *     dispatched compositionend *before* the keydown of the same keypress, so
 *     isComposing is already false by the time the handler runs. The flag is
 *     cleared on the next macrotask rather than after a delay, so it covers
 *     exactly the keydown the browser pairs with that compositionend and never
 *     a deliberate Enter a moment later — a time window would swallow one.
 *
 * This lives in a .js file because there is no component test harness here,
 * and a guard nobody can test is a guard that quietly stops working. The
 * failure it prevents is silent and destructive, which is the worst pair.
 */
export function sendOnEnter(action) {
  let justComposed = false

  return {
    /** Bind to @compositionend. */
    compositionend() {
      justComposed = true
      setTimeout(() => {
        justComposed = false
      }, 0)
    },

    /**
     * Bind to @keydown.enter.exact — without .prevent, because whether to
     * swallow the keystroke is exactly what this decides. Shift+Enter never
     * reaches here (.exact), so it keeps inserting a newline.
     */
    keydown(event) {
      if (composing(event) || justComposed) return false
      event?.preventDefault?.()
      action()
      return true
    },
  }
}

function composing(event) {
  return !!event && (event.isComposing === true || event.keyCode === 229)
}
