/**
 * Only the most recent call's result is delivered.
 *
 * Every list-then-open view has the same race: the user picks A, picks B
 * before A answers, and A's slower response renders under B. It is worse than
 * a slow page because nothing about the result looks wrong — the transcript
 * shown simply belongs to a different session than the one selected.
 *
 * Guarding on some other flag does not work. ChatView tried `busy`, which
 * means "a chat turn is streaming" and is false for the whole of a history
 * load, so the guard never fired. Ownership needs its own answer, which is
 * what the sequence number is.
 *
 * This lives in a .js file rather than inside the component because that is
 * the only way it can be tested — there is no component test harness here, and
 * a race guard nobody can test is a race guard that quietly stops working.
 */
export function latestOnly() {
  let seq = 0
  let pending = null

  return {
    /**
     * Runs `fn(signal)` and resolves only if no newer call started meanwhile.
     *
     * Resolves to `{ owned: false }` when superseded, so the caller can return
     * without touching shared state — including its error state, which an
     * abandoned request must not overwrite either.
     */
    async run(fn) {
      const mine = ++seq
      if (pending) pending.abort()
      const ac = typeof AbortController === 'function' ? new AbortController() : null
      pending = ac

      try {
        const value = await fn(ac ? ac.signal : undefined)
        // Checked after every await, not just the first: a caller may await
        // again (scrolling, a second fetch) and be superseded in between.
        return seq === mine ? { owned: true, value } : { owned: false }
      } catch (error) {
        if (seq !== mine || isAbort(error)) return { owned: false }
        return { owned: true, error }
      }
    },

    /** True while `mine` is still the newest call. */
    owns(mine) {
      return mine === seq
    },

    /** The sequence number a caller can hold across its own awaits. */
    current() {
      return seq
    },
  }
}

function isAbort(e) {
  return e && (e.name === 'AbortError' || e.code === 20)
}
