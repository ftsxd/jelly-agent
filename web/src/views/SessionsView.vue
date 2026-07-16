<script setup>
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const PAGE = 50 // sessions per page

const sessions = ref([]) // accumulated across loaded pages
const total = ref(0)
const hasMore = ref(false)
const loading = ref(true)
const loadingMore = ref(false)
const error = ref('')

const selected = ref(null) // session id of the open detail
const detail = ref(null)
const detailLoading = ref(false)

const checked = ref([]) // session ids ticked for batch delete
const deleting = ref(false)
const router = useRouter()

// allLoadedChecked: every currently-loaded row is ticked (drives 全选 state and
// the "select all N across pages" affordance).
const allLoadedChecked = computed(() => sessions.value.length > 0 && sessions.value.every((s) => checked.value.includes(s.id)))
const someChecked = computed(() => checked.value.length > 0 && !allLoadedChecked.value)
// More matching rows exist than are loaded/ticked — offer to select them all.
const canSelectAll = computed(() => allLoadedChecked.value && checked.value.length < total.value)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.sessions(PAGE, 0)
    sessions.value = res.sessions
    total.value = res.total ?? res.sessions.length
    hasMore.value = !!res.has_more
    // Drop ticks for sessions that no longer exist after a reload.
    const ids = new Set(sessions.value.map((s) => s.id))
    checked.value = checked.value.filter((id) => ids.has(id))
    if (sessions.value.length && !selected.value) open(sessions.value[0].id)
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function loadMore() {
  if (loadingMore.value || !hasMore.value) return
  loadingMore.value = true
  error.value = ''
  try {
    const res = await api.sessions(PAGE, sessions.value.length)
    const known = new Set(sessions.value.map((s) => s.id))
    sessions.value.push(...res.sessions.filter((s) => !known.has(s.id)))
    total.value = res.total ?? total.value
    hasMore.value = !!res.has_more
  } catch (e) {
    error.value = e.message
  } finally {
    loadingMore.value = false
  }
}

function toggleAll() {
  // Toggle the loaded rows; clears any prior "select all matching" superset too.
  checked.value = allLoadedChecked.value ? [] : sessions.value.map((s) => s.id)
}

async function selectAllMatching() {
  try {
    checked.value = (await api.sessionIds()).ids
  } catch (e) {
    error.value = e.message
  }
}

async function removeChecked() {
  if (!checked.value.length || deleting.value) return
  if (!confirm(`确认删除选中的 ${checked.value.length} 个会话？此操作不可恢复。`)) return
  deleting.value = true
  error.value = ''
  try {
    const ids = [...checked.value]
    await api.deleteSessions(ids)
    if (selected.value && ids.includes(selected.value)) {
      selected.value = null
      detail.value = null
    }
    checked.value = []
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    deleting.value = false
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

async function remove(s) {
  if (!confirm(`确认删除会话「${s.id}」？此操作不可恢复。`)) return
  try {
    await api.deleteSession(s.id)
    if (selected.value === s.id) {
      selected.value = null
      detail.value = null
    }
    checked.value = checked.value.filter((id) => id !== s.id)
    await load()
  } catch (e) {
    error.value = e.message
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
function continueChat(id) { router.push({ path: '/chat', query: { session: id } }) }
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
        <div v-if="sessions.length" class="list-bar">
          <label class="selall" title="全选本页 / 取消">
            <input
              type="checkbox"
              :checked="allLoadedChecked"
              :indeterminate.prop="someChecked"
              @change="toggleAll"
            />
            <span>{{ checked.length ? `已选 ${checked.length}` : '全选' }} / {{ total }}</span>
          </label>
          <button
            v-if="checked.length"
            class="btn btn-danger btn-sm"
            :disabled="deleting"
            @click="removeChecked"
          >
            <span v-if="deleting" class="spinner" /><Icon v-else name="trash" :size="14" /> 删除选中
          </button>
        </div>
        <div v-if="canSelectAll" class="selectall-bar">
          已选本页 {{ checked.length }} 个，
          <button class="link" @click="selectAllMatching">选择全部 {{ total }} 个会话</button>
        </div>
        <div v-if="loading" class="empty"><span class="spinner" /></div>
        <div v-else-if="error && !sessions.length" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
        <div v-else-if="!sessions.length" class="empty">
          <Icon name="sessions" :size="28" />
          <span class="muted">暂无持久化会话</span>
        </div>
        <div
          v-for="s in sessions"
          :key="s.id"
          class="sess"
          :class="{ active: s.id === selected, picked: checked.includes(s.id) }"
          role="button"
          tabindex="0"
          @click="open(s.id)"
          @keydown.enter="open(s.id)"
        >
          <div class="sess-top">
            <input
              class="pick"
              type="checkbox"
              :value="s.id"
              v-model="checked"
              title="选择以批量删除"
              @click.stop
            />
            <span class="mono sess-id">{{ s.id }}</span>
            <button class="del" title="删除会话" @click.stop="remove(s)"><Icon name="trash" :size="14" /></button>
          </div>
          <span class="sess-meta">
            <span class="badge">{{ s.events }} 事件</span>
            <span class="muted time">{{ fmtTime(s.last_update) }}</span>
          </span>
        </div>
        <button v-if="hasMore" class="loadmore" :disabled="loadingMore" @click="loadMore">
          <span v-if="loadingMore" class="spinner" /> 加载更多（还有 {{ total - sessions.length }} 个）
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
            <div class="detail-actions"><span class="badge mono">total {{ detail.usage.total }} tok</span><button class="btn btn-sm" @click="continueChat(detail.id)">继续对话</button></div>
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
.list-bar {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  padding: 2px var(--sp-1) var(--sp-2);
  border-bottom: 1px solid var(--border);
  margin-bottom: var(--sp-1);
}
.selall {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: 12px;
  color: var(--text-dim);
  cursor: pointer;
  user-select: none;
}
.selall input {
  cursor: pointer;
}
.btn-sm {
  padding: 4px 10px;
  font-size: 12px;
}
.btn-danger {
  border-color: var(--danger-border, var(--danger));
  color: var(--danger);
}
.btn-danger:hover {
  background: var(--danger-tint);
}
.pick {
  flex-shrink: 0;
  cursor: pointer;
  margin: 0;
}
.selectall-bar {
  padding: var(--sp-2) var(--sp-3);
  background: var(--accent-tint);
  border-radius: var(--radius-sm);
  font-size: 12px;
  color: var(--text-dim);
  text-align: center;
}
.link {
  background: none;
  border: none;
  padding: 0;
  color: var(--accent);
  cursor: pointer;
  font: inherit;
  text-decoration: underline;
}
.loadmore {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  padding: var(--sp-3);
  margin-top: var(--sp-1);
  border: 1px dashed var(--border);
  border-radius: var(--radius-sm);
  background: transparent;
  color: var(--text-dim);
  font-size: 13px;
  cursor: pointer;
}
.loadmore:hover:not(:disabled) {
  background: var(--surface-2);
  color: var(--text);
}
.loadmore:disabled {
  cursor: default;
  opacity: 0.7;
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
.sess.picked {
  border-color: var(--accent);
}
.sess-top {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.sess-id {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  flex: 1;
  min-width: 0;
}
.del {
  flex-shrink: 0;
  display: grid;
  place-items: center;
  width: 24px;
  height: 24px;
  border: none;
  background: transparent;
  color: var(--text-muted);
  border-radius: var(--radius-sm);
  cursor: pointer;
  opacity: 0;
  transition: opacity 0.15s ease, color 0.15s ease, background 0.15s ease;
}
.sess:hover .del {
  opacity: 1;
}
.del:hover {
  color: var(--danger);
  background: var(--danger-tint);
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
