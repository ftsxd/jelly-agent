<script setup>
import { onMounted, ref } from 'vue'
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

onMounted(loadCore)

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
