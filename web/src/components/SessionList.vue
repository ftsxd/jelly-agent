<script setup>
import { computed, onMounted, ref, watch } from 'vue'
import Icon from './Icon.vue'
import { api } from '../api'
import { absTime, relTime } from '../time'

// The conversation list for the chat page: paging, a filter over what is
// loaded, and single/batch delete. It only reports picks and deletions — the
// chat page owns opening a session, so there is one way a session gets shown.
const props = defineProps({ selected: { type: String, default: '' }, disabled: Boolean, refreshKey: { type: Number, default: 0 } })
const emit = defineEmits(['open', 'deleted'])

const PAGE = 50 // sessions per page

const sessions = ref([]) // accumulated across loaded pages
const total = ref(0)
const hasMore = ref(false)
const loading = ref(true)
const loadingMore = ref(false)
const error = ref('')

const q = ref('') // filter over the loaded rows
const checked = ref([]) // session ids ticked for batch delete
const deleting = ref(false)

const filtering = computed(() => q.value.trim() !== '')

// Filtering is client-side over the pages already fetched, so a search can only
// see what has been loaded. The bar under the box says so rather than letting
// an empty result imply the session does not exist.
const visible = computed(() => {
  const needle = q.value.trim().toLowerCase()
  if (!needle) return sessions.value
  return sessions.value.filter(
    (s) => (s.preview || '').toLowerCase().includes(needle) || s.id.toLowerCase().includes(needle),
  )
})

// Select-all acts on what is visible: ticking it under a filter and silently
// selecting hidden rows would make the delete button understate its reach.
const allLoadedChecked = computed(() => visible.value.length > 0 && visible.value.every((s) => checked.value.includes(s.id)))
const someChecked = computed(() => checked.value.length > 0 && !allLoadedChecked.value)
// More matching rows exist than are loaded/ticked — offer to select them all.
// Not offered under a filter, where "全部" would mean something else.
const canSelectAll = computed(() => !filtering.value && allLoadedChecked.value && checked.value.length < total.value)

onMounted(load)
watch(() => props.refreshKey, load)

async function load() {
  loading.value = !sessions.value.length
  error.value = ''
  try {
    const res = await api.sessions(PAGE, 0)
    sessions.value = res.sessions
    total.value = res.total ?? res.sessions.length
    hasMore.value = !!res.has_more
    // Drop ticks for sessions that no longer exist after a reload.
    const ids = new Set(sessions.value.map((s) => s.id))
    checked.value = checked.value.filter((id) => ids.has(id))
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
  // Toggle the visible rows; clears any prior "select all matching" superset too.
  checked.value = allLoadedChecked.value ? [] : visible.value.map((s) => s.id)
}

async function selectAllMatching() {
  try {
    checked.value = (await api.sessionIds()).ids
  } catch (e) {
    error.value = e.message
  }
}

function pick(id) {
  if (!props.disabled) emit('open', id)
}

async function removeChecked() {
  if (!checked.value.length || deleting.value) return
  if (!confirm(`确认删除选中的 ${checked.value.length} 个会话？此操作不可恢复。`)) return
  deleting.value = true
  error.value = ''
  const ids = [...checked.value]
  let failure = ''
  try {
    await api.deleteSessions(ids)
  } catch (e) {
    failure = e.message
  } finally {
    // Refreshed whether or not it reported success. A failure here is usually
    // about what could not be cleaned up afterwards — the sessions themselves
    // are already gone — and leaving the list showing them means the error
    // banner sits above rows that 404 when clicked.
    emit('deleted', ids)
    checked.value = []
    await load()
    // Restored after the refresh, not before it: load() clears the banner on
    // its way in, so setting it in the catch would publish the failure and
    // wipe it a moment later — which reads as a clean delete.
    if (failure) error.value = failure
    deleting.value = false
  }
}

async function remove(s) {
  if (!confirm(`确认删除会话「${s.preview || s.id}」？此操作不可恢复。`)) return
  let failure = ''
  try {
    await api.deleteSession(s.id)
  } catch (e) {
    failure = e.message
  } finally {
    // See removeChecked: the session is gone even when the response is not
    // ok, and the refresh that proves it must not take the message with it.
    emit('deleted', [s.id])
    checked.value = checked.value.filter((id) => id !== s.id)
    await load()
    if (failure) error.value = failure
  }
}
</script>

<template>
  <aside class="session-list" aria-label="会话列表">
    <div v-if="sessions.length" class="list-search">
      <Icon name="search" :size="14" />
      <input v-model="q" placeholder="搜索标题或会话 ID" @keydown.esc="q = ''" />
      <button v-if="filtering" class="clear" title="清除" @click="q = ''">×</button>
    </div>
    <div v-if="filtering && hasMore" class="hint-bar">
      仅在已加载的 {{ sessions.length }} 个会话中搜索，共 {{ total }} 个。
      <button class="link" :disabled="loadingMore" @click="loadMore">加载更多</button>
    </div>
    <div v-if="sessions.length" class="list-bar">
      <label class="selall" title="全选本页 / 取消">
        <input type="checkbox" :checked="allLoadedChecked" :indeterminate.prop="someChecked" @change="toggleAll" />
        <span>{{ checked.length ? `已选 ${checked.length}` : '全选' }} / {{ total }}</span>
      </label>
      <button v-if="checked.length" class="btn btn-danger btn-sm" :disabled="deleting" @click="removeChecked">
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
      <span class="muted">还没有会话</span>
    </div>
    <div v-if="error && sessions.length" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
    <div v-if="filtering && !visible.length" class="empty">
      <Icon name="search" :size="28" />
      <span class="muted">没有匹配的会话</span>
    </div>
    <div
      v-for="s in visible"
      :key="s.id"
      class="sess"
      :class="{ active: s.id === selected, picked: checked.includes(s.id) }"
      role="button"
      tabindex="0"
      @click="pick(s.id)"
      @keydown.enter="pick(s.id)"
    >
      <div class="sess-top">
        <input class="pick" type="checkbox" :value="s.id" v-model="checked" title="选择以批量删除" @click.stop />
        <span class="sess-title">{{ s.preview || '（空会话）' }}</span>
        <button class="del" title="删除会话" @click.stop="remove(s)"><Icon name="trash" :size="14" /></button>
      </div>
      <span class="mono sess-id">{{ s.id }}</span>
      <span class="sess-meta">
        <span class="badge">{{ s.events }} 事件</span>
        <span class="muted time" :title="absTime(s.last_update)">{{ relTime(s.last_update) }}</span>
      </span>
    </div>
    <button v-if="hasMore" class="loadmore" :disabled="loadingMore" @click="loadMore">
      <span v-if="loadingMore" class="spinner" /> 加载更多（还有 {{ total - sessions.length }} 个）
    </button>
  </aside>
</template>

<style scoped>
.session-list {
  border-right: 1px solid var(--border);
  overflow-y: auto;
  padding: var(--sp-3);
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.list-search {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  margin-bottom: var(--sp-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  color: var(--text-muted);
}
.list-search:focus-within {
  border-color: var(--primary-border);
  background: var(--surface);
}
.list-search input {
  flex: 1;
  min-width: 0;
  border: 0;
  background: none;
  outline: none;
  font: inherit;
  font-size: 13px;
  color: var(--text);
}
.list-search input::placeholder {
  color: var(--text-muted);
}
.list-search .clear {
  flex-shrink: 0;
  border: 0;
  background: none;
  cursor: pointer;
  color: var(--text-muted);
  font-size: 16px;
  line-height: 1;
  padding: 0 2px;
}
.list-search .clear:hover {
  color: var(--text);
}
.hint-bar {
  padding: var(--sp-2) var(--sp-3);
  margin-bottom: var(--sp-2);
  background: var(--surface-3);
  border-radius: var(--radius-sm);
  font-size: 12px;
  color: var(--text-dim);
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
  transition: border-color 0.15s ease, background 0.15s ease, transform 0.18s ease, box-shadow 0.18s ease;
}
.sess:hover {
  background: var(--surface-2);
  border-color: var(--border-strong);
  transform: translateY(-1px);
}
.sess.active {
  border-color: var(--primary-border);
  background: var(--primary-tint);
}
.sess.picked {
  border-color: var(--accent);
  background: var(--accent-tint);
}
.sess-top {
  display: flex;
  align-items: flex-start;
  gap: var(--sp-2);
}
/* What the session was about carries the row. It is the only field anyone
   scans for, so it gets the weight and two lines to land in. */
.sess-title {
  flex: 1;
  min-width: 0;
  font-size: 13px;
  font-weight: 500;
  line-height: 1.4;
  color: var(--text);
  display: -webkit-box;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  -webkit-box-orient: vertical;
  overflow: hidden;
}
/* The id is how you address a session in a log or a URL, not how you find it —
   so it stays available but stops competing with the title. */
.sess-id {
  font-size: 11px;
  color: var(--text-muted);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
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
</style>
