<script setup>
import { onMounted, reactive, ref, computed } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const agents = ref([])
const defaultAgent = ref('')
const providers = ref([])
const mcpServers = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false) // false | 'new' | name
const saving = ref(false)
const form = reactive({
  name: '',
  description: '',
  provider: '',
  instruction: '',
  mcp: [],
  sub_agents: [],
  enabled: true,
  make_default: false,
})

// other agents (exclude the one being edited) — candidate sub-agents
const otherAgents = computed(() => agents.value.filter((a) => a.name !== form.name))
const enabledMcp = computed(() => mcpServers.value.filter((s) => s.enabled))

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const [ag, pv, mc] = await Promise.all([api.agents(), api.providers(), api.mcp()])
    agents.value = ag.agents || []
    defaultAgent.value = ag.default_agent || ''
    providers.value = pv.providers || []
    mcpServers.value = mc.servers || []
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startNew() {
  editing.value = 'new'
  Object.assign(form, {
    name: '',
    description: '',
    provider: '',
    instruction: '',
    mcp: [],
    sub_agents: [],
    enabled: true,
    make_default: false,
  })
}

function startEdit(a) {
  editing.value = a.name
  Object.assign(form, {
    name: a.name,
    description: a.description || '',
    provider: a.provider || '',
    instruction: a.instruction || '',
    mcp: [...(a.mcp || [])],
    sub_agents: [...(a.sub_agents || [])],
    enabled: a.enabled,
    make_default: defaultAgent.value === a.name,
  })
}

function cancel() {
  editing.value = false
  error.value = ''
}

async function submit() {
  error.value = ''
  saving.value = true
  try {
    const res = await api.saveAgent({
      name: form.name.trim(),
      description: form.description.trim(),
      provider: form.provider,
      instruction: form.instruction,
      mcp: form.mcp,
      sub_agents: form.sub_agents,
      enabled: form.enabled,
      make_default: form.make_default,
    })
    notice.value = `已保存到 ${res.saved_to}（已热重载）`
    editing.value = false
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}

async function toggle(a) {
  try {
    await api.saveAgent({
      name: a.name,
      description: a.description || '',
      provider: a.provider || '',
      instruction: a.instruction || '',
      mcp: a.mcp || [],
      sub_agents: a.sub_agents || [],
      enabled: !a.enabled,
    })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function makeDefault(a) {
  try {
    await api.saveAgent({
      name: a.name,
      description: a.description || '',
      provider: a.provider || '',
      instruction: a.instruction || '',
      mcp: a.mcp || [],
      sub_agents: a.sub_agents || [],
      enabled: a.enabled,
      make_default: true,
    })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function remove(a) {
  if (!confirm(`确认删除 Agent「${a.name}」？它会从其它 Agent 的子列表中一并移除。`)) return
  try {
    await api.deleteAgent(a.name)
    await load()
  } catch (e) {
    error.value = e.message
  }
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>Agent</h1>
        <span class="muted sub">多 Agent · 协调者按需转交给子 Agent（transfer）</span>
      </div>
      <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
        <Icon name="plus" :size="16" /> 新建 Agent
      </button>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
      <p class="muted hint">
        给协调者填上「子 Agent」即开启转交：协调者的 LLM 会按每个子 Agent 的<strong>描述</strong>判断把任务交给谁。未定义任何 Agent 时，对话仍走默认的单 Agent。
      </p>

      <!-- create / edit form -->
      <div v-if="editing" class="card form">
        <h2 class="form-title">{{ editing === 'new' ? '新建 Agent' : `编辑 ${editing}` }}</h2>
        <div class="grid">
          <label class="field">
            <span class="label">名称（标识符，字母/数字/-/_）*</span>
            <input v-model="form.name" class="input mono" :disabled="editing !== 'new'" placeholder="coordinator" />
          </label>
          <label class="field">
            <span class="label">应答 Provider</span>
            <select v-model="form.provider" class="input">
              <option value="">默认 Provider</option>
              <option v-for="p in providers" :key="p.name" :value="p.name">{{ p.name }}（{{ p.model }}）</option>
            </select>
          </label>
          <label class="field span2">
            <span class="label">描述 *（供上级协调者判断何时转交到这里）</span>
            <input v-model="form.description" class="input" placeholder="擅长联网检索与资料整理" />
          </label>
          <label class="field span2">
            <span class="label">系统指令（留空 = 使用内置默认指令）</span>
            <textarea v-model="form.instruction" class="textarea" rows="5" placeholder="你是协调者，先判断该由哪个专家处理，再决定是否转交…" />
          </label>

          <div class="field span2" v-if="otherAgents.length">
            <span class="label">子 Agent（勾选 = 可转交的对象；构成协调者）</span>
            <div class="chips">
              <label v-for="a in otherAgents" :key="a.name" class="chip">
                <input type="checkbox" :value="a.name" v-model="form.sub_agents" />
                <span>{{ a.name }}</span>
              </label>
            </div>
          </div>

          <div class="field span2" v-if="enabledMcp.length">
            <span class="label">MCP（勾选要加载的服务器；不选 = 不挂 MCP）</span>
            <div class="chips">
              <label v-for="s in enabledMcp" :key="s.name" class="chip">
                <input type="checkbox" :value="s.name" v-model="form.mcp" />
                <span>{{ s.name }}</span>
              </label>
            </div>
          </div>

          <label class="check">
            <input type="checkbox" v-model="form.enabled" />
            <span>启用</span>
          </label>
          <label class="check">
            <input type="checkbox" v-model="form.make_default" />
            <span>设为默认 Agent（对话默认用它）</span>
          </label>
        </div>

        <div class="form-actions">
          <button class="btn" @click="cancel" :disabled="saving">取消</button>
          <button class="btn btn-primary" @click="submit" :disabled="saving">
            <span v-if="saving" class="spinner" /> 保存并热重载
          </button>
        </div>
      </div>

      <!-- agent list -->
      <div v-if="loading" class="empty"><span class="spinner" /></div>
      <div v-else-if="!agents.length && !editing" class="empty">
        <Icon name="bot" :size="32" />
        <div>
          <p style="margin: 0 0 4px">尚未定义任何 Agent</p>
          <p class="muted" style="margin: 0; font-size: 13px">
            新建一个协调者 + 几个专家子 Agent，对话页即可选择多 Agent 协作
          </p>
        </div>
      </div>
      <div v-else class="list">
        <div v-for="a in agents" :key="a.name" class="card srv" :class="{ off: !a.enabled }">
          <div class="srv-row">
            <div class="srv-main">
              <div class="srv-head">
                <span class="srv-name">{{ a.name }}</span>
                <span v-if="defaultAgent === a.name" class="badge badge-accent">默认</span>
                <span class="badge">{{ a.provider || '默认 Provider' }}</span>
                <span class="badge" :class="a.enabled ? 'badge-accent' : ''">{{ a.enabled ? '已启用' : '已停用' }}</span>
              </div>
              <div class="srv-meta dim">{{ a.description || '（无描述）' }}</div>
              <div v-if="(a.sub_agents || []).length || (a.mcp || []).length" class="srv-secrets">
                <span v-for="n in a.sub_agents" :key="'s' + n" class="badge" title="子 Agent（转交目标）">↪ {{ n }}</span>
                <span v-for="n in a.mcp" :key="'m' + n" class="badge mono" title="MCP">{{ n }}</span>
              </div>
            </div>
            <div class="srv-actions">
              <button class="btn" @click="makeDefault(a)" :disabled="defaultAgent === a.name" title="设为默认"><Icon name="star" :size="15" /></button>
              <button class="btn" @click="toggle(a)" :title="a.enabled ? '停用' : '启用'"><Icon name="power" :size="15" /></button>
              <button class="btn" @click="startEdit(a)" title="编辑"><Icon name="settings" :size="15" /></button>
              <button class="btn danger" @click="remove(a)" title="删除"><Icon name="trash" :size="15" /></button>
            </div>
          </div>
        </div>
      </div>
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
.hint {
  font-size: 13px;
  line-height: 1.6;
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
.chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-2);
}
.chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  padding: 4px 10px;
  border: 1px solid var(--border);
  border-radius: 999px;
  font-size: 13px;
  cursor: pointer;
  user-select: none;
}
.chip:has(input:checked) {
  border-color: var(--accent);
  background: var(--accent-tint);
  color: var(--accent);
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
  background: linear-gradient(180deg, var(--primary), var(--primary-2));
  opacity: 0;
  transition: opacity 0.18s ease;
  pointer-events: none;
}
.srv:hover {
  transform: translateY(-1px);
  border-color: var(--border-strong);
  background:
    linear-gradient(90deg, rgba(110, 139, 255, 0.05), transparent 45%),
    linear-gradient(180deg, rgba(255, 255, 255, 0.024), transparent 30%),
    var(--surface-2);
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
  flex-wrap: wrap;
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
