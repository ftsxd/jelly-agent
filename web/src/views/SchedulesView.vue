<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'
const tasks=ref([]),runs=ref([]),error=ref(''),form=ref({name:'',cron:'0 9 * * 1-5',prompt:'',agent:'',provider:'',skill:'',enabled:true})
async function load(){try{tasks.value=(await api.schedules()).schedules||[];runs.value=(await api.scheduleRuns()).runs||[]}catch(e){error.value=e.message}}
async function save(){error.value='';try{await api.saveSchedule(form.value);form.value={name:'',cron:'0 9 * * 1-5',prompt:'',agent:'',provider:'',skill:'',enabled:true};await load()}catch(e){error.value=e.message}}
function edit(t){form.value={...t}}
async function del(n){if(confirm(`删除周期任务 ${n}？`)){await api.deleteSchedule(n);await load()}}
async function run(n){try{await api.runSchedule(n);await load()}catch(e){error.value=e.message}}
onMounted(load)
</script>
<template>
  <div class="view">
    <header class="topbar">
      <div class="topbar-l">
        <h1>定时任务</h1>
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
          <b>{{ r.task }}</b> · {{ r.status }} · <span class="dim">{{ new Date(r.started_at).toLocaleString() }}</span>
          <pre v-if="r.output || r.error" class="mono">{{ r.error || r.output }}</pre>
        </div>
        <p v-if="!runs.length" class="muted">暂无执行记录。</p>
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
