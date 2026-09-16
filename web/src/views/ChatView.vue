<script setup>
import { computed, nextTick, onMounted, onUnmounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import SessionPicker from '../components/SessionPicker.vue'
import ChatTranscript from '../components/ChatTranscript.vue'
import { api, streamChat } from '../api'
import { applyFrame, emptyTimeline, summarize } from '../timeline'
import { replayMessages } from '../replay'
import { latestOnly } from '../latest'
import { sendOnEnter } from '../ime'
import { statusOf, taskOfSession } from '../tasks'

const PROVIDER_KEY = 'jelly.provider' // remembers the last-used provider
const AGENT_KEY = 'jelly.agent' // remembers the last-used agent (multi-agent)
// Remembers which conversation this browser was in. The sidebar links to a
// bare /chat, so leaving the view and coming back drops ?session= and the
// route alone can no longer say what to reopen.
const SESSION_KEY = 'jelly.session'
const POLL_MS = 3000 // how often an open session is re-read while it runs

const providers = ref([])
const historySessions = ref([])
const provider = ref('')
const agents = ref([]) // enabled named agents (multi-agent); empty = single-agent mode
const agentName = ref('') // '' = single agent on the chosen provider
const messages = ref([]) // {role, text, timeline, usage, provider, model}
const input = ref('')
const sessionId = ref('')
const busy = ref(false)
// What the server says the open session is doing, as of the last replay.
// Empty when it says nothing — see the endpoint's note on why "nothing known"
// is not the same as "not running".
const remoteStatus = ref('')
const error = ref('')
const scroller = ref(null)
let abort = null
const route = useRoute()
const router = useRouter()

// Tag agent messages with the model that produced them only when there's a
// choice to make — a single-provider setup needs no per-message label.
const showProviderTag = computed(() => providers.value.length > 1)

// A turn of this session is in flight, whoever started it: this page (busy),
// another tab, or this tab before the user walked away from the view.
const running = computed(() => busy.value || remoteStatus.value === 'running')
// The same run, but driven by somebody else — which is the only case that has
// to be polled for, and the only one worth telling the user about separately.
const remoteRunning = computed(() => !busy.value && remoteStatus.value === 'running')
// Nothing may be sent while a turn of this session is going. Two turns at once
// on one session interleave into one event log.
const locked = computed(() => running.value)

// How the last turn ended, when the server still remembers and nothing is
// running now.
//
// Shown rather than left blank, because "no badge" has to keep meaning "this
// process has no idea" — the state after a restart, or past the outcome TTL.
// A conversation whose timeline simply stops looks identical whether the agent
// is thinking, has finished, or was cut off; saying so is the whole point.
const endedStatus = computed(() => (running.value ? '' : remoteStatus.value))

// A transcript that stops in the middle of a tool call while nothing is
// running.
//
// The registry holds every run this process drives, so a replay with no status
// means nothing is in flight — it is only the *outcome* that is forgotten once
// the TTL passes. A call still waiting for its result under those conditions
// did not finish: it was cut off, by a restart or by the browser hanging up.
// Saying nothing there is what left a half-run looking like a finished one.
const interrupted = computed(() => {
  if (running.value || endedStatus.value) return false
  const last = messages.value[messages.value.length - 1]
  return !!last?.timeline && summarize(last.timeline).pending > 0
})

// Badge colour per status tone, since the tone classes live inside the task
// centre's scoped styles.
const BADGE_TONE = { run: 'badge-primary', ok: 'badge-accent', bad: 'badge-danger', warn: 'badge-amber', muted: '' }
function badgeClass(status) {
  return BADGE_TONE[statusOf(status).tone] ?? ''
}

function modelOf(name) {
  return providers.value.find((p) => p.name === name)?.model ?? ''
}

onMounted(async () => {
  try {
    const data = await api.providers()
    providers.value = data.providers
    // Restore the remembered provider if it still exists, else fall back to the
    // configured default (or the first provider).
    const saved = localStorage.getItem(PROVIDER_KEY)
    const valid = (n) => data.providers.some((p) => p.name === n)
    provider.value = (saved && valid(saved) && saved) || data.default || (data.providers[0]?.name ?? '')
  } catch (e) {
    error.value = e.message
  }
  try {
    const data = await api.agents()
    agents.value = (data.agents || []).filter((a) => a.enabled)
    if (agents.value.length) {
      const saved = localStorage.getItem(AGENT_KEY)
      const valid = (n) => agents.value.some((a) => a.name === n)
      // Default to the saved/configured-default agent so multi-agent is used out
      // of the box once defined; '' falls back to single-agent mode.
      agentName.value = (saved && valid(saved) && saved) || (valid(data.default_agent) && data.default_agent) || agents.value[0].name
    }
  } catch {
    /* agents are optional; ignore when unavailable */
  }
  await loadHistorySessions()
  await restoreSession()
})

onUnmounted(stopPolling)

// Which conversation to open on arrival: the one the route names, else the one
// this browser was last in.
//
// The route is authoritative when it has a session — that is a deep link or a
// continue-from-task, and it opens even if the history list has not heard of
// it. The remembered id is only honoured when the list still has it: a session
// that was deleted, or one left over from another server, must not greet the
// user with an error banner instead of a composer.
async function restoreSession() {
  if (typeof route.query.session === 'string' && route.query.session) {
    await openHistorySession(route.query.session)
    return
  }
  const saved = localStorage.getItem(SESSION_KEY) || ''
  if (!saved || !historySessions.value.some((s) => s.id === saved)) return
  await openHistorySession(saved)
  // The URL is made to match, so a reload of the restored page stays put.
  if (sessionId.value) router.replace({ query: { ...route.query, session: sessionId.value } })
}

// The task this conversation is continuing, if the user came from one.
//
// Held as what the route said; what actually goes out is taskOfSession of it,
// so an attachment that belongs to another conversation is simply not sent
// rather than sent and refused. The ref outlives a session switch — the
// attachment must not.
const continuingTask = ref(typeof route.query.task === 'string' ? route.query.task : '')
watch(() => route.query.task, (id) => {
  continuingTask.value = typeof id === 'string' ? id : ''
})
const activeTask = computed(() => taskOfSession(continuingTask.value, sessionId.value))

watch(() => route.query.session, async (id) => {
  if (typeof id === 'string' && id && id !== sessionId.value) await openHistorySession(id)
})

async function loadHistorySessions() {
  try { historySessions.value = (await api.sessions(100, 0)).sessions || [] } catch { /* history is optional */ }
}

// Replay runs through the same reducer as the live stream.
//
// The old version rebuilt messages from the transcript DTO, which lists a
// turn's calls and its results as two separate arrays — so the pairing was
// already gone by the time it arrived, and every historical tool rendered as
// a pending call next to an orphan result.
// Only the newest pick wins; see latest.js for why a `busy` guard did not.
const openGate = latestOnly()

async function openHistorySession(id) {
  if (!id) return
  error.value = ''
  const r = await openGate.run(async (signal) => {
    const mine = openGate.current()
    const detail = await api.sessionTimeline(id, signal)
    if (!openGate.owns(mine)) return null
    sessionId.value = detail.id
    messages.value = replayMessages(detail.frames || [])
    remoteStatus.value = detail.status || ''
    await scrollDown()
    return detail
  })
  if (r.owned && r.error) error.value = r.error.message
}

// refreshOpen re-reads the open session in place.
//
// Same endpoint, same reducer, same ownership gate as opening it — a poll that
// lands after the user has switched sessions must not paint one conversation's
// frames under another's id. The scroll is only followed when the user was
// already at the bottom; yanking them back down every three seconds while they
// read the middle of a long run is worse than not following at all.
async function refreshOpen() {
  const id = sessionId.value
  if (!id || busy.value) return
  // The result is deliberately not inspected. A failed poll is worth neither a
  // banner nor a stop: the next tick may well succeed, whereas an error bar
  // appearing on its own while nobody is touching the page reads as the run
  // having failed — which is the one thing a failed poll cannot tell you.
  await openGate.run(async (signal) => {
    const mine = openGate.current()
    const detail = await api.sessionTimeline(id, signal)
    if (!openGate.owns(mine) || sessionId.value !== id) return null
    const follow = atBottom()
    messages.value = replayMessages(detail.frames || [])
    remoteStatus.value = detail.status || ''
    if (follow) await scrollDown()
    return detail
  })
}

// Polling, only while a run this page is not driving is still going.
//
// The server's only push channel is the chat stream, and that is a POST that
// runs a turn — a page cannot subscribe to a run somebody else started, not
// even its own from before it was unmounted. Polling is what the task centre
// already does for the same reason. An idle conversation sends nothing.
let poller = null
function stopPolling() {
  if (poller) clearInterval(poller)
  poller = null
}
function retime() {
  stopPolling()
  if (!remoteRunning.value) return
  poller = setInterval(refreshOpen, POLL_MS)
}
watch(remoteRunning, retime)

// The open conversation is remembered so that leaving the view and coming
// back reopens it. Cleared with the view: an empty id means a new chat, and
// restoring the previous one on top of that would undo the button.
watch(sessionId, (id) => {
  if (id) localStorage.setItem(SESSION_KEY, id)
  else localStorage.removeItem(SESSION_KEY)
})

watch(provider, (name) => {
  if (name) localStorage.setItem(PROVIDER_KEY, name)
})
watch(agentName, (name) => {
  localStorage.setItem(AGENT_KEY, name || '')
})

async function scrollDown() {
  await nextTick()
  if (scroller.value) scroller.value.scrollTop = scroller.value.scrollHeight
}

// Near enough to the bottom that the user is following the run rather than
// reading back through it.
function atBottom() {
  const el = scroller.value
  if (!el) return true
  return el.scrollHeight - el.scrollTop - el.clientHeight < 80
}

// Enter sends — unless it belongs to the input method. Typing Chinese, Enter
// picks the highlighted candidate, and sending then posts the raw pinyin.
const enter = sendOnEnter(() => send())

function newChat() {
  if (busy.value) return
  // The gate is abandoned, not just ignored: a replay still in flight would
  // otherwise land after this and refill the view with the session the user
  // just left. Polling stops for the same reason.
  openGate.abandon()
  stopPolling()
  messages.value = []
  sessionId.value = ''
  remoteStatus.value = ''
  continuingTask.value = ''
  error.value = ''
  router.replace({ query: {} })
}

async function send() {
  const text = input.value.trim()
  if (!text || locked.value) return
  error.value = ''
  // This page is driving the turn now, so what a replay said about the
  // previous one stops being the answer.
  remoteStatus.value = ''
  input.value = ''
  messages.value.push({ role: 'user', text })
  const agentMsg = {
    role: 'agent',
    text: '',
    timeline: emptyTimeline(),
    usage: null,
    provider: provider.value,
    model: modelOf(provider.value),
    author: '', // which (sub-)agent produced the reply, for multi-agent
  }
  messages.value.push(agentMsg)
  // Vue 3 only tracks mutations made through the reactive proxy. The literal we
  // pushed is still the raw object, so streaming deltas into `agentMsg` directly
  // wouldn't trigger re-renders and the answer would appear all at once. Grab the
  // proxied element and mutate that instead so each delta paints incrementally.
  const live = messages.value[messages.value.length - 1]
  busy.value = true
  scrollDown()

  abort = new AbortController()
  try {
    await streamChat(
      {
        message: text, sessionId: sessionId.value, provider: provider.value,
        agent: agentName.value,
        // Set when the user arrived from a task and is continuing it. Cleared
        // after the turn: the next question is a new goal unless they say so,
        // and silently attaching everything after would let one task swallow
        // the rest of the conversation.
        taskId: activeTask.value,
      },
      (ev) => handleFrame(live, ev),
      abort.signal,
    )
  } catch (e) {
    if (e.name === 'AbortError') remoteStatus.value = 'cancelled'
    else {
      error.value = e.message
      remoteStatus.value = 'failed'
    }
  } finally {
    // Cleared however the turn ended. It is an attachment the user made once,
    // by arriving from a task; carrying it into the next question would let
    // one task swallow the rest of the conversation, and carrying it past a
    // failure would repeat the failure.
    continuingTask.value = ''
    busy.value = false
    abort = null
    scrollDown()
  }
}

// Stopping has two halves, and a reopened page only has the second one. The
// abort ends this browser's stream; the request ends the run, which is driven
// by whichever request started it — not necessarily this one.
async function stop() {
  if (abort) abort.abort()
  if (!sessionId.value) return
  try { await api.stopSession(sessionId.value) }
  catch (e) { error.value = e.message }
}

// handleFrame folds one frame into a message, plus the few things that are
// this view's own business rather than the timeline's: the resolved session id
// and the error banner.
//
// Everything about *what happened* goes to the reducer. Nothing here decides
// how a call pairs with its result or whether a tool succeeded — the same view
// used to do both, and got both wrong.
function handleFrame(live, ev) {
  switch (ev.type) {
    case 'session':
      sessionId.value = ev.session_id
      router.replace({ query: { session: ev.session_id } })
      loadHistorySessions()
      break
    case 'error':
      error.value = ev.message
      // The stream said so, so the status line can stop guessing. Without it a
      // turn that failed mid-way left the same silent transcript as one that
      // finished, with only the banner to tell them apart — and the banner is
      // at the other end of a long page.
      remoteStatus.value = 'failed'
      break
    case 'done':
      remoteStatus.value = 'completed'
      break
    default:
      break
  }
  applyFrame(live.timeline, ev)
  if (ev.type === 'done') live.usage = live.timeline.usage
  if (ev.type === 'text_delta' || ev.type === 'tool_call') scrollDown()
}

</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>对话</h1>
        <span v-if="sessionId" class="badge mono">{{ sessionId }}</span>
        <!-- Running is a fact about the server, not about this page: it shows
             for a run started here, in another tab, or by this tab before the
             user walked away from the view. -->
        <span v-if="running" class="badge badge-primary">
          <span class="spinner sm" /> {{ statusOf('running').label }}
        </span>
        <span v-else-if="endedStatus" class="badge" :class="badgeClass(endedStatus)">
          <Icon :name="statusOf(endedStatus).icon" :size="12" /> {{ statusOf(endedStatus).label }}
        </span>
      </div>
      <div class="topbar-r">
        <SessionPicker
          v-model="sessionId"
          :sessions="historySessions"
          :disabled="busy"
          @pick="openHistorySession"
        />
        <select v-if="agents.length" v-model="agentName" class="input select" :disabled="busy" aria-label="选择 Agent">
          <option value="">单 Agent（默认）</option>
          <option v-for="a in agents" :key="a.name" :value="a.name">
            🤖 {{ a.name }}{{ (a.sub_agents || []).length ? ` · ${a.sub_agents.length} 子` : '' }}
          </option>
        </select>
        <select v-model="provider" class="input select" :disabled="busy || !!agentName" :title="agentName ? 'Agent 自带 Provider，此选择仅用于单 Agent 模式' : ''" aria-label="选择 Provider">
          <option v-if="!providers.length" value="">（无 Provider）</option>
          <option v-for="p in providers" :key="p.name" :value="p.name">
            {{ p.name }} · {{ p.model }}{{ p.is_default ? ' · 默认' : '' }}
          </option>
        </select>
        <button class="btn" @click="newChat" :disabled="busy">
          <Icon name="spark" :size="16" /> 新对话
        </button>
      </div>
    </header>

    <div ref="scroller" class="stream">
      <div v-if="!messages.length" class="empty">
        <Icon name="chat" :size="32" />
        <div v-if="providers.length">
          <p style="margin: 0 0 4px">开始与 jelly-agent 对话</p>
          <p class="muted" style="margin: 0; font-size: 13px">
            流式响应、工具调用可视化、逐轮 Token 统计
          </p>
        </div>
        <div v-else>
          <p style="margin: 0 0 4px">尚未配置 Provider</p>
          <p class="muted" style="margin: 0; font-size: 13px">
            前往 <RouterLink to="/config">配置</RouterLink> 页新建一个 OpenAI 兼容端点即可开始对话
          </p>
        </div>
      </div>

      <ChatTranscript :messages="messages" :show-provider="showProviderTag" :pending="locked" />

      <!-- Below the transcript, not above it. A run is watched from the bottom
           of a long page, and a notice at the top is a notice nobody sees.
           It also covers the gap the timeline cannot: between a finished tool
           and the next call there is no pending step to spin, so a model that
           is thinking renders as a row of ticks and reads as done. -->
      <div v-if="running" class="livebar">
        <span class="spinner sm" />
        <span>{{ remoteRunning ? '仍在后台运行，本页每 3 秒自动刷新' : '正在运行…' }}</span>
        <!-- Only when this page is not the one streaming: the composer already
             carries a stop button for that case, and it sits where the hand
             already is. Here there is nothing else to click. -->
        <button v-if="remoteRunning" class="btn livebar-stop" @click="stop">停止本轮</button>
      </div>
      <div v-else-if="endedStatus" class="livebar done">
        <Icon :name="statusOf(endedStatus).icon" :size="12" />
        <span>本轮{{ statusOf(endedStatus).label }}</span>
      </div>
      <div v-else-if="interrupted" class="livebar warn">
        <Icon name="alert" :size="12" />
        <span>本轮未正常结束：有工具调用没有回结果，且当前没有运行在进行</span>
      </div>
    </div>

    <div v-if="error" class="error-bar">
      <Icon name="alert" :size="16" /> {{ error }}
    </div>

    <footer class="composer">
      <textarea
        v-model="input"
        class="textarea"
        rows="1"
        @keydown.enter.exact="enter.keydown"
        @compositionend="enter.compositionend"
        :disabled="locked"
        :placeholder="remoteRunning ? '本轮运行尚未结束，结束后可继续输入' : '输入消息，Enter 发送，Shift+Enter 换行'"
      />
      <button v-if="busy" class="btn btn-icon" @click="stop" title="停止">
        <span class="stop-square" />
      </button>
      <button v-else class="btn btn-primary btn-icon" @click="send" :disabled="!input.trim() || locked" title="发送">
        <Icon name="send" :size="16" />
      </button>
    </footer>
  </div>
</template>

<style scoped>
.view {
  display: flex;
  flex-direction: column;
  height: 100%;
}
.topbar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  padding: var(--sp-4) var(--sp-5);
  border-bottom: 1px solid var(--border);
}
.topbar-l {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}
.topbar-l h1 {
  font-size: 18px;
}
.topbar-r {
  display: flex;
  gap: var(--sp-2);
}
.select {
  width: auto;
  height: 36px;
}

.stream {
  flex: 1;
  overflow-y: auto;
  padding: var(--sp-5);
  display: flex;
  flex-direction: column;
  gap: var(--sp-5);
}

/* The shared spinner is sized for a bubble; inside a badge it has to match
   the text next to it. */
.spinner.sm {
  width: 10px;
  height: 10px;
  border-width: 1.5px;
}

/* The run's own status line, under the transcript it belongs to. */
.livebar {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  max-width: 820px;
  width: 100%;
  margin: 0 auto;
  font-size: 12px;
  color: var(--primary);
}
/* Sized down to sit inside a 12px status line without becoming the loudest
   thing on the page — it is an escape hatch, not the main action. */
.livebar-stop {
  padding: 2px 10px;
  min-height: 0;
  font-size: 12px;
}

.livebar.done {
  color: var(--text-muted);
}
.livebar.warn {
  color: var(--warning);
}

.error-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  margin: 0 var(--sp-5);
  padding: var(--sp-2) var(--sp-3);
  background: var(--danger-tint);
  color: var(--danger);
  border-radius: var(--radius-sm);
  font-size: 13px;
}

.composer {
  display: flex;
  gap: var(--sp-2);
  align-items: flex-end;
  padding: var(--sp-4) var(--sp-5);
  border-top: 1px solid var(--border);
  background: var(--surface-glass);
  -webkit-backdrop-filter: blur(14px) saturate(1.3);
  backdrop-filter: blur(14px) saturate(1.3);
  box-shadow: 0 -8px 24px rgba(0, 0, 0, 0.2);
}
.composer .textarea {
  max-height: 180px;
}
.stop-square {
  width: 12px;
  height: 12px;
  border-radius: 2px;
  background: var(--danger);
}
</style>
