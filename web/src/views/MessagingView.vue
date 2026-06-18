<script setup>
import { onMounted, onUnmounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const bots = ref([])
const providers = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false) // false | 'new' | name
const saving = ref(false)
const form = reactive({ name: '', type: 'dingtalk', client_id: '', client_secret: '', provider: '', enabled: true })

let poll = null

onMounted(async () => {
  try {
    providers.value = (await api.providers()).providers
  } catch {
    /* provider list is optional context */
  }
  await load()
  // Refresh connection state so a bot's badge moves connecting → online.
  poll = setInterval(refresh, 3000)
})
onUnmounted(() => poll && clearInterval(poll))

async function load() {
  loading.value = true
  error.value = ''
  try {
    bots.value = (await api.platforms()).platforms
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

async function refresh() {
  if (editing.value) return // don't yank the form out from under an edit
  try {
    bots.value = (await api.platforms()).platforms
  } catch {
    /* transient; keep last known */
  }
}

function startNew() {
  editing.value = 'new'
  Object.assign(form, { name: '', type: 'dingtalk', client_id: '', client_secret: '', provider: '', enabled: true })
}

function startEdit(b) {
  editing.value = b.name
  Object.assign(form, {
    name: b.name,
    type: b.type || 'dingtalk',
    client_id: b.client_id || '',
    client_secret: '', // never prefilled; blank keeps the stored secret
    provider: b.provider || '',
    enabled: b.enabled,
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
    const res = await api.savePlatform({
      name: form.name.trim(),
      type: form.type,
      client_id: form.client_id.trim(),
      client_secret: form.client_secret,
      provider: form.provider,
      enabled: form.enabled,
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

async function toggle(b) {
  try {
    // Empty client_secret keeps the stored one server-side.
    await api.savePlatform({ name: b.name, type: b.type, client_id: b.client_id || '', client_secret: '', provider: b.provider || '', enabled: !b.enabled })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function remove(b) {
  if (!confirm(`确认删除机器人「${b.name}」？`)) return
  try {
    await api.deletePlatform(b.name)
    await load()
  } catch (e) {
    error.value = e.message
  }
}

const stateLabel = { online: '在线', connecting: '连接中', error: '错误', stopped: '已停止' }
function stateClass(s) {
  return { online: 'badge-accent', connecting: 'badge-primary', error: 'badge-danger', stopped: '' }[s] || ''
}
</script>

<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>消息绑定</h1>
        <span class="muted sub">把钉钉等平台接入同一套 Agent</span>
      </div>
      <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
        <Icon name="plus" :size="16" /> 新建机器人
      </button>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>

      <!-- create / edit form -->
      <div v-if="editing" class="card form">
        <h2 class="form-title">{{ editing === 'new' ? '新建机器人' : `编辑 ${editing}` }}</h2>
        <div class="grid">
          <label class="field">
            <span class="label">名称 *</span>
            <input v-model="form.name" class="input" :disabled="editing !== 'new'" placeholder="dingtalk-bot" />
          </label>
          <label class="field">
            <span class="label">平台</span>
            <select v-model="form.type" class="input">
              <option value="dingtalk">钉钉（Stream 模式）</option>
            </select>
          </label>

          <label class="field span2">
            <span class="label">ClientID（AppKey）*</span>
            <input v-model="form.client_id" class="input mono" placeholder="dingxxxxxxxxxxxx" />
          </label>
          <label class="field span2">
            <span class="label">
              ClientSecret（AppSecret）{{ editing === 'new' ? ' *' : '（留空=保留原值）' }}
            </span>
            <input v-model="form.client_secret" class="input mono" type="password" :placeholder="editing === 'new' ? '' : '••••••••（已设置）'" />
          </label>
          <label class="field span2">
            <span class="label">应答 Provider（留空=默认）</span>
            <select v-model="form.provider" class="input">
              <option value="">默认 Provider</option>
              <option v-for="p in providers" :key="p.name" :value="p.name">{{ p.name }} · {{ p.model }}</option>
            </select>
          </label>

          <label class="check span2">
            <input type="checkbox" v-model="form.enabled" />
            <span>启用（保存后立即连接钉钉）</span>
          </label>
        </div>

        <div class="form-actions">
          <button class="btn" @click="cancel" :disabled="saving">取消</button>
          <button class="btn btn-primary" @click="submit" :disabled="saving">
            <span v-if="saving" class="spinner" /> 保存并热重载
          </button>
        </div>
      </div>

      <!-- bot list -->
      <div v-if="loading" class="empty"><span class="spinner" /></div>
      <div v-else-if="!bots.length && !editing" class="empty">
        <Icon name="message" :size="32" />
        <div>
          <p style="margin: 0 0 4px">尚未绑定任何消息平台</p>
          <p class="muted" style="margin: 0; font-size: 13px">
            新建钉钉机器人后，在钉钉里 @它 即可用同一套 Agent（含记忆/工具）对话
          </p>
        </div>
      </div>
      <div v-else class="list">
        <div v-for="b in bots" :key="b.name" class="card bot" :class="{ off: !b.enabled }">
          <div class="bot-main">
            <div class="bot-head">
              <span class="bot-name">{{ b.name }}</span>
              <span class="badge">{{ b.type }}</span>
              <span class="badge" :class="stateClass(b.state)">
                <span class="dot" :class="b.state" /> {{ stateLabel[b.state] || b.state }}
              </span>
              <span v-if="!b.enabled" class="badge">已停用</span>
            </div>
            <div class="bot-meta mono dim">
              {{ b.client_id || '（未设置 ClientID）' }} ·
              {{ b.has_secret ? '密钥已设置' : '密钥未设置' }}
              <template v-if="b.provider"> · Provider: {{ b.provider }}</template>
            </div>
            <div v-if="b.state === 'error' && b.detail" class="bot-err mono">{{ b.detail }}</div>
          </div>
          <div class="bot-actions">
            <button class="btn" @click="toggle(b)" :title="b.enabled ? '停用' : '启用'"><Icon name="power" :size="15" /></button>
            <button class="btn" @click="startEdit(b)" title="编辑"><Icon name="settings" :size="15" /></button>
            <button class="btn danger" @click="remove(b)" title="删除"><Icon name="trash" :size="15" /></button>
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
.bot {
  padding: var(--sp-3) var(--sp-4);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
}
.bot.off {
  opacity: 0.6;
}
.bot-main {
  min-width: 0;
}
.bot-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.bot-name {
  font-weight: 600;
}
.bot-meta {
  font-size: 12px;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.bot-err {
  font-size: 12px;
  margin-top: var(--sp-1);
  color: var(--danger);
  word-break: break-word;
}
.bot-actions {
  display: flex;
  gap: var(--sp-2);
  flex-shrink: 0;
}
.btn.danger:hover {
  border-color: var(--danger);
  color: var(--danger);
}
.badge-danger {
  background: var(--danger-tint);
  color: var(--danger);
}
.dot {
  display: inline-block;
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--text-muted);
  margin-right: 3px;
}
.dot.online {
  background: var(--accent);
  box-shadow: 0 0 6px var(--accent);
}
.dot.connecting {
  background: var(--primary);
}
.dot.error {
  background: var(--danger);
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
