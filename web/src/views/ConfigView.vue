<script setup>
import { onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const providers = ref([])
const defaultName = ref('')
const loading = ref(true)
const error = ref('')
const notice = ref('')
const savedTo = ref('')

const editing = ref(false) // false | 'new' | provider name
const saving = ref(false)
const advOpen = ref(false)

// Tuning inputs are kept as strings so "blank" (= use the endpoint default) is
// representable; submit() converts them to numbers.
const BLANK_TUNING = { temperature: '', max_tokens: '', timeout_sec: '', max_retries: '', context_window: '' }
const DEFAULT_RETRIES = 2
const form = reactive({ name: '', base_url: '', api_key: '', model: '', make_default: false, ...BLANK_TUNING })

// tuningOf maps a provider from the API onto the form's string fields. A null
// or 0 from the server means "unset" and shows as blank — except max_retries,
// where 0 means "never retry" and must stay visible as 0.
function tuningOf(p) {
  return {
    temperature: p.temperature ?? '',
    max_tokens: p.max_tokens || '',
    timeout_sec: p.timeout_sec || '',
    max_retries: p.max_retries ?? DEFAULT_RETRIES,
    context_window: p.context_window || '',
  }
}

// Any non-default tuning is worth surfacing on the card and auto-expanding the
// advanced section, so a setting can't sit there forgotten.
function tuningSummary(p) {
  const bits = []
  if (p.temperature != null) bits.push(`温度 ${p.temperature}`)
  if (p.max_tokens) bits.push(`≤${p.max_tokens} tokens`)
  if (p.timeout_sec) bits.push(`首字节 ${p.timeout_sec}s`)
  if (p.max_retries != null && p.max_retries !== DEFAULT_RETRIES) {
    bits.push(p.max_retries === 0 ? '不重试' : `重试 ${p.max_retries}`)
  }
  if (p.context_window) bits.push(`窗口 ${p.context_window}`)
  return bits.join(' · ')
}

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const data = await api.providers()
    providers.value = data.providers
    defaultName.value = data.default
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startNew() {
  editing.value = 'new'
  advOpen.value = false
  Object.assign(form, {
    name: '', base_url: 'https://api.deepseek.com/v1', api_key: '', model: '',
    make_default: !providers.value.length,
    ...BLANK_TUNING, max_retries: DEFAULT_RETRIES,
  })
}

function startEdit(p) {
  editing.value = p.name
  advOpen.value = !!tuningSummary(p) // already customised ⇒ show it
  Object.assign(form, { name: p.name, base_url: p.base_url, api_key: '', model: p.model, make_default: p.is_default, ...tuningOf(p) })
}

function cancel() {
  editing.value = false
  error.value = ''
}

// num parses a tuning input, treating blank/garbage as `fallback`.
function num(v, fallback = 0) {
  const s = String(v ?? '').trim()
  if (s === '') return fallback
  const n = Number(s)
  return Number.isFinite(n) ? n : fallback
}

async function submit() {
  error.value = ''
  if (!form.name.trim()) {
    error.value = 'name 不能为空'
    return
  }
  const temperature = num(form.temperature)
  if (temperature < 0 || temperature > 2) {
    error.value = '温度需在 0–2 之间'
    advOpen.value = true
    return
  }
  saving.value = true
  try {
    // Tuning is always sent explicitly: a 0 clears the field back to the
    // endpoint default (for max_retries, 0 means "never retry").
    const res = await api.saveProvider({
      name: form.name,
      base_url: form.base_url,
      api_key: form.api_key,
      model: form.model,
      make_default: form.make_default,
      temperature,
      max_tokens: num(form.max_tokens),
      timeout_sec: num(form.timeout_sec),
      max_retries: num(form.max_retries, DEFAULT_RETRIES),
      context_window: num(form.context_window),
    })
    savedTo.value = res.saved_to
    notice.value = `已保存到 ${res.saved_to}`
    editing.value = false
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}

async function setDefault(p) {
  try {
    // Tuning fields are deliberately omitted: the server keeps whatever is
    // already stored when a key is absent, so this can't wipe them.
    await api.saveProvider({ name: p.name, base_url: p.base_url, model: p.model, api_key: '', make_default: true })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function remove(p) {
  if (!confirm(`确认删除 Provider「${p.name}」？`)) return
  try {
    await api.deleteProvider(p.name)
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
        <h1>配置</h1>
        <span v-if="savedTo" class="mono dim path">{{ savedTo }}</span>
      </div>
      <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
        <Icon name="plus" :size="16" /> 新建 Provider
      </button>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>

      <!-- create / edit form -->
      <div v-if="editing" class="card form">
        <h2 class="form-title">{{ editing === 'new' ? '新建 Provider' : `编辑 ${editing}` }}</h2>
        <div class="grid">
          <label class="field">
            <span class="label">名称 *</span>
            <input v-model="form.name" class="input" :disabled="editing !== 'new'" placeholder="deepseek" />
          </label>
          <label class="field">
            <span class="label">模型 *</span>
            <input v-model="form.model" class="input" placeholder="deepseek-chat" />
          </label>
          <label class="field span2">
            <span class="label">Base URL *</span>
            <input v-model="form.base_url" class="input mono" placeholder="https://api.deepseek.com/v1" />
          </label>
          <label class="field span2">
            <span class="label">API Key {{ editing === 'new' ? '' : '（留空 = 保留原 key）' }}</span>
            <input v-model="form.api_key" class="input mono" type="password" placeholder="sk-…" autocomplete="off" />
          </label>
          <label class="check span2">
            <input type="checkbox" v-model="form.make_default" />
            <span>设为默认 Provider</span>
          </label>
        </div>

        <!-- tuning: generation params + resilience, all optional -->
        <div class="adv">
          <button class="adv-head" type="button" @click="advOpen = !advOpen">
            <span class="caret" :class="{ open: advOpen }">▸</span>
            <span class="adv-title">高级参数</span>
            <span class="adv-hint muted">温度 / 长度 / 超时 / 重试</span>
          </button>
          <div v-if="advOpen" class="adv-body">
            <div class="grid">
              <label class="field">
                <span class="label">温度</span>
                <input v-model="form.temperature" class="input" type="number" step="0.1" min="0" max="2" placeholder="留空 = 服务端默认" />
                <span class="tiny muted">0–2。填 0 等同留空（该值无法发送），要近似确定性请填 0.01</span>
              </label>
              <label class="field">
                <span class="label">最大回复 tokens</span>
                <input v-model="form.max_tokens" class="input" type="number" min="0" step="1" placeholder="留空 = 服务端默认" />
                <span class="tiny muted">回复长度上限</span>
              </label>
              <label class="field">
                <span class="label">首字节超时（秒）</span>
                <input v-model="form.timeout_sec" class="input" type="number" min="0" step="1" placeholder="留空 = 120" />
                <span class="tiny muted">只等首字节，不限整轮时长，长流式回复不会被掐断</span>
              </label>
              <label class="field">
                <span class="label">重试次数</span>
                <input v-model="form.max_retries" class="input" type="number" min="0" step="1" placeholder="2" />
                <span class="tiny muted">仅 429 / 5xx / 网络抖动，退避 0.5s→8s。填 0 关闭；4xx 一律不重试</span>
              </label>
              <!-- Stated, not discovered: OpenAI-compatible endpoints do not
                   report it, and a model-name table in the binary goes stale. -->
              <label class="field">
                <span class="label">上下文窗口（token）</span>
                <input v-model="form.context_window" class="input" type="number" min="0" step="1000" placeholder="留空 = 用保守默认" />
                <span class="tiny muted">
                  填了之后历史预算按它的 60% 自动推导，不必手填。留空则用 24000 的通用默认
                </span>
              </label>
            </div>
          </div>
        </div>
        <div class="form-actions">
          <button class="btn" @click="cancel" :disabled="saving">取消</button>
          <button class="btn btn-primary" @click="submit" :disabled="saving">
            <span v-if="saving" class="spinner" /> 保存并热重载
          </button>
        </div>
      </div>

      <!-- provider list -->
      <div v-if="loading" class="empty"><span class="spinner" /></div>
      <div v-else-if="!providers.length && !editing" class="empty">
        <Icon name="settings" :size="32" />
        <div>
          <p style="margin: 0 0 4px">尚未配置任何 Provider</p>
          <p class="muted" style="margin: 0; font-size: 13px">新建一个 OpenAI 兼容端点即可在「对话」页开始聊天</p>
        </div>
      </div>
      <div v-else class="list">
        <div v-for="p in providers" :key="p.name" class="card prov" :class="{ def: p.is_default }">
          <div class="prov-main">
            <div class="prov-head">
              <span class="prov-name">{{ p.name }}</span>
              <span v-if="p.is_default" class="badge badge-primary"><Icon name="star" :size="11" /> 默认</span>
            </div>
            <div class="prov-meta mono dim">{{ p.model }} · {{ p.base_url }}</div>
            <div class="prov-key mono muted">key {{ p.api_key }}</div>
            <div v-if="tuningSummary(p)" class="prov-tuning muted">{{ tuningSummary(p) }}</div>
          </div>
          <div class="prov-actions">
            <button v-if="!p.is_default" class="btn" @click="setDefault(p)" title="设为默认">
              <Icon name="star" :size="15" />
            </button>
            <button class="btn" @click="startEdit(p)" title="编辑">
              <Icon name="settings" :size="15" />
            </button>
            <button class="btn danger" @click="remove(p)" title="删除">
              <Icon name="trash" :size="15" />
            </button>
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
  align-items: center;
  gap: var(--sp-3);
}
.topbar-l h1 {
  font-size: 18px;
}
.path {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  max-width: 40vw;
}
.body {
  flex: 1;
  overflow-y: auto;
  padding: var(--sp-5);
  display: flex;
  flex-direction: column;
  gap: var(--sp-4);
  max-width: 820px;
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

.adv {
  margin-top: var(--sp-3);
  border-top: 1px solid var(--border);
}
.adv-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  width: 100%;
  padding: var(--sp-3) 0 0;
  background: none;
  border: 0;
  color: var(--text);
  cursor: pointer;
  text-align: left;
}
.adv-title {
  font-size: 13px;
  font-weight: 600;
}
.adv-hint {
  font-size: 12px;
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
.adv-body {
  padding-top: var(--sp-3);
}
.tiny {
  font-size: 11px;
  line-height: 1.5;
}
.prov-tuning {
  margin-top: 4px;
  font-size: 12px;
}

.list {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.prov {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  padding: var(--sp-3) var(--sp-4);
  transition: transform 0.18s ease, border-color 0.18s ease, background 0.18s ease, box-shadow 0.18s ease;
}
.prov::after {
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
.prov:hover {
  transform: translateY(-1px);
  border-color: var(--border-strong);
  background: var(--surface-2);
}
.prov:hover::after {
  opacity: 1;
}
.prov.def {
  border-color: var(--primary-border);
}
.prov.def:hover {
  border-color: var(--primary);
}
.prov-main {
  min-width: 0;
}
.prov-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.prov-name {
  font-weight: 600;
}
.prov-meta {
  font-size: 12px;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.prov-key {
  font-size: 11px;
  margin-top: 2px;
}
.prov-actions {
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
