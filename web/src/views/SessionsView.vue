<script setup>
import { onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const sessions = ref([])
const loading = ref(true)
const error = ref('')

const selected = ref(null) // session id
const detail = ref(null)
const detailLoading = ref(false)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    sessions.value = (await api.sessions()).sessions
    if (sessions.value.length && !selected.value) open(sessions.value[0].id)
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function open(id) {
  selected.value = id
  detailLoading.value = true
  detail.value = null
  try {
    detail.value = await api.session(id)
  } catch (e) {
    error.value = e.message
  } finally {
    detailLoading.value = false
  }
}

function fmtTime(unix) {
  if (!unix) return ''
  return new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false })
}

function fmtArgs(args) {
  if (!args) return ''
  return Object.entries(args)
    .map(([k, v]) => `${k}=${JSON.stringify(v)}`)
    .join(', ')
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <h1>会话</h1>
      <button class="btn" @click="load" :disabled="loading">
        <Icon name="refresh" :size="16" /> 刷新
      </button>
    </header>

    <div class="body">
      <aside class="list">
        <div v-if="loading" class="empty"><span class="spinner" /></div>
        <div v-else-if="error && !sessions.length" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
        <div v-else-if="!sessions.length" class="empty">
          <Icon name="sessions" :size="28" />
          <span class="muted">暂无持久化会话</span>
        </div>
        <button
          v-for="s in sessions"
          :key="s.id"
          class="sess"
          :class="{ active: s.id === selected }"
          @click="open(s.id)"
        >
          <span class="mono sess-id">{{ s.id }}</span>
          <span class="sess-meta">
            <span class="badge">{{ s.events }} 事件</span>
            <span class="muted time">{{ fmtTime(s.last_update) }}</span>
          </span>
        </button>
      </aside>

      <section class="detail">
        <div v-if="detailLoading" class="empty"><span class="spinner" /> 加载会话…</div>
        <div v-else-if="!detail" class="empty">
          <Icon name="doc" :size="28" />
          <span class="muted">选择左侧会话查看记录</span>
        </div>
        <template v-else>
          <div class="detail-head">
            <span class="mono dim">{{ detail.id }}</span>
            <span class="badge mono">total {{ detail.usage.total }} tok</span>
          </div>
          <div class="transcript">
            <div v-if="!detail.events.length" class="empty"><span class="muted">（空会话）</span></div>
            <div v-for="(ev, i) in detail.events" :key="i" class="ev" :class="ev.role">
              <div class="ev-head">
                <Icon :name="ev.role === 'user' ? 'user' : 'bot'" :size="14" />
                <span class="mono author">{{ ev.author }}</span>
              </div>
              <div v-if="ev.text" class="ev-text">{{ ev.text }}</div>
              <div v-for="(tc, ti) in ev.tool_calls" :key="'c' + ti" class="ev-tool">
                <Icon name="tool" :size="13" />
                <span class="mono">{{ tc.name }}({{ fmtArgs(tc.args) }})</span>
              </div>
              <div v-for="(tr, ti) in ev.tool_results" :key="'r' + ti" class="ev-tool result">
                <Icon name="check" :size="13" />
                <span class="mono">{{ tr.name }} →
                  {{ Array.isArray(tr.response?.results) ? tr.response.results.length + ' 条结果' : '已返回' }}</span>
              </div>
            </div>
          </div>
        </template>
      </section>
    </div>
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
  padding: var(--sp-4) var(--sp-5);
  border-bottom: 1px solid var(--border);
}
.topbar h1 {
  font-size: 18px;
}
.body {
  flex: 1;
  display: grid;
  grid-template-columns: 320px 1fr;
  overflow: hidden;
}
.list {
  border-right: 1px solid var(--border);
  overflow-y: auto;
  padding: var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.sess {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  padding: var(--sp-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface);
  text-align: left;
  cursor: pointer;
  font: inherit;
  color: var(--text);
  transition: border-color 0.15s ease, background 0.15s ease;
}
.sess:hover {
  background: var(--surface-2);
}
.sess.active {
  border-color: var(--primary-border);
  background: var(--primary-tint);
}
.sess-id {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sess-meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
}
.time {
  font-size: 11px;
}

.detail {
  overflow-y: auto;
  padding: var(--sp-5);
}
.detail-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding-bottom: var(--sp-3);
  margin-bottom: var(--sp-4);
  border-bottom: 1px solid var(--border);
}
.transcript {
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
  max-width: 760px;
}
.ev {
  border-left: 2px solid var(--border-strong);
  padding-left: var(--sp-3);
}
.ev.user {
  border-left-color: var(--primary);
}
.ev.agent {
  border-left-color: var(--accent);
}
.ev-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  color: var(--text-dim);
  margin-bottom: var(--sp-2);
}
.author {
  font-size: 12px;
}
.ev-text {
  white-space: pre-wrap;
  word-break: break-word;
}
.ev-tool {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: 12px;
  color: var(--text-dim);
  margin-top: var(--sp-2);
}
.ev-tool.result {
  color: var(--accent);
}
.error-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  background: var(--danger-tint);
  color: var(--danger);
  border-radius: var(--radius-sm);
  font-size: 13px;
}
@media (max-width: 820px) {
  .body {
    grid-template-columns: 1fr;
  }
  .list {
    display: none;
  }
}
</style>
