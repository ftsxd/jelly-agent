<script setup>
import { computed, onMounted, ref } from 'vue'
import Icon from './Icon.vue'
import { api } from '../api'

// Conversation compaction: how much history rides along with each request.
// Inputs are strings so "blank = use the default" is representable; a
// max_tokens of 0 means compaction is off entirely.
const hist = ref(null) // server state incl. defaults
const open = ref(false)
const form = ref({ max_tokens: '', keep_recent: '', tool_result_tokens: '', max_result_bytes: '' })
const saving = ref(false)
const error = ref('')
const notice = ref('')
// Only for the warning below: dropped history is lost unless L2 can find it.
const searchEnabled = ref(true)

onMounted(() => {
  load()
  api.memoryCore().then(c => { searchEnabled.value = !!c.search_enabled }).catch(() => {})
})

async function load() {
  try {
    hist.value = await api.history()
    form.value = {
      max_tokens: hist.value.max_tokens ?? '',
      keep_recent: hist.value.keep_recent || '',
      tool_result_tokens: hist.value.tool_result_tokens || '',
      max_result_bytes: hist.value.max_result_bytes || '',
    }
  } catch (e) {
    error.value = e.message
  }
}

// Mirrors the server rule: an explicit 0 disables compaction, while blank
// falls back to the default budget.
const off = computed(() => String(form.value.max_tokens).trim() === '0')

function num(v) {
  const s = String(v ?? '').trim()
  if (s === '') return null
  const n = Number(s)
  return Number.isFinite(n) && n >= 0 ? Math.floor(n) : null
}

async function save() {
  if (saving.value) return
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.setHistory({
      max_tokens: num(form.value.max_tokens), // null = 用默认预算
      keep_recent: num(form.value.keep_recent) ?? 0,
      tool_result_tokens: num(form.value.tool_result_tokens) ?? 0,
      max_result_bytes: num(form.value.max_result_bytes) ?? 0,
    })
    notice.value = `已保存到 ${r.saved_to}（即时热重载）`
    await load()
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="card hist-card">
    <button class="hist-head" @click="open = !open">
      <span class="caret" :class="{ open }">▸</span>
      <Icon name="settings" :size="16" />
      <span class="hist-title">上下文压缩</span>
      <span class="hist-badge mono" :class="{ off }">
        {{ off ? '已关闭' : `${form.max_tokens || hist?.defaults?.max_tokens} token` }}
      </span>
    </button>
    <div v-if="open" class="hist-body">
      <p class="muted hint">控制每轮带多少历史对话发给模型。确定性裁剪，不调用模型做摘要；留空用默认值，保存即热重载。</p>
      <div class="hist-grid">
        <label class="field">
          <span class="label">历史预算（token）</span>
          <input v-model="form.max_tokens" class="input" type="number" min="0" step="1000"
            :placeholder="`留空 = ${hist?.defaults?.max_tokens ?? 24000}`" />
          <span class="tiny muted">填 0 关闭压缩，永远发送完整历史</span>
        </label>
        <label class="field">
          <span class="label">保留最近条数</span>
          <input v-model="form.keep_recent" class="input" type="number" min="0" step="1"
            :placeholder="`留空 = ${hist?.defaults?.keep_recent ?? 6}`" :disabled="off" />
          <span class="tiny muted">末尾这些条永不丢弃，保证当前问题送达</span>
        </label>
        <label class="field">
          <span class="label">单个工具结果上限（token）</span>
          <input v-model="form.tool_result_tokens" class="input" type="number" min="0" step="100"
            :placeholder="`留空 = ${hist?.defaults?.tool_result_tokens ?? 800}`" :disabled="off" />
          <span class="tiny muted">超出后保留首尾、省略中间</span>
        </label>
        <!-- Under `tools` in the config file, edited here because it and
             compaction are the only two things bounding what reaches the
             context, and they interact. -->
        <label class="field">
          <span class="label">工具返回硬上限（字节）</span>
          <input v-model="form.max_result_bytes" class="input" type="number" min="0" step="1000"
            placeholder="留空或 0 = 不限制" />
          <span class="tiny muted">0 = 不限制（推荐）</span>
        </label>
      </div>
      <details class="more">
        <summary class="tiny muted">了解更多：token 上限与字节上限的区别</summary>
        <p class="tiny muted">
          token 上限只在超出历史预算时才动手，从旧到新，保护当前这轮；字节上限每次调用都切，切的正是最新那个结果。
          把返回从中间切断后模型往往无法使用、会换参数重试，而每次重试都要重发整段历史——省下的字节远不及重试的代价。
        </p>
      </details>
      <p v-if="hist?.context_unguarded" class="tiny warn">
        压缩已关闭且工具返回不限制，没有任何东西约束进入上下文的内容；一个足够大的返回会直接让请求失败。二者至少开启一个。
      </p>
      <p v-if="!searchEnabled && !off" class="tiny warn">
        L2 会话检索未开启，被压缩丢弃的早期对话将无法找回。可在 <RouterLink to="/memory">记忆</RouterLink> 页开启。
      </p>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div class="hist-actions">
        <button class="btn btn-primary" :disabled="saving" @click="save">
          <span v-if="saving" class="spinner" /> 保存压缩设置
        </button>
      </div>
    </div>
  </div>
</template>

<style scoped>
.hist-card { padding: 0; }
.hist-head { display: flex; align-items: center; gap: var(--sp-2); width: 100%; padding: var(--sp-3) var(--sp-4); background: none; border: 0; color: var(--text); cursor: pointer; text-align: left; }
.hist-title { font-weight: 600; font-size: 14px; }
.hist-badge { margin-left: auto; font-size: 11px; padding: 2px 8px; border-radius: var(--radius-sm); color: var(--accent); background: var(--accent-tint); border: 1px solid var(--accent-border); }
.hist-badge.off { color: var(--text-dim); background: none; border-color: var(--border); }
.caret { display: inline-block; font-size: 10px; color: var(--text-dim); transition: transform 0.15s ease; }
.caret.open { transform: rotate(90deg); }
.hist-body { padding: 0 var(--sp-4) var(--sp-4); border-top: 1px solid var(--border); }
.hint { margin: var(--sp-3) 0 0; font-size: 12px; line-height: 1.6; }
.hist-grid { display: grid; grid-template-columns: 1fr 1fr; gap: var(--sp-3); margin-top: var(--sp-3); }
.field { display: flex; flex-direction: column; gap: var(--sp-2); }
.label { font-size: 12px; color: var(--text-dim); }
.tiny { font-size: 11px; line-height: 1.5; }
.more { margin-top: var(--sp-3); }
.more summary { cursor: pointer; }
.more p { margin: var(--sp-2) 0 0; line-height: 1.7; }
.warn { margin: var(--sp-3) 0 0; color: var(--warning); }
.error-bar, .notice-bar { display: flex; align-items: center; gap: var(--sp-2); margin-top: var(--sp-3); padding: var(--sp-2) var(--sp-3); border-radius: var(--radius-sm); font-size: 13px; }
.error-bar { background: var(--danger-tint); color: var(--danger); }
.notice-bar { color: var(--accent); background: var(--accent-tint); }
.hist-actions { display: flex; justify-content: flex-end; margin-top: var(--sp-3); }
@media (max-width: 640px) { .hist-grid { grid-template-columns: 1fr; } }
</style>
