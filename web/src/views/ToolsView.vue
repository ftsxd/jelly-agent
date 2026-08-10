<script setup>
import { onMounted, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const tools = ref([])
const loading = ref(true)
const listError = ref('')

const tab = ref('web_search') // which tool the bench is exercising

const query = ref('')
const maxNum = ref(5)
const running = ref(false)
const result = ref(null)
const runError = ref('')

const url = ref('')
const maxChars = ref(8000)
const fetched = ref(null)

onMounted(load)

function switchTab(name) {
  tab.value = name
  runError.value = ''
}

async function load() {
  loading.value = true
  listError.value = ''
  try {
    tools.value = (await api.tools()).tools
  } catch (e) {
    listError.value = e.message
  } finally {
    loading.value = false
  }
}

async function run() {
  const q = query.value.trim()
  if (!q || running.value) return
  running.value = true
  runError.value = ''
  result.value = null
  try {
    result.value = await api.testTool(q, Number(maxNum.value) || 5)
  } catch (e) {
    runError.value = e.message
  } finally {
    running.value = false
  }
}

async function runFetch() {
  const u = url.value.trim()
  if (!u || running.value) return
  running.value = true
  runError.value = ''
  fetched.value = null
  try {
    fetched.value = await api.fetchUrl(u, Number(maxChars.value) || 8000)
  } catch (e) {
    runError.value = e.message
  } finally {
    running.value = false
  }
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <h1>工具</h1>
      <button class="btn" @click="load" :disabled="loading">
        <Icon name="refresh" :size="16" /> 刷新
      </button>
    </header>

    <div class="body">
      <section class="col">
        <h2 class="section-title">内置工具</h2>
        <div v-if="loading" class="empty"><span class="spinner" /> 加载中…</div>
        <div v-else-if="listError" class="error-bar"><Icon name="alert" :size="16" /> {{ listError }}</div>
        <div v-else class="tool-list">
          <div v-for="t in tools" :key="t.name" class="card tool-card">
            <div class="tool-card-head">
              <Icon name="tool" :size="16" />
              <span class="mono name">{{ t.name }}</span>
            </div>
            <p class="desc">{{ t.description }}</p>
          </div>
        </div>
      </section>

      <section class="col">
        <h2 class="section-title">测试台</h2>
        <div class="tabs">
          <button class="tab" :class="{ on: tab === 'web_search' }" @click="switchTab('web_search')">web_search</button>
          <button class="tab" :class="{ on: tab === 'fetch_url' }" @click="switchTab('fetch_url')">fetch_url</button>
        </div>

        <div v-if="tab === 'fetch_url'" class="card runner">
          <label class="field">
            <span class="label">网址</span>
            <input
              v-model="url"
              class="input mono"
              placeholder="https://example.com"
              @keydown.enter="runFetch"
            />
          </label>
          <div class="row">
            <label class="field max-field">
              <span class="label">最大字符</span>
              <input v-model="maxChars" class="input" type="number" min="100" max="40000" step="1000" />
            </label>
            <button class="btn btn-primary run-btn" @click="runFetch" :disabled="running || !url.trim()">
              <Icon v-if="!running" name="link" :size="16" />
              <span v-else class="spinner" />
              {{ running ? '抓取中…' : '抓取正文' }}
            </button>
          </div>
          <p class="muted hint">只接受公网 http/https 地址；内网、环回与云元数据地址会被拒绝。</p>

          <div v-if="runError" class="error-bar"><Icon name="alert" :size="16" /> {{ runError }}</div>

          <div v-if="fetched" class="results">
            <div class="results-meta mono dim">
              {{ fetched.content_type || '未知类型' }}
              <template v-if="fetched.truncated"> · 已截断</template>
            </div>
            <div class="fetch-title">{{ fetched.title || '（无标题）' }}</div>
            <a v-if="fetched.final_url" class="hit-url mono dim" :href="fetched.final_url" target="_blank" rel="noopener">
              重定向至 {{ fetched.final_url }}
            </a>
            <pre class="fetch-body">{{ fetched.content }}</pre>
          </div>
        </div>

        <div v-else class="card runner">
          <label class="field">
            <span class="label">查询关键词</span>
            <input
              v-model="query"
              class="input"
              placeholder="例如：ADK-Go 是什么"
              @keydown.enter="run"
            />
          </label>
          <div class="row">
            <label class="field max-field">
              <span class="label">返回条数</span>
              <input v-model="maxNum" class="input" type="number" min="1" max="10" />
            </label>
            <button class="btn btn-primary run-btn" @click="run" :disabled="running || !query.trim()">
              <Icon v-if="!running" name="search" :size="16" />
              <span v-else class="spinner" />
              {{ running ? '搜索中…' : '运行搜索' }}
            </button>
          </div>

          <div v-if="runError" class="error-bar"><Icon name="alert" :size="16" /> {{ runError }}</div>

          <div v-if="result" class="results">
            <div class="results-meta mono dim">
              来源 {{ result.source }} · {{ result.results.length }} 条
            </div>
            <a
              v-for="(r, i) in result.results"
              :key="i"
              class="hit"
              :href="r.url || undefined"
              :target="r.url ? '_blank' : undefined"
              rel="noopener"
            >
              <div class="hit-title">
                {{ r.title || '（无标题）' }}
                <Icon v-if="r.url" name="link" :size="13" class="dim" />
              </div>
              <div class="hit-snippet">{{ r.snippet }}</div>
              <div v-if="r.url" class="hit-url mono dim">{{ r.url }}</div>
            </a>
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
  padding: var(--sp-4) var(--sp-5);
  border-bottom: 1px solid var(--border);
}
.topbar h1 {
  font-size: 18px;
}
.body {
  flex: 1;
  overflow-y: auto;
  display: grid;
  grid-template-columns: minmax(280px, 360px) 1fr;
  gap: var(--sp-5);
  padding: var(--sp-5);
  align-items: start;
}
.section-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--text-dim);
  text-transform: uppercase;
  letter-spacing: 0.04em;
  margin-bottom: var(--sp-3);
}
.tool-list {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.tool-card {
  padding: var(--sp-3) var(--sp-4);
}
.tool-card-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  color: var(--primary);
  margin-bottom: var(--sp-1);
}
.tool-card-head .name {
  font-weight: 600;
  color: var(--text);
}
.desc {
  margin: 0;
  font-size: 13px;
  color: var(--text-dim);
}

.runner {
  padding: var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-3);
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
.row {
  display: flex;
  gap: var(--sp-3);
  align-items: flex-end;
}
.max-field {
  width: 110px;
}
.run-btn {
  flex: 1;
}
.tabs {
  display: flex;
  gap: var(--sp-2);
  margin-bottom: var(--sp-3);
}
.tab {
  padding: 6px 14px;
  font-family: var(--font-mono);
  font-size: 12px;
  color: var(--text-dim);
  background: none;
  border: 1px solid var(--border);
  border-radius: var(--radius);
  cursor: pointer;
}
.tab:hover {
  color: var(--text);
}
.tab.on {
  color: var(--text);
  border-color: var(--accent-border);
  background: var(--accent-tint);
}
.hint {
  margin: 0;
  font-size: 12px;
}
.fetch-title {
  margin-top: var(--sp-2);
  font-weight: 600;
  font-size: 14px;
}
.fetch-body {
  margin: var(--sp-2) 0 0;
  padding: var(--sp-3);
  max-height: 420px;
  overflow: auto;
  font-family: var(--font-mono);
  font-size: 12px;
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-dim);
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}

.results {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
  margin-top: var(--sp-2);
}
.results-meta {
  font-size: 12px;
}
.hit {
  display: block;
  padding: var(--sp-3);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  background: var(--surface-2);
  transition: border-color 0.15s ease, background 0.15s ease;
}
.hit:hover {
  border-color: var(--primary-border);
  background: var(--surface-3);
}
.hit-title {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-weight: 600;
  color: var(--text);
  margin-bottom: 2px;
}
.hit-snippet {
  font-size: 13px;
  color: var(--text-dim);
}
.hit-url {
  font-size: 11px;
  margin-top: var(--sp-1);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
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
