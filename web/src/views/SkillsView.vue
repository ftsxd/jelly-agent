<script setup>
import { onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const skills = ref([])
const dir = ref('')
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false) // false | 'new' | name
const saving = ref(false)
const form = reactive({ name: '', description: '', body: '', enabled: true })

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.skills()
    skills.value = res.skills
    dir.value = res.dir
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startNew() {
  editing.value = 'new'
  Object.assign(form, { name: '', description: '', body: '', enabled: true })
}

async function startEdit(s) {
  error.value = ''
  try {
    const full = await api.skill(s.name) // fetch body
    editing.value = s.name
    Object.assign(form, { name: full.name, description: full.description || '', body: full.body || '', enabled: full.enabled })
  } catch (e) {
    error.value = e.message
  }
}

function cancel() {
  editing.value = false
  error.value = ''
}

async function submit() {
  error.value = ''
  saving.value = true
  try {
    const res = await api.saveSkill({
      name: form.name.trim(),
      description: form.description.trim(),
      body: form.body,
      enabled: form.enabled,
    })
    notice.value = `已保存到 ${res.saved_to}`
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
    const full = await api.skill(s.name)
    await api.saveSkill({ name: full.name, description: full.description, body: full.body, enabled: !full.enabled })
    await load()
  } catch (e) {
    error.value = e.message
  }
}

async function remove(s) {
  if (!confirm(`确认删除技能「${s.name}」？`)) return
  try {
    await api.deleteSkill(s.name)
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
        <h1>技能</h1>
        <span class="muted sub">Agent Skills · 按需加载的能力包</span>
      </div>
      <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
        <Icon name="plus" :size="16" /> 新建技能
      </button>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
      <p class="muted hint">
        每个技能 = 一段 Markdown 指令。Agent 平时只看到技能的名称与描述，需要时调用 <code class="mono">use_skill</code> 拉取完整步骤再执行。
      </p>

      <!-- create / edit form -->
      <div v-if="editing" class="card form">
        <h2 class="form-title">{{ editing === 'new' ? '新建技能' : `编辑 ${editing}` }}</h2>
        <div class="grid">
          <label class="field">
            <span class="label">名称（标识符，字母/数字/-/_）*</span>
            <input v-model="form.name" class="input mono" :disabled="editing !== 'new'" placeholder="weekly-report" />
          </label>
          <label class="field">
            <span class="check-wrap">
              <input type="checkbox" v-model="form.enabled" /> 启用（进入技能清单）
            </span>
          </label>
          <label class="field span2">
            <span class="label">描述 *（进清单，供 Agent 判断何时使用）</span>
            <input v-model="form.description" class="input" placeholder="把本周事项整理成结构化中文周报" />
          </label>
          <label class="field span2">
            <span class="label">技能正文（Markdown，调用 use_skill 时返回）</span>
            <textarea v-model="form.body" class="textarea mono" rows="12" placeholder="## 步骤&#10;1. ...&#10;2. ...&#10;&#10;## 输出模板&#10;..." />
          </label>
        </div>
        <div class="form-actions">
          <button class="btn" @click="cancel" :disabled="saving">取消</button>
          <button class="btn btn-primary" @click="submit" :disabled="saving">
            <span v-if="saving" class="spinner" /> 保存
          </button>
        </div>
      </div>

      <!-- skill list -->
      <div v-if="loading" class="empty"><span class="spinner" /></div>
      <div v-else-if="!skills.length && !editing" class="empty">
        <Icon name="book" :size="32" />
        <div>
          <p style="margin: 0 0 4px">尚无技能</p>
          <p class="muted" style="margin: 0; font-size: 13px">新建一个技能，Agent 即可按需加载它的步骤</p>
        </div>
      </div>
      <div v-else class="list">
        <div v-for="s in skills" :key="s.name" class="card skill" :class="{ off: !s.enabled }">
          <div class="skill-main">
            <div class="skill-head">
              <span class="mono skill-name">{{ s.name }}</span>
              <span class="badge" :class="s.enabled ? 'badge-accent' : ''">{{ s.enabled ? '已启用' : '已停用' }}</span>
            </div>
            <div class="skill-desc">{{ s.description }}</div>
          </div>
          <div class="skill-actions">
            <button class="btn" @click="toggle(s)" :title="s.enabled ? '停用' : '启用'"><Icon name="power" :size="15" /></button>
            <button class="btn" @click="startEdit(s)" title="编辑"><Icon name="settings" :size="15" /></button>
            <button class="btn danger" @click="remove(s)" title="删除"><Icon name="trash" :size="15" /></button>
          </div>
        </div>
        <p v-if="dir" class="muted dir mono">{{ dir }}</p>
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
  margin: 0;
}
.hint code {
  color: var(--primary);
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
  justify-content: flex-end;
}
.span2 {
  grid-column: 1 / -1;
}
.label {
  font-size: 12px;
  color: var(--text-dim);
}
.check-wrap {
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
.skill {
  padding: var(--sp-3) var(--sp-4);
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
}
.skill.off {
  opacity: 0.6;
}
.skill-main {
  min-width: 0;
}
.skill-head {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}
.skill-name {
  font-weight: 600;
}
.skill-desc {
  font-size: 13px;
  color: var(--text-dim);
  margin-top: 2px;
}
.skill-actions {
  display: flex;
  gap: var(--sp-2);
  flex-shrink: 0;
}
.btn.danger:hover {
  border-color: var(--danger);
  color: var(--danger);
}
.dir {
  font-size: 11px;
  margin: var(--sp-2) 0 0;
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
