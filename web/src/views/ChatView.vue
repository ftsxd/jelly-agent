<script setup>
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import SessionPicker from '../components/SessionPicker.vue'
import { api, streamChat } from '../api'
import { renderMarkdown } from '../markdown'

const PROVIDER_KEY = 'jelly.provider' // remembers the last-used provider
const AGENT_KEY = 'jelly.agent' // remembers the last-used agent (multi-agent)

const providers = ref([])
const historySessions = ref([])
const provider = ref('')
const agents = ref([]) // enabled named agents (multi-agent); empty = single-agent mode
const agentName = ref('') // '' = single agent on the chosen provider
const messages = ref([]) // {role, text, tools: [{name,args,response}], usage, provider, model}
const input = ref('')
const sessionId = ref('')
const busy = ref(false)
const error = ref('')
const scroller = ref(null)
let abort = null
const route = useRoute()
const router = useRouter()

// Tag agent messages with the model that produced them only when there's a
// choice to make — a single-provider setup needs no per-message label.
const showProviderTag = computed(() => providers.value.length > 1)

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
  if (typeof route.query.session === 'string') await openHistorySession(route.query.session)
})

watch(() => route.query.session, async (id) => {
  if (typeof id === 'string' && id && id !== sessionId.value) await openHistorySession(id)
})

async function loadHistorySessions() {
  try { historySessions.value = (await api.sessions(100, 0)).sessions || [] } catch { /* history is optional */ }
}

async function openHistorySession(id) {
  if (!id || busy.value) return
  error.value = ''
  try {
    const detail = await api.session(id)
    sessionId.value = detail.id
    messages.value = (detail.events || []).filter((ev) => ev.text || ev.tool_calls?.length || ev.tool_results?.length).map((ev) => ({
      role: ev.role,
      text: ev.text || '',
      tools: [
        ...(ev.tool_calls || []).map((t) => ({ name: t.name, args: t.args, response: null, ok: null, error: '' })),
        ...(ev.tool_results || []).map((t) => ({ name: t.name, args: null, response: t.response, ok: t.ok, error: t.error || '' })),
      ],
      usage: null,
      provider: '', model: '', author: ev.author || '',
    }))
    await scrollDown()
  } catch (e) { error.value = e.message }
}

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

function newChat() {
  if (busy.value) return
  messages.value = []
  sessionId.value = ''
  error.value = ''
  router.replace({ query: {} })
}

async function send() {
  const text = input.value.trim()
  if (!text || busy.value) return
  error.value = ''
  input.value = ''
  messages.value.push({ role: 'user', text })
  const agentMsg = {
    role: 'agent',
    text: '',
    tools: [],
    usage: null,
    provider: provider.value,
    model: modelOf(provider.value),
    author: '', // which (sub-)agent produced the latest text, for multi-agent
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
      { message: text, sessionId: sessionId.value, provider: provider.value, agent: agentName.value },
      (ev) => {
        switch (ev.type) {
          case 'session':
            sessionId.value = ev.session_id
            router.replace({ query: { session: ev.session_id } })
            loadHistorySessions()
            break
          case 'text_delta':
            live.text += ev.text
            if (ev.agent) live.author = ev.agent
            scrollDown()
            break
          case 'tool_call':
            live.tools.push({ name: ev.name, args: ev.args, response: null, ok: null, error: '' })
            scrollDown()
            break
          case 'tool_result': {
            const t = [...live.tools].reverse().find((t) => t.name === ev.name && t.response === null)
            if (t) Object.assign(t, { response: ev.response, ok: ev.ok, error: ev.error || '' })
            else live.tools.push({ name: ev.name, args: null, response: ev.response, ok: ev.ok, error: ev.error || '' })
            break
          }
          case 'usage':
            live.usage = ev
            break
          case 'error':
            error.value = ev.message
            break
        }
      },
      abort.signal,
    )
  } catch (e) {
    if (e.name !== 'AbortError') error.value = e.message
  } finally {
    busy.value = false
    abort = null
    scrollDown()
  }
}

function stop() {
  if (abort) abort.abort()
}

function fmtArgs(args) {
  if (!args) return ''
  return Object.entries(args)
    .map(([k, v]) => `${k}=${JSON.stringify(v)}`)
    .join(', ')
}

// A tool that failed still comes back as a normal response, so success is read
// from the server's `ok` flag, never from the payload having arrived.
function resultSummary(t) {
  if (!t.response) return '执行中…'
  if (t.error) return t.error.length > 200 ? t.error.slice(0, 200) + '…' : t.error
  if (Array.isArray(t.response.results)) return `${t.response.results.length} 条结果`
  const s = JSON.stringify(t.response)
  return s.length > 120 ? s.slice(0, 120) + '…' : s
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>对话</h1>
        <span v-if="sessionId" class="badge mono">{{ sessionId }}</span>
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

      <div v-for="(m, i) in messages" :key="i" class="msg" :class="m.role">
        <div class="avatar" :class="m.role">
          <Icon :name="m.role === 'user' ? 'user' : 'bot'" :size="16" />
        </div>
        <div class="bubble-wrap">
          <div v-if="m.role === 'agent' && m.author && m.author !== 'root'" class="who mono dim">
            <Icon name="bot" :size="12" /> {{ m.author }}
          </div>
          <div v-else-if="m.role === 'agent' && m.provider && showProviderTag" class="who mono dim">
            <Icon name="bot" :size="12" /> {{ m.provider }}<span v-if="m.model"> · {{ m.model }}</span>
          </div>
          <div v-for="(t, ti) in m.tools" :key="ti" class="tool-chip">
            <div class="tool-head">
              <Icon name="tool" :size="13" />
              <span class="mono tool-name">{{ t.name }}</span>
              <span v-if="t.args" class="mono dim tool-args">{{ fmtArgs(t.args) }}</span>
            </div>
            <div class="tool-res mono" :class="{ pending: !t.response, failed: t.ok === false }">
              {{ resultSummary(t) }}
            </div>
          </div>

          <!-- Agent replies are markdown; a user's own message is not. Sending
               user input through the renderer would let someone paste markup
               into their own transcript, and there is nothing to gain from
               formatting what they just typed. -->
          <div v-if="m.text && m.role === 'agent'" class="bubble agent md" v-html="renderMarkdown(m.text)"></div>
          <div v-else-if="m.text" class="bubble" :class="m.role">{{ m.text }}</div>
          <div v-else-if="m.role === 'agent' && busy" class="bubble agent typing">
            <span class="spinner" />
            <span class="muted">思考中…</span>
          </div>

          <div v-if="m.usage" class="usage mono">
            prompt {{ m.usage.prompt }} · completion {{ m.usage.completion }} · total
            {{ m.usage.total }}
          </div>
        </div>
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
        placeholder="输入消息，Enter 发送，Shift+Enter 换行"
        @keydown.enter.exact.prevent="send"
        :disabled="busy"
      />
      <button v-if="busy" class="btn btn-icon" @click="stop" title="停止">
        <span class="stop-square" />
      </button>
      <button v-else class="btn btn-primary btn-icon" @click="send" :disabled="!input.trim()" title="发送">
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
  white-space: pre-wrap;
  word-break: break-word;
  border: 1px solid var(--border);
}
.bubble.user {
  /* A flat tint with the accent as its edge. The gradient fill plus gradient
     border needed a dark surround to read as one shape; on white the two
     gradients fought each other and the text sat on a moving ground. */
  background: var(--primary-tint);
  border-color: var(--primary-border);
}

/* Rendered markdown. Deep selectors because the HTML is injected by v-html and
   never sees scoped-style attributes. */
.bubble.md :deep(p) {
  margin: 0 0 0.6em;
}
.bubble.md :deep(p:last-child) {
  margin-bottom: 0;
}
/* Lists keep their marker inside the bubble: the default padding-left of 40px
   pushes markers past the bubble's own padding and they hang in the gutter. */
.bubble.md :deep(ul),
.bubble.md :deep(ol) {
  margin: 0.4em 0 0.6em;
  padding-left: 1.4em;
}
.bubble.md :deep(li) {
  margin: 0.2em 0;
}
.bubble.md :deep(li)::marker {
  color: var(--text-muted);
}
.bubble.md :deep(strong) {
  font-weight: 600;
  color: var(--text);
}
.bubble.md :deep(code) {
  font-family: var(--font-mono);
  font-size: 0.88em;
  padding: 0.12em 0.36em;
  border-radius: 4px;
  background: var(--surface-3);
  border: 1px solid var(--hairline);
}
.bubble.md :deep(pre) {
  margin: 0.5em 0;
  padding: var(--sp-3);
  border-radius: var(--radius-sm);
  background: var(--surface-3);
  border: 1px solid var(--hairline);
  /* A wide code block scrolls inside itself rather than widening the bubble
     and, through it, the whole conversation column. */
  overflow-x: auto;
}
.bubble.md :deep(pre code) {
  padding: 0;
  border: 0;
  background: none;
  font-size: 12.5px;
  line-height: 1.6;
}
.bubble.md :deep(h1),
.bubble.md :deep(h2),
.bubble.md :deep(h3),
.bubble.md :deep(h4) {
  /* A reply is already inside a bubble under a heading of its own, so the
     model's "#" levels are rendered as emphasis rather than as page headings —
     browser defaults would put 2em type in a chat line. */
  margin: 0.7em 0 0.35em;
  font-size: 1em;
  font-weight: 600;
}
.bubble.md :deep(h1:first-child),
.bubble.md :deep(h2:first-child),
.bubble.md :deep(h3:first-child) {
  margin-top: 0;
}
.bubble.md :deep(blockquote) {
  margin: 0.5em 0;
  padding: 0.1em 0 0.1em var(--sp-3);
  border-left: 2px solid var(--border);
  color: var(--text-dim);
}
.bubble.md :deep(a) {
  color: var(--primary);
  text-decoration: underline;
  text-underline-offset: 2px;
}
.bubble.md :deep(hr) {
  margin: 0.8em 0;
  border: 0;
  border-top: 1px solid var(--border);
}
.bubble.md :deep(table) {
  margin: 0.5em 0;
  border-collapse: collapse;
  font-size: 13px;
  display: block;
  overflow-x: auto;
}
.bubble.md :deep(th),
.bubble.md :deep(td) {
  padding: 6px 10px;
  border: 1px solid var(--border);
  text-align: left;
}
.bubble.md :deep(th) {
  background: var(--surface-3);
  font-weight: 600;
}

.bubble.agent {
  background: var(--surface-2);
  border-left: 3px solid var(--accent);
}
.bubble.typing {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.tool-chip {
  border: 1px solid var(--border);
  border-left: 2px solid var(--accent);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  overflow: hidden;
}
.tool-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  color: var(--accent);
}
.tool-name {
  font-size: 13px;
  font-weight: 600;
}
.tool-args {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tool-res {
  padding: var(--sp-2) var(--sp-3);
  font-size: 12px;
  color: var(--text-dim);
  border-top: 1px solid var(--border);
  word-break: break-word;
}
.tool-res.pending {
  color: var(--text-muted);
}
.tool-res.failed {
  color: var(--danger);
  background: var(--danger-tint);
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
