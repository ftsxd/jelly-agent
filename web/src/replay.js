/**
 * Stored frames → the alternating user/agent bubbles a conversation is.
 *
 * Lives here rather than inside a view for the reason timeline.js and
 * latest.js do: this project has no component test harness, so anything
 * written inside a .vue file is untested by construction — and this one is
 * shared by two views, which is exactly how the chat and the session browser
 * came to render the same stored run two different ways.
 */
import { applyFrame, emptyTimeline } from './timeline'

function newTurn() {
  return {
    role: 'agent', text: '', timeline: emptyTimeline(),
    usage: null, provider: '', model: '', author: '',
  }
}

/**
 * replayMessages splits a session's frames into bubbles.
 *
 * A user_message frame starts a new pair. Everything until the next one
 * belongs to the agent's answer, which is exactly the grouping the live path
 * produces one request at a time — so a stored run and a running one fold
 * through the same reducer into the same shape.
 */
export function replayMessages(frames) {
  const out = []
  let live = null
  for (const fr of frames || []) {
    if (fr.type === 'user_message') {
      out.push({ role: 'user', text: fr.text || '' })
      live = newTurn()
      out.push(live)
      continue
    }
    if (!live) {
      // Frames before any user message — a session that starts mid-run.
      live = newTurn()
      out.push(live)
    }
    applyFrame(live.timeline, fr)
    live.usage = live.timeline.usage
  }
  return out
}
