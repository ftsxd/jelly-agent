<script setup>
import { nextTick, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api, streamChat } from '../api'

const providers = ref([])
const provider = ref('')
const messages = ref([]) // {role, text, tools: [{name,args,response}], usage}
const input = ref('')
const sessionId = ref('')
const busy = ref(false)
const error = ref('')
const scroller = ref(null)
let abort = null

onMounted(async () => {
  try {
    const data = await api.providers()
    providers.value = data.providers
    provider.value = data.default || (data.providers[0]?.name ?? '')
  } catch (e) {
    error.value = e.message
  }
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
}

async function send() {
  const text = input.value.trim()
  if (!text || busy.value) return
  error.value = ''
  input.value = ''
  messages.value.push({ role: 'user', text })
  const agentMsg = { role: 'agent', text: '', tools: [], usage: null }
  messages.value.push(agentMsg)
  busy.value = true
  scrollDown()

  abort = new AbortController()
  try {
    await streamChat(
      { message: text, sessionId: sessionId.value, provider: provider.value },
      (ev) => {
        switch (ev.type) {
          case 'session':
            sessionId.value = ev.session_id
            break
          case 'text_delta':
            agentMsg.text += ev.text
            scrollDown()
            break
          case 'tool_call':
            agentMsg.tools.push({ name: ev.name, args: ev.args, response: null })
            scrollDown()
            break
          case 'tool_result': {
            const t = [...agentMsg.tools].reverse().find((t) => t.name === ev.name && t.response === null)
            if (t) t.response = ev.response
            else agentMsg.tools.push({ name: ev.name, args: null, response: ev.response })
            break
          }
          case 'usage':
            agentMsg.usage = ev
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

function resultSummary(resp) {
  if (!resp) return '执行中…'
  if (Array.isArray(resp.results)) return `${resp.results.length} 条结果`
  const s = JSON.stringify(resp)
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
        <select v-model="provider" class="input select" :disabled="busy" aria-label="选择 Provider">
          <option v-if="!providers.length" value="">（无 Provider）</option>
          <option v-for="p in providers" :key="p.name" :value="p.name">
            {{ p.name }} · {{ p.model }}
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
          <div v-for="(t, ti) in m.tools" :key="ti" class="tool-chip">
            <div class="tool-head">
              <Icon name="tool" :size="13" />
              <span class="mono tool-name">{{ t.name }}</span>
              <span v-if="t.args" class="mono dim tool-args">{{ fmtArgs(t.args) }}</span>
            </div>
            <div class="tool-res mono" :class="{ pending: !t.response }">
              {{ resultSummary(t.response) }}
            </div>
          </div>

          <div v-if="m.text" class="bubble" :class="m.role">{{ m.text }}</div>
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
  border-radius: 8px;
  display: grid;
  place-items: center;
  border: 1px solid var(--border);
}
.avatar.user {
  background: var(--primary-tint);
  color: var(--primary);
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
  background: var(--primary-tint);
  border-color: var(--primary-border);
}
.bubble.agent {
  background: var(--surface);
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
