<script setup>
import { onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const core = ref(null)
const coreLoading = ref(true)
const coreError = ref('')

const query = ref('')
const searching = ref(false)
const searchError = ref('')
const searchResult = ref(null) // {enabled, results}

onMounted(loadCore)

async function loadCore() {
  coreLoading.value = true
  coreError.value = ''
  try {
    core.value = await api.memoryCore()
  } catch (e) {
    coreError.value = e.message
  } finally {
    coreLoading.value = false
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
            <div class="mem-head"><Icon name="user" :size="15" /> USER.md</div>
            <pre v-if="core.user" class="mem-body mono">{{ core.user }}</pre>
            <div v-else class="mem-empty muted">（暂无用户画像）</div>
          </div>
          <div class="card mem-card">
            <div class="mem-head"><Icon name="doc" :size="15" /> MEMORY.md</div>
            <pre v-if="core.memory" class="mem-body mono">{{ core.memory }}</pre>
            <div v-else class="mem-empty muted">（暂无长期记忆）</div>
          </div>
        </template>
      </section>

      <section class="col">
        <h2 class="section-title">L2 会话检索 · FTS5</h2>
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
              <span>L2 检索未启用。在配置中设置 <code class="mono">memory.search.enabled: true</code> 后重启服务。</span>
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
.mem-card {
  padding: 0;
  overflow: hidden;
}
.mem-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-3) var(--sp-4);
  border-bottom: 1px solid var(--border);
  font-weight: 600;
  font-size: 13px;
  color: var(--text-dim);
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
