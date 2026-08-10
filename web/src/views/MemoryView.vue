<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const core = ref(null)
const coreLoading = ref(true)
const coreError = ref('')

// inline editing of USER.md / MEMORY.md, keyed by target ('user' | 'memory')
const editTarget = ref('') // '' | 'user' | 'memory'
const draft = ref('')
const savingCore = ref(false)

const query = ref('')
const searching = ref(false)
const searchError = ref('')
const searchResult = ref(null) // {enabled, results}

const searchEnabled = ref(false)
const toggling = ref(false)
const toggleError = ref('')

// Conversation compaction. Inputs are strings so "blank = use the default" is
// representable; a max_tokens of 0 means compaction is off entirely.
const hist = ref(null) // server state incl. defaults
const histOpen = ref(false)
const histForm = ref({ max_tokens: '', keep_recent: '', tool_result_tokens: '' })
const histSaving = ref(false)
const histError = ref('')
const histNotice = ref('')

onMounted(() => {
  loadCore()
  loadHistory()
})

async function loadHistory() {
  try {
    hist.value = await api.history()
    histForm.value = {
      max_tokens: hist.value.max_tokens ?? '',
      keep_recent: hist.value.keep_recent || '',
      tool_result_tokens: hist.value.tool_result_tokens || '',
    }
  } catch (e) {
    histError.value = e.message
  }
}

// compactionOff mirrors the server rule: an explicit 0 disables compaction,
// while blank falls back to the default budget.
const compactionOff = computed(() => String(histForm.value.max_tokens).trim() === '0')

function histNum(v) {
  const s = String(v ?? '').trim()
  if (s === '') return null
  const n = Number(s)
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : null
}

async function saveHistory() {
  if (histSaving.value) return
  histSaving.value = true
  histError.value = ''
  histNotice.value = ''
  try {
    const r = await api.setHistory({
      max_tokens: histNum(histForm.value.max_tokens), // null = 用默认预算
      keep_recent: histNum(histForm.value.keep_recent) ?? 0,
      tool_result_tokens: histNum(histForm.value.tool_result_tokens) ?? 0,
    })
    histNotice.value = `已保存到 ${r.saved_to}（即时热重载）`
    await loadHistory()
  } catch (e) {
    histError.value = e.message
  } finally {
    histSaving.value = false
  }
}

async function loadCore() {
  coreLoading.value = true
  coreError.value = ''
  try {
    core.value = await api.memoryCore()
    searchEnabled.value = !!core.value.search_enabled
  } catch (e) {
    coreError.value = e.message
  } finally {
    coreLoading.value = false
  }
}

// toggleSearch flips memory.search.enabled on the server, which persists to
// config.yaml and hot-reloads the engine — so it takes effect immediately, no
// restart. New turns get indexed from here on; turns before enabling are not.
async function toggleSearch() {
  if (toggling.value) return
  toggling.value = true
  toggleError.value = ''
  try {
    const r = await api.setMemorySearch({ enabled: !searchEnabled.value })
    searchEnabled.value = !!r.enabled
    if (!searchEnabled.value) searchResult.value = null
  } catch (e) {
    toggleError.value = e.message
  } finally {
    toggling.value = false
  }
}

function startEditCore(target) {
  editTarget.value = target
  draft.value = (target === 'user' ? core.value?.user : core.value?.memory) || ''
}

function cancelEditCore() {
  editTarget.value = ''
  draft.value = ''
}

async function saveCore() {
  if (savingCore.value) return
  savingCore.value = true
  coreError.value = ''
  try {
    await api.setMemoryCore(editTarget.value, draft.value)
    editTarget.value = ''
    await loadCore()
  } catch (e) {
    coreError.value = e.message
  } finally {
    savingCore.value = false
  }
}

async function search() {
  const q = query.value.trim()
  if (!q || searching.value) return
  searching.value = true
  searchError.value = ''
  searchResult.value = null
  try {
    searchResult.value = await api.memorySearch(q)
  } catch (e) {
    searchError.value = e.message
  } finally {
    searching.value = false
  }
}

function fmtTime(unix) {
  if (!unix) return ''
  return new Date(unix * 1000).toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <h1>记忆</h1>
      <span v-if="core" class="mono dim path">{{ core.dir }}</span>
    </header>

    <div class="body">
      <section class="col">
        <h2 class="section-title">L1 核心记忆</h2>
        <div v-if="coreLoading" class="empty"><span class="spinner" /></div>
        <div v-else-if="coreError" class="error-bar"><Icon name="alert" :size="16" /> {{ coreError }}</div>
        <template v-else>
          <div class="card mem-card">
            <div class="mem-head">
              <span class="mem-title"><Icon name="user" :size="15" /> USER.md</span>
              <button v-if="editTarget !== 'user'" class="btn btn-mini" @click="startEditCore('user')">
                <Icon name="settings" :size="13" /> 编辑
              </button>
            </div>
            <template v-if="editTarget === 'user'">
              <textarea v-model="draft" class="textarea mono mem-edit" rows="6" placeholder="- 每条一行，例如：偏好简洁中文回答" />
              <div class="mem-actions">
                <button class="btn btn-mini" @click="cancelEditCore" :disabled="savingCore">取消</button>
                <button class="btn btn-mini btn-primary" @click="saveCore" :disabled="savingCore">
                  <span v-if="savingCore" class="spinner" /> 保存
                </button>
              </div>
            </template>
            <template v-else>
              <pre v-if="core.user" class="mem-body mono">{{ core.user }}</pre>
              <div v-else class="mem-empty muted">（暂无用户画像）</div>
            </template>
          </div>
          <div class="card mem-card">
            <div class="mem-head">
              <span class="mem-title"><Icon name="doc" :size="15" /> MEMORY.md</span>
              <button v-if="editTarget !== 'memory'" class="btn btn-mini" @click="startEditCore('memory')">
                <Icon name="settings" :size="13" /> 编辑
              </button>
            </div>
            <template v-if="editTarget === 'memory'">
              <textarea v-model="draft" class="textarea mono mem-edit" rows="6" placeholder="- 每条一行，例如：项目部署在阿里云" />
              <div class="mem-actions">
                <button class="btn btn-mini" @click="cancelEditCore" :disabled="savingCore">取消</button>
                <button class="btn btn-mini btn-primary" @click="saveCore" :disabled="savingCore">
                  <span v-if="savingCore" class="spinner" /> 保存
                </button>
              </div>
            </template>
            <template v-else>
              <pre v-if="core.memory" class="mem-body mono">{{ core.memory }}</pre>
              <div v-else class="mem-empty muted">（暂无长期记忆）</div>
            </template>
          </div>
        </template>
      </section>

      <section class="col">
        <!-- compaction: how much conversation survives into each request -->
        <div class="card hist-card">
          <button class="hist-head" @click="histOpen = !histOpen">
            <span class="caret" :class="{ open: histOpen }">▸</span>
            <Icon name="settings" :size="16" />
            <span class="hist-title">上下文压缩</span>
            <span class="hist-badge mono" :class="{ off: compactionOff }">
              {{ compactionOff ? '已关闭' : `${histForm.max_tokens || hist?.defaults?.max_tokens} token` }}
            </span>
          </button>
          <div v-if="histOpen" class="hist-body">
            <p class="muted hint">
              每轮把完整历史发给模型，几次 fetch_url 就能顶满上下文窗口。压缩确定性执行、不调用模型做摘要。留空 = 用默认值，保存即热重载。
            </p>
            <div class="hist-grid">
              <label class="field">
                <span class="label">历史预算（token）</span>
                <input v-model="histForm.max_tokens" class="input" type="number" min="0" step="1000"
                  :placeholder="`留空 = ${hist?.defaults?.max_tokens ?? 24000}`" />
                <span class="tiny muted">填 0 关闭压缩，永远发送完整历史</span>
              </label>
              <label class="field">
                <span class="label">保留最近条数</span>
                <input v-model="histForm.keep_recent" class="input" type="number" min="0" step="1"
                  :placeholder="`留空 = ${hist?.defaults?.keep_recent ?? 6}`" :disabled="compactionOff" />
                <span class="tiny muted">末尾这些条永不丢弃，保证当前问题送达</span>
              </label>
              <label class="field">
                <span class="label">单个工具结果上限（token）</span>
                <input v-model="histForm.tool_result_tokens" class="input" type="number" min="0" step="100"
                  :placeholder="`留空 = ${hist?.defaults?.tool_result_tokens ?? 800}`" :disabled="compactionOff" />
                <span class="tiny muted">超出后保留首尾、省略中间</span>
              </label>
            </div>
            <p v-if="!searchEnabled && !compactionOff" class="tiny warn">
              L2 会话检索当前关闭，被压缩丢弃的早期对话将无法找回。开启下方的 L2 可让 agent 用 load_memory 检索历史。
            </p>
            <div v-if="histError" class="error-bar"><Icon name="alert" :size="16" /> {{ histError }}</div>
            <div v-if="histNotice" class="notice-bar"><Icon name="check" :size="16" /> {{ histNotice }}</div>
            <div class="hist-actions">
              <button class="btn btn-primary" :disabled="histSaving" @click="saveHistory">
                <span v-if="histSaving" class="spinner" /> 保存压缩设置
              </button>
            </div>
          </div>
        </div>

        <div class="l2-head">
          <h2 class="section-title">L2 会话检索 · FTS5</h2>
          <div class="l2-toggle">
            <span v-if="toggleError" class="toggle-err mono">{{ toggleError }}</span>
            <span v-else class="mono dim toggle-state">{{ searchEnabled ? '已启用' : '已关闭' }}</span>
            <button
              class="switch"
              :class="{ on: searchEnabled }"
              role="switch"
              :aria-checked="searchEnabled"
              :disabled="toggling || coreLoading"
              :title="searchEnabled ? '点击关闭 L2 检索' : '点击启用 L2 检索'"
              @click="toggleSearch"
            >
              <span class="switch-knob" :class="{ spin: toggling }" />
            </button>
          </div>
        </div>
        <div class="card search-card">
          <div class="search-row">
            <input
              v-model="query"
              class="input"
              placeholder="全文检索历史会话…"
              @keydown.enter="search"
            />
            <button class="btn btn-primary" @click="search" :disabled="searching || !query.trim()">
              <Icon v-if="!searching" name="search" :size="16" />
              <span v-else class="spinner" />
              检索
            </button>
          </div>

          <div v-if="searchError" class="error-bar"><Icon name="alert" :size="16" /> {{ searchError }}</div>

          <template v-else-if="searchResult">
            <div v-if="!searchResult.enabled" class="notice">
              <Icon name="alert" :size="16" />
              <span>L2 检索未启用。打开右上角开关即可启用（即时生效，自动写入 <code class="mono">memory.search.enabled: true</code>）。</span>
            </div>
            <div v-else-if="!searchResult.results.length" class="empty">
              <span class="muted">未命中「{{ searchResult.query }}」</span>
            </div>
            <div v-else class="hits">
              <div v-for="(h, i) in searchResult.results" :key="i" class="hit">
                <div class="hit-head">
                  <span class="badge" :class="h.author === 'user' ? 'badge-primary' : 'badge-accent'">
                    {{ h.author }}
                  </span>
                  <span class="muted time">{{ fmtTime(h.timestamp) }}</span>
                </div>
                <div class="hit-text">{{ h.text }}</div>
              </div>
            </div>
          </template>

          <div v-else class="empty hint">
            <Icon name="memory" :size="28" />
            <span class="muted">输入关键词检索已索引的历史会话（trigram 全文匹配）</span>
          </div>
        </div>
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
  gap: var(--sp-3);
  padding: var(--sp-4) var(--sp-5);
  border-bottom: 1px solid var(--border);
}
.topbar h1 {
  font-size: 18px;
}
.path {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 50%;
}
.body {
  flex: 1;
  overflow-y: auto;
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-5);
  padding: var(--sp-5);
  align-items: start;
}
.col {
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
  min-width: 0;
}
.section-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
}
.hist-card {
  padding: 0;
  margin-bottom: var(--sp-3);
}
.hist-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  padding: var(--sp-3) var(--sp-4);
  background: none;
  border: 0;
  color: var(--text);
  cursor: pointer;
  text-align: left;
}
.hist-title {
  font-weight: 600;
  font-size: 14px;
}
.hist-badge {
  margin-left: auto;
  font-size: 11px;
  padding: 2px 8px;
  border-radius: var(--radius-sm);
  color: var(--accent);
  background: var(--accent-tint);
  border: 1px solid var(--accent-border);
}
.hist-badge.off {
  color: var(--text-dim);
  background: none;
  border-color: var(--border);
}
.caret {
  display: inline-block;
  font-size: 10px;
  color: var(--text-dim);
  transition: transform 0.15s ease;
}
.caret.open {
  transform: rotate(90deg);
}
.hist-body {
  padding: 0 var(--sp-4) var(--sp-4);
  border-top: 1px solid var(--border);
}
.hist-body .hint {
  margin: var(--sp-3) 0 0;
  font-size: 12px;
  line-height: 1.6;
}
.hist-grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-3);
  margin-top: var(--sp-3);
}
.field {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.label {
  font-size: 12px;
  color: var(--text-dim);
}
.tiny {
  font-size: 11px;
  line-height: 1.5;
}
.warn {
  margin: var(--sp-3) 0 0;
  color: var(--warning);
}
.hist-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: var(--sp-3);
}
.l2-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
}
.l2-toggle {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.toggle-state {
  font-size: 12px;
}
.toggle-err {
  font-size: 11px;
  color: var(--danger);
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.switch {
  position: relative;
  width: 40px;
  height: 22px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface-3);
  cursor: pointer;
  padding: 0;
  transition: background 0.18s ease, border-color 0.18s ease;
}
.switch.on {
  background: linear-gradient(90deg, var(--primary), var(--primary-2));
  border-color: transparent;
  box-shadow: 0 0 10px rgba(110, 139, 255, 0.32);
}
.switch:disabled {
  opacity: 0.6;
  cursor: progress;
}
.switch-knob {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: #fff;
  box-shadow:
    inset 0 1px 1px rgba(255, 255, 255, 0.6),
    0 1px 3px rgba(0, 0, 0, 0.35);
  transition: transform 0.18s ease;
}
.switch.on .switch-knob {
  transform: translateX(18px);
}
.switch-knob.spin {
  animation: knob-pulse 0.8s ease-in-out infinite;
}
@keyframes knob-pulse {
  50% {
    opacity: 0.5;
  }
}

.mem-card {
  padding: 0;
  overflow: hidden;
}
.mem-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--border);
  background: linear-gradient(180deg, rgba(255, 255, 255, 0.02), transparent);
  box-shadow: inset 0 -1px 0 rgba(0, 0, 0, 0.18);
  font-weight: 600;
  font-size: 13px;
  color: var(--text-dim);
}
.mem-title {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.btn-mini {
  padding: 2px var(--sp-2);
  font-size: 12px;
  height: auto;
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.mem-edit {
  margin: var(--sp-3);
  width: calc(100% - var(--sp-3) * 2);
}
.mem-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--sp-2);
  padding: 0 var(--sp-3) var(--sp-3);
}
.mem-body {
  margin: 0;
  padding: var(--sp-4);
  white-space: pre-wrap;
  word-break: break-word;
  font-size: 13px;
  line-height: 1.6;
}
.mem-empty {
  padding: var(--sp-4);
  font-size: 13px;
}

.search-card {
  padding: var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
}
.search-row {
  display: flex;
  gap: var(--sp-2);
}
.hits {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.hit {
  padding: var(--sp-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
}
.hit-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: var(--sp-2);
}
.time {
  font-size: 11px;
}
.hit-text {
  font-size: 13px;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-dim);
}
.notice {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  font-size: 13px;
  color: var(--text-dim);
}
.notice code {
  color: var(--warning);
}
.hint {
  padding: var(--sp-5);
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
@media (max-width: 900px) {
  .body {
    grid-template-columns: 1fr;
  }
}
</style>
