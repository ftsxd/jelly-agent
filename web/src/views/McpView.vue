<script setup>
import { onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const servers = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false) // false | 'new' | name
const saving = ref(false)
const form = reactive({ name: '', transport: 'stdio', command: '', argsText: '', envText: '', url: '', headersText: '', enabled: true, toolsText: '' })

// per-server live test state, keyed by name: {loading, tools, error}
const tests = reactive({})

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    servers.value = (await api.mcp()).servers
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startNew() {
  editing.value = 'new'
  Object.assign(form, { name: '', transport: 'stdio', command: '', argsText: '', envText: '', url: '', headersText: '', enabled: true, toolsText: '' })
}

function startEdit(s) {
  editing.value = s.name
  Object.assign(form, {
    name: s.name,
    transport: s.transport || 'stdio',
    command: s.command || '',
    argsText: (s.args || []).join('\n'),
    // values are hidden by the API; prefill keys only so a blank value keeps it
    envText: (s.env_keys || []).map((k) => `${k}=`).join('\n'),
    url: s.url || '',
    headersText: (s.header_keys || []).map((k) => `${k}=`).join('\n'),
    enabled: s.enabled,
    toolsText: (s.tools || []).join('\n'),
  })
}

function cancel() {
  editing.value = false
  error.value = ''
}

function parseLines(text) {
  return text.split('\n').map((l) => l.trim()).filter(Boolean)
}
function parseKV(text) {
  const out = {}
  for (const line of parseLines(text)) {
    const i = line.indexOf('=')
    if (i < 0) continue
    out[line.slice(0, i).trim()] = line.slice(i + 1).trim()
  }
  return out
}

function payload() {
  return {
    name: form.name.trim(),
    transport: form.transport,
    command: form.command.trim(),
    args: parseLines(form.argsText),
    env: parseKV(form.envText),
    url: form.url.trim(),
    headers: parseKV(form.headersText),
    enabled: form.enabled,
    // Always sent, so an emptied box really clears the whitelist. The API
    // treats a missing field as "leave it alone", which is right for other
    // callers but wrong for a form the user just edited.
    tools: parseLines(form.toolsText),
  }
}

// Clicking a tool name in the test result toggles it in the whitelist. Typing
// two dozen names by hand is what makes a whitelist go unused, and the list is
// right there on screen.
function toggleTool(name) {
  const current = parseLines(form.toolsText)
  const i = current.indexOf(name)
  if (i >= 0) current.splice(i, 1)
  else current.push(name)
  form.toolsText = current.join('\n')
}

function inWhitelist(name) {
  return parseLines(form.toolsText).includes(name)
}

async function submit() {
  error.value = ''
  saving.value = true
  try {
    const res = await api.saveMCP(payload())
    notice.value = `已保存到 ${res.saved_to}（已热重载）`
    editing.value = false
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}

async function toggle(s) {
  try {
    await api.saveMCP({ name: s.name, transport: s.transport, command: s.command || '', args: s.args || [], url: s.url || '', env: {}, headers: {}, enabled: !s.enabled })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function remove(s) {
  if (!confirm(`确认删除 MCP 服务器「${s.name}」？`)) return
  try {
    await api.deleteMCP(s.name)
    delete tests[s.name]
    await load()
  } catch (e) {
    error.value = e.message
  }
}

// test a configured server by name (uses its expanded, secret-resolved config)
async function test(s) {
  tests[s.name] = { loading: true, tools: null, error: '' }
  try {
    const res = await api.testMCP({ name: s.name })
    tests[s.name] = { loading: false, tools: res.tools, conflicts: res.conflicts || [], error: '' }
  } catch (e) {
    tests[s.name] = { loading: false, tools: null, error: e.message }
  }
}

// test the in-progress form spec before saving
async function testForm() {
  const key = '__form__'
  tests[key] = { loading: true, tools: null, error: '' }
  try {
    const res = await api.testMCP(payload())
    tests[key] = { loading: false, tools: res.tools, conflicts: res.conflicts || [], error: '' }
  } catch (e) {
    tests[key] = { loading: false, tools: null, error: e.message }
  }
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>MCP</h1>
        <span class="muted sub">Model Context Protocol · 外部工具服务器</span>
      </div>
      <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
        <Icon name="plus" :size="16" /> 新建服务器
      </button>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>

      <!-- create / edit form -->
      <div v-if="editing" class="card form">
        <h2 class="form-title">{{ editing === 'new' ? '新建 MCP 服务器' : `编辑 ${editing}` }}</h2>
        <div class="grid">
          <label class="field">
            <span class="label">名称 *</span>
            <input v-model="form.name" class="input" :disabled="editing !== 'new'" placeholder="filesystem" />
          </label>
          <label class="field">
            <span class="label">传输方式</span>
            <select v-model="form.transport" class="input">
              <option value="stdio">stdio（本地命令）</option>
              <option value="http">http（Streamable）</option>
              <option value="sse">sse</option>
            </select>
          </label>

          <template v-if="form.transport === 'stdio'">
            <label class="field span2">
              <span class="label">命令 *</span>
              <input v-model="form.command" class="input mono" placeholder="npx" />
            </label>
            <label class="field span2">
              <span class="label">参数（每行一个）</span>
              <textarea v-model="form.argsText" class="textarea mono" rows="3" placeholder="-y&#10;@modelcontextprotocol/server-filesystem&#10;/tmp" />
            </label>
            <label class="field span2">
              <span class="label">环境变量（KEY=VALUE，每行一个{{ editing !== 'new' ? '；留空值=保留原值' : '' }}）</span>
              <textarea v-model="form.envText" class="textarea mono" rows="2" placeholder="GITHUB_TOKEN=ghp_xxx" />
            </label>
          </template>

          <template v-else>
            <label class="field span2">
              <span class="label">URL *</span>
              <input v-model="form.url" class="input mono" placeholder="https://api.example.com/mcp/" />
            </label>
            <label class="field span2">
              <span class="label">请求头（Key=Value，每行一个{{ editing !== 'new' ? '；留空值=保留原值' : '' }}）</span>
              <textarea v-model="form.headersText" class="textarea mono" rows="2" placeholder="Authorization=Bearer xxx" />
            </label>
          </template>

          <label class="field span2">
            <span>工具白名单（每行一个，留空表示全部加载）</span>
            <textarea v-model="form.toolsText" class="input mono" rows="3"
              placeholder="get_pods&#10;get_events"></textarea>
            <span class="hint">一个 MCP 服务器常暴露二三十个工具，而一次诊断只用到几个。未列入的工具不会进入模型上下文，也不会与其他服务器重名冲突。</span>
          </label>

          <label class="check span2">
            <input type="checkbox" v-model="form.enabled" />
            <span>启用（注入到 Agent 工具集）</span>
          </label>
        </div>

        <div v-if="tests.__form__" class="test-result">
          <div v-if="tests.__form__.loading" class="muted"><span class="spinner" /> 连接中…</div>
          <div v-else-if="tests.__form__.error" class="error-bar"><Icon name="alert" :size="14" /> {{ tests.__form__.error }}</div>
          <div v-else>
            <div v-if="tests.__form__.conflicts?.length" class="error-bar conflicts">
              <Icon name="alert" :size="14" />
              <div>
                <b>{{ tests.__form__.conflicts.length }} 个工具名与已有服务器冲突</b>
                <div v-for="c in tests.__form__.conflicts" :key="c.tool" class="mono conflict-line">{{ c.reason }}</div>
              </div>
            </div>
            <div class="tool-tags">
              <span class="badge badge-accent">{{ tests.__form__.tools.length }} 个工具</span>
              <span class="muted pick-hint">点击工具名加入/移出白名单</span>
              <button v-for="t in tests.__form__.tools" :key="t.name" type="button"
                class="badge mono pick" :class="{ picked: inWhitelist(t.name) }"
                :title="t.description" @click="toggleTool(t.name)">{{ t.name }}</button>
            </div>
          </div>
        </div>

        <div class="form-actions">
          <button class="btn" @click="cancel" :disabled="saving">取消</button>
          <button class="btn" @click="testForm" :disabled="saving"><Icon name="plug" :size="15" /> 测试连接</button>
          <button class="btn btn-primary" @click="submit" :disabled="saving">
            <span v-if="saving" class="spinner" /> 保存并热重载
          </button>
        </div>
      </div>

      <!-- server list -->
      <div v-if="loading" class="empty"><span class="spinner" /></div>
      <div v-else-if="!servers.length && !editing" class="empty">
        <Icon name="plug" :size="32" />
        <div>
          <p style="margin: 0 0 4px">尚未接入 MCP 服务器</p>
          <p class="muted" style="margin: 0; font-size: 13px">
            接入后，外部服务器的工具会和内置工具一起提供给 Agent
          </p>
        </div>
      </div>
      <div v-else class="list">
        <div v-for="s in servers" :key="s.name" class="card srv" :class="{ off: !s.enabled }">
          <div class="srv-row">
            <div class="srv-main">
              <div class="srv-head">
                <span class="srv-name">{{ s.name }}</span>
                <span class="badge">{{ s.transport }}</span>
                <span class="badge" :class="s.enabled ? 'badge-accent' : ''">{{ s.enabled ? '已启用' : '已停用' }}</span>
              </div>
              <div class="srv-meta mono dim">
                {{ s.transport === 'stdio' ? [s.command, ...(s.args || [])].join(' ') : s.url }}
              </div>
              <div v-if="(s.env_keys || []).length || (s.header_keys || []).length" class="srv-secrets">
                <span v-for="k in s.env_keys" :key="'e' + k" class="badge mono">{{ k }}</span>
                <span v-for="k in s.header_keys" :key="'h' + k" class="badge mono">{{ k }}</span>
              </div>
            </div>
            <div class="srv-actions">
              <button class="btn" @click="test(s)" title="连接并列出工具"><Icon name="plug" :size="15" /></button>
              <button class="btn" @click="toggle(s)" :title="s.enabled ? '停用' : '启用'"><Icon name="power" :size="15" /></button>
              <button class="btn" @click="startEdit(s)" title="编辑"><Icon name="settings" :size="15" /></button>
              <button class="btn danger" @click="remove(s)" title="删除"><Icon name="trash" :size="15" /></button>
            </div>
          </div>

          <div v-if="tests[s.name]" class="test-result">
            <div v-if="tests[s.name].loading" class="muted"><span class="spinner" /> 连接中…</div>
            <div v-else-if="tests[s.name].error" class="error-bar"><Icon name="alert" :size="14" /> {{ tests[s.name].error }}</div>
            <div v-else>
              <div v-if="tests[s.name].conflicts?.length" class="error-bar conflicts">
                <Icon name="alert" :size="14" />
                <div>
                  <b>{{ tests[s.name].conflicts.length }} 个工具名冲突，这些工具不会生效</b>
                  <div v-for="c in tests[s.name].conflicts" :key="c.tool" class="mono conflict-line">{{ c.reason }}</div>
                  <div class="conflict-fix">改法：给它们起不同的名字（工具元数据里的 name），或用白名单只加载其中一个服务器的。</div>
                </div>
              </div>
              <div class="tool-tags">
                <span class="badge badge-accent">{{ tests[s.name].tools.length }} 个工具</span>
                <span v-if="s.tools?.length" class="badge">白名单 {{ s.tools.length }} 个</span>
                <span v-for="t in tests[s.name].tools" :key="t.name" class="badge mono"
                  :class="{ 'badge-dim': s.tools?.length && !s.tools.includes(t.name) }"
                  :title="s.tools?.length && !s.tools.includes(t.name) ? '未列入白名单，不会加载：' + (t.description || '') : t.description">{{ t.name }}</span>
              </div>
            </div>
          </div>
        </div>
      </div>
    </div>
  </div>
</template>

<style scoped>
.hint {
  font-size: 12px;
  color: var(--text-muted);
  line-height: 1.55;
}

/* A conflicting tool silently does nothing, so the report needs room to say
   which two entries collided and how to fix it. */
.conflicts {
  align-items: flex-start;
  gap: 8px;
  margin-bottom: 10px;
  text-align: left;
  white-space: normal;
}

.conflict-line {
  font-size: 12px;
  margin-top: 4px;
  opacity: 0.9;
}

.conflict-fix {
  font-size: 12px;
  margin-top: 6px;
  opacity: 0.75;
}

.pick-hint {
  font-size: 12px;
}

/* Typing two dozen tool names by hand is what makes a whitelist go unused, so
   the names double as buttons. */
.pick {
  cursor: pointer;
  border-color: var(--border);
  background: transparent;
}

.pick:hover {
  border-color: var(--border-strong);
}

.pick.picked {
  border-color: var(--primary);
  color: var(--primary);
  background: var(--primary-tint);
}

/* A tool outside the whitelist is configured but will not load — shown, and
   visibly inert. */
.badge-dim {
  opacity: 0.4;
}
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
  align-items: baseline;
  gap: var(--sp-3);
}
.topbar-l h1 {
  font-size: 18px;
}
.sub {
  font-size: 12px;
}
.body {
  flex: 1;
  overflow-y: auto;
  padding: var(--sp-5);
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
  max-width: 860px;
  width: 100%;
  margin: 0 auto;
}

.form {
  padding: var(--sp-4);
}
.form-title {
  font-size: 15px;
  margin-bottom: var(--sp-4);
}
.grid {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: var(--sp-3);
}
.field {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.span2 {
  grid-column: 1 / -1;
}
.label {
  font-size: 12px;
  color: var(--text-dim);
}
.check {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  color: var(--text-dim);
  font-size: 13px;
  cursor: pointer;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
  gap: var(--sp-2);
  margin-top: var(--sp-4);
}

.list {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.srv {
  padding: var(--sp-3) var(--sp-4);
  transition: transform 0.18s ease, border-color 0.18s ease, background 0.18s ease, box-shadow 0.18s ease;
}
.srv::after {
  content: '';
  position: absolute;
  left: 0;
  top: 12px;
  bottom: 12px;
  width: 3px;
  border-radius: 0 999px 999px 0;
  background: var(--primary);
  opacity: 0;
  transition: opacity 0.18s ease;
  pointer-events: none;
}
.srv:hover {
  transform: translateY(-1px);
  border-color: var(--border-strong);
  background: var(--surface-2);
}
.srv:hover::after {
  opacity: 1;
}
.srv.off {
  opacity: 0.6;
}
.srv-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
}
.srv-main {
  min-width: 0;
}
.srv-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.srv-name {
  font-weight: 600;
}
.srv-meta {
  font-size: 12px;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.srv-secrets {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
  margin-top: var(--sp-2);
}
.srv-actions {
  display: flex;
  gap: var(--sp-2);
  flex-shrink: 0;
}
.btn.danger:hover {
  border-color: var(--danger);
  color: var(--danger);
}

.test-result {
  margin-top: var(--sp-3);
  padding-top: var(--sp-3);
  border-top: 1px solid var(--border);
}
.tool-tags {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}

.notice-bar {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  background: var(--accent-tint);
  color: var(--accent);
  border-radius: var(--radius-sm);
  font-size: 13px;
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
@media (max-width: 600px) {
  .grid {
    grid-template-columns: 1fr;
  }
  .span2 {
    grid-column: auto;
  }
}
</style>
