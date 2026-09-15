<script setup>
/*
  One conversation, rendered once.
  
  The chat view and the session browser both show a stored run, and they used
  to draw it two different ways: the chat folded frames into paired steps,
  while the browser printed the raw event DTO — a turn's calls in one block and
  their results in the next, pairing already lost before it reached the page.
  Same session, two answers to "what did the agent do", and only one of them
  matched what the user had watched live. So the markup lives here and both
  views mount it.
*/
import AgentTimeline from './AgentTimeline.vue'
import Icon from './Icon.vue'
import { renderMarkdown } from '../markdown'
import { finalAnswer } from '../timeline'

const props = defineProps({
  messages: { type: Array, required: true },
  // Tag agent bubbles with the model that produced them. Only worth the room
  // when there is a choice to make — a single-provider setup needs no label.
  showProvider: { type: Boolean, default: false },
  // A turn is in flight, so the last agent bubble may still be empty and
  // should say it is thinking rather than render as an empty answer.
  pending: { type: Boolean, default: false },
})

// The reply bubble is the last root-level text the reducer folded. Sub-agent
// prose stays in the timeline: merging it into the answer is what made a
// handoff produce one bubble attributed to whichever agent spoke last.
function answerOf(m) {
  if (m.role !== 'agent') return m.text || ''
  const step = m.timeline ? finalAnswer(m.timeline) : null
  return step ? step.text : m.text || ''
}

function authorOf(m) {
  if (!m.timeline) return m.author || ''
  const step = finalAnswer(m.timeline)
  return step ? step.agent : m.author || ''
}

function isLast(i) {
  return i === props.messages.length - 1
}
</script>

<template>
  <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
    <div class="avatar" :class="m.role">
      <Icon :name="m.role === 'user' ? 'user' : 'bot'" :size="16" />
    </div>
    <div class="bubble-wrap">
      <div v-if="m.role === 'agent' && authorOf(m) && authorOf(m) !== 'root'" class="who mono dim">
        <Icon name="bot" :size="12" /> {{ authorOf(m) }}
      </div>
      <div v-else-if="m.role === 'agent' && m.provider && showProvider" class="who mono dim">
        <Icon name="bot" :size="12" /> {{ m.provider }}<span v-if="m.model"> · {{ m.model }}</span>
      </div>
      <AgentTimeline v-if="m.timeline" :timeline="m.timeline" class="msg-tl" />

      <!-- Agent replies are markdown; a user's own message is not. Sending
           user input through the renderer would let someone paste markup
           into their own transcript, and there is nothing to gain from
           formatting what they just typed. -->
      <div v-if="answerOf(m) && m.role === 'agent'" class="bubble agent md" v-html="renderMarkdown(answerOf(m))"></div>
      <div v-else-if="m.text" class="bubble" :class="m.role">{{ m.text }}</div>
      <div v-else-if="m.role === 'agent' && pending && isLast(i)" class="bubble agent typing">
        <span class="spinner" />
        <span class="muted">思考中…</span>
      </div>

      <div v-if="m.usage" class="usage mono">
        prompt {{ m.usage.prompt }} · completion {{ m.usage.completion }} · total
        {{ m.usage.total }}
      </div>
    </div>
  </div>
</template>

<style scoped>
.msg {
  display: flex;
  gap: var(--sp-3);
  max-width: 820px;
  width: 100%;
  margin: 0 auto;
}
.avatar {
  flex-shrink: 0;
  width: 30px;
  height: 30px;
  border-radius: var(--radius-sm);
  display: grid;
  place-items: center;
  border: 1px solid var(--border);
}
.avatar.user {
  background: var(--primary);
  border-color: transparent;
  color: #fff;
}
.avatar.agent {
  background: var(--accent-tint);
  color: var(--accent);
}
.bubble-wrap {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  min-width: 0;
  flex: 1;
}
.bubble {
  padding: var(--sp-3) var(--sp-4);
  border-radius: var(--radius);
  /* Newlines are preserved for the plain-text bubbles — a user's own message,
     and an agent's before it is rendered. Not for a rendered one: markdown
     output already carries its own block elements, and pre-wrap turns the
     newlines *between* those tags into real blank lines. That is what made the
     chat's copy of a reply twice as tall as the task centre's copy of the same
     text, with the table's header floating off from its body. */
  white-space: pre-wrap;
  word-break: break-word;
  border: 1px solid var(--border);
}
.bubble.md {
  white-space: normal;
}
.bubble.user {
  /* A flat tint with the accent as its edge. The gradient fill plus gradient
     border needed a dark surround to read as one shape; on white the two
     gradients fought each other and the text sat on a moving ground. */
  background: var(--primary-tint);
  border-color: var(--primary-border);
}

/* Rendered markdown lives in style.css under .md, shared with the task
   centre. See the note there. */

.bubble.agent {
  background: var(--surface-2);
  border-left: 3px solid var(--accent);
}
.bubble.typing {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

/* The timeline sits above the reply, close enough to read as part of the same
   answer rather than as a separate panel. */
.msg-tl {
  margin-bottom: var(--sp-2);
}

.who {
  display: flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--text-dim);
  padding-left: var(--sp-1);
}

.usage {
  font-size: 11px;
  color: var(--text-muted);
  padding-left: var(--sp-1);
}
</style>
