<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'
const blank = () => ({ name: '', cron: '0 9 * * 1-5', prompt: '', agent: '', provider: '', skill: '', enabled: true, retry_count: 0, retry_delay_sec: 60 })
const tasks = ref([]), runs = ref([]), error = ref(''), form = ref(blank())
const agents = ref([]), providers = ref([])
async function load() {
  try {
    tasks.value = (await api.schedules()).schedules || []
    runs.value = (await api.scheduleRuns()).runs || []
  } catch (e) { error.value = e.message }
  // Choices only; the form still saves whatever name a task already names.
  api.agents().then(r => { agents.value = (r.agents || []).map(a => a.name) }).catch(() => {})
  api.providers().then(r => { providers.value = (r.providers || []).map(p => p.name) }).catch(() => {})
}
async function save() {
  error.value = ''
  try {
    await api.saveSchedule({ ...form.value, retry_count: Number(form.value.retry_count) || 0, retry_delay_sec: Number(form.value.retry_delay_sec) || 0 })
    form.value = blank(); await load()
  } catch (e) { error.value = e.message }
}
function edit(t) { form.value = { ...blank(), ...t } }
async function del(n) { if (confirm(`删除周期任务 ${n}？`)) { await api.deleteSchedule(n); await load() } }
async function run(n) { try { await api.runSchedule(n); await load() } catch (e) { error.value = e.message } }
// A run with its session and round opens in 执行记录, where its steps are.
const recordLink = (r) => r.session_id && r.invocation_id ? { path: '/tasks', query: { id: `${r.session_id}/${r.invocation_id}` } } : null
const statusName = { success: '成功', succeeded: '成功', failed: '失败', error: '失败', running: '运行中' }
onMounted(load)
</script>
<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>周期任务</h1>
        <span class="muted sub">使用标准 Cron 表达式；每次执行都会记录结果。</span>
      </div>
      <button class="btn" @click="load">刷新</button>
    </header>
    <div class="body">
      <div v-if="error" class="error-bar">{{ error }}</div>
      <section class="card panel">
        <h2 class="form-title">{{ form.name ? '编辑任务' : '新建任务' }}</h2>
        <form @submit.prevent="save">
          <label>名称<input v-model="form.name" class="input" required placeholder="weekday-report" /></label>
          <label>Cron<input v-model="form.cron" class="input mono" required /></label>
          <label>任务提示<textarea v-model="form.prompt" class="textarea" required rows="3" /></label>
          <label>Skill（可选）<input v-model="form.skill" class="input" placeholder="weekly-report" /></label>
          <label class="check"><input v-model="form.enabled" type="checkbox" /> 启用</label>
          <details class="advanced" :open="!!(form.agent || form.provider || form.retry_count)">
            <summary>高级：Agent、Provider、失败重试</summary>
            <div class="grid2">
              <label>Agent<select v-model="form.agent" class="input"><option value="">默认 Agent</option><option v-for="a in agents" :key="a" :value="a">{{ a }}</option><option v-if="form.agent && !agents.includes(form.agent)" :value="form.agent">{{ form.agent }}</option></select></label>
              <label>Provider<select v-model="form.provider" class="input"><option value="">默认 Provider</option><option v-for="p in providers" :key="p" :value="p">{{ p }}</option><option v-if="form.provider && !providers.includes(form.provider)" :value="form.provider">{{ form.provider }}</option></select></label>
              <label>失败重试次数<input v-model="form.retry_count" class="input" type="number" min="0" max="10" /></label>
              <label>重试间隔（秒）<input v-model="form.retry_delay_sec" class="input" type="number" min="0" max="3600" /></label>
            </div>
          </details>
          <div class="form-actions"><button class="btn btn-primary">保存</button></div>
        </form>
      </section>
      <section class="card panel">
        <h2 class="form-title">已配置任务</h2>
        <div v-for="t in tasks" :key="t.name" class="row">
          <div class="row-main">
            <div class="row-head"><b>{{ t.name }}</b><span class="mono dim cron">{{ t.cron }}</span></div>
            <p class="dim">{{ t.prompt }}</p>
          </div>
          <div class="row-actions">
            <button class="btn" @click="run(t.name)">立即执行</button>
            <button class="btn" @click="edit(t)">编辑</button>
            <button class="btn danger" @click="del(t.name)">删除</button>
          </div>
        </div>
        <p v-if="!tasks.length" class="muted">暂无周期任务。</p>
      </section>
      <section class="card panel">
        <h2 class="form-title">执行历史</h2>
        <div v-for="r in runs" :key="r.id" class="run">
          <b>{{ r.task }}</b> · {{ statusName[r.status] || r.status }} · <span class="dim">{{ new Date(r.started_at).toLocaleString() }}</span>
          <RouterLink v-if="recordLink(r)" :to="recordLink(r)" class="record-link">查看执行步骤</RouterLink>
          <pre v-if="r.error" class="mono">{{ r.error }}</pre>
          <details v-else-if="!recordLink(r) && r.output"><summary class="dim">输出</summary><pre class="mono">{{ r.output }}</pre></details>
        </div>
        <p v-if="!runs.length" class="muted">暂无执行记录。</p>
      </section>
    </div>
  </div>
</template>
<style scoped>
.advanced summary { cursor: pointer; font-size: 13px; color: var(--text-dim); }
.advanced[open] summary { margin-bottom: var(--sp-3); }
.grid2 { display: grid; grid-template-columns: 1fr 1fr; gap: var(--sp-3); }
.record-link { margin-left: var(--sp-2); font-size: 12px; }
@media (max-width: 640px) { .grid2 { grid-template-columns: 1fr; } }
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
.panel {
  padding: var(--sp-4) var(--sp-5);
}
.form-title {
  font-size: 15px;
  margin: 0 0 var(--sp-4);
}
form {
  display: grid;
  gap: var(--sp-3);
}
label {
  display: grid;
  gap: var(--sp-2);
  font-size: 13px;
  color: var(--text-dim);
}
.check {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  cursor: pointer;
}
.form-actions {
  display: flex;
  justify-content: flex-end;
}
.row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
  padding: var(--sp-3) 0;
  border-bottom: 1px solid var(--border);
}
.row:last-child {
  border-bottom: none;
}
.row-main {
  min-width: 0;
}
.row-head {
  display: flex;
  align-items: baseline;
  gap: var(--sp-3);
}
.cron {
  font-size: 12px;
}
.row-main p {
  margin: 4px 0 0;
  font-size: 13px;
}
.row-actions {
  display: flex;
  gap: var(--sp-2);
  flex-shrink: 0;
}
.btn.danger:hover {
  border-color: var(--danger);
  color: var(--danger);
}
.run {
  padding: var(--sp-3) 0;
  border-bottom: 1px solid var(--border);
  font-size: 13px;
}
.run:last-child {
  border-bottom: none;
}
pre {
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-dim);
  font-size: 12px;
  background: var(--surface-2);
  border: 1px solid var(--border);
  border-radius: var(--radius-sm);
  padding: var(--sp-2) var(--sp-3);
  margin: var(--sp-2) 0 0;
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
</style>
