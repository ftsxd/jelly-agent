<script setup>
import { ref } from 'vue'
import { api } from '../api'

// The base instruction: what a turn runs with when no agent is chosen, and
// what every agent without its own instruction falls back to. It lives with
// the agents because that is where the per-agent instructions are edited.
const open = ref(false)
const loaded = ref(false)
const draft = ref('')
const saving = ref(false)
const error = ref('')
const notice = ref('')

async function load() {
  error.value = ''
  try {
    const p = await api.prompt('', '')
    draft.value = (p?.parts || []).find((x) => x.name === '指令')?.text || ''
    loaded.value = true
  } catch (e) {
    error.value = e.message
  }
}

function toggle(e) {
  open.value = e.target.open
  if (open.value && !loaded.value) load()
}

async function save() {
  if (saving.value) return
  saving.value = true
  error.value = ''
  notice.value = ''
  try {
    const r = await api.saveInstruction(draft.value)
    notice.value = `已保存到 ${r.saved_to}（已热重载）`
    if (typeof r.instruction === 'string') draft.value = r.instruction
  } catch (e) {
    error.value = e.message
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <details class="card base-instruction" :open="open" @toggle="toggle">
    <summary>
      <span class="title">基础指令</span>
      <span class="muted tiny">未选择 Agent 时使用；Agent 的「系统指令」留空时也沿用它</span>
    </summary>
    <div class="body">
      <textarea v-model="draft" class="textarea mono" rows="8" placeholder="留空恢复内置默认" :disabled="!loaded" />
      <div v-if="error" class="error-bar">{{ error }}</div>
      <div v-if="notice" class="notice-bar">{{ notice }}</div>
      <div class="actions">
        <span class="muted tiny">每轮都会随请求发出，占用固定 token；构成见「用量统计」页。</span>
        <button class="btn btn-primary" :disabled="saving || !loaded" @click="save">
          <span v-if="saving" class="spinner" /> 保存基础指令
        </button>
      </div>
    </div>
  </details>
</template>

<style scoped>
.base-instruction { padding: var(--sp-3) var(--sp-4); }
summary { cursor: pointer; display: flex; align-items: baseline; gap: var(--sp-3); flex-wrap: wrap; }
.title { font-weight: 600; font-size: 14px; }
.tiny { font-size: 12px; }
.body { display: grid; gap: var(--sp-3); margin-top: var(--sp-3); }
.actions { display: flex; align-items: center; justify-content: space-between; gap: var(--sp-3); flex-wrap: wrap; }
.error-bar { color: var(--danger); font-size: 13px; }
.notice-bar { color: var(--accent); font-size: 13px; }
</style>
