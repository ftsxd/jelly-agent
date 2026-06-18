<script setup>
import { onMounted, reactive, ref } from 'vue'
import Icon from '../components/Icon.vue'
import { api } from '../api'

const skills = ref([])
const dir = ref('')
const allowScripts = ref(false)
const loading = ref(true)
const error = ref('')
const notice = ref('')

const editing = ref(false) // false | 'new' | name
const saving = ref(false)
const form = reactive({ name: '', description: '', body: '', enabled: true, varKeys: [], scripts: [], varsText: '', savingVars: false })

const fileInput = ref(null)
const uploading = ref(false)

onMounted(load)

async function load() {
  loading.value = true
  error.value = ''
  try {
    const res = await api.skills()
    skills.value = res.skills
    dir.value = res.dir
    allowScripts.value = !!res.allow_scripts
  } catch (e) {
    error.value = e.message
  } finally {
    loading.value = false
  }
}

function startNew() {
  editing.value = 'new'
  Object.assign(form, { name: '', description: '', body: '', enabled: true, varKeys: [], scripts: [], varsText: '', savingVars: false })
}

async function startEdit(s) {
  error.value = ''
  try {
    const full = await api.skill(s.name) // fetch body + var_keys + scripts
    editing.value = s.name
    Object.assign(form, {
      name: full.name,
      description: full.description || '',
      body: full.body || '',
      enabled: full.enabled,
      varKeys: full.var_keys || [],
      scripts: full.scripts || [],
      // prefill existing keys with blank values (blank = keep server-side)
      varsText: (full.var_keys || []).map((k) => `${k}=`).join('\n'),
      savingVars: false,
    })
  } catch (e) {
    error.value = e.message
  }
}

function parseVars(text) {
  const out = {}
  for (const line of text.split('\n')) {
    const t = line.trim()
    if (!t) continue
    const i = t.indexOf('=')
    if (i < 0) continue
    out[t.slice(0, i).trim()] = t.slice(i + 1).trim()
  }
  return out
}

async function saveVars() {
  if (form.savingVars) return
  form.savingVars = true
  error.value = ''
  try {
    const res = await api.setSkillVars(form.name, parseVars(form.varsText))
    form.varKeys = res.var_keys || []
    form.varsText = form.varKeys.map((k) => `${k}=`).join('\n')
    notice.value = '变量已保存（密钥已脱敏存储）'
  } catch (e) {
    error.value = e.message
  } finally {
    form.savingVars = false
  }
}

async function removeVar(key) {
  try {
    await api.deleteSkillVar(form.name, key)
    form.varKeys = form.varKeys.filter((k) => k !== key)
    form.varsText = form.varKeys.map((k) => `${k}=`).join('\n')
  } catch (e) {
    error.value = e.message
  }
}

async function toggleAllowScripts() {
  try {
    const res = await api.setAllowScripts(!allowScripts.value)
    allowScripts.value = !!res.allow_scripts
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

function pickZip() {
  fileInput.value?.click()
}

async function onZipPicked(e) {
  const file = e.target.files?.[0]
  e.target.value = '' // allow re-picking the same file
  if (!file) return
  uploading.value = true
  error.value = ''
  try {
    const res = await api.uploadSkill(file)
    notice.value = `已导入技能「${res.name}」`
    await load()
  } catch (err) {
    error.value = err.message
  } finally {
    uploading.value = false
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
      <div class="topbar-r">
        <input ref="fileInput" type="file" accept=".zip" class="hidden-file" @change="onZipPicked" />
        <button class="btn" @click="pickZip" :disabled="uploading">
          <span v-if="uploading" class="spinner" /><Icon v-else name="plug" :size="16" /> 上传 ZIP
        </button>
        <button class="btn btn-primary" @click="startNew" :disabled="editing === 'new'">
          <Icon name="plus" :size="16" /> 新建技能
        </button>
      </div>
    </header>

    <div class="body">
      <div v-if="notice" class="notice-bar"><Icon name="check" :size="16" /> {{ notice }}</div>
      <div v-if="error" class="error-bar"><Icon name="alert" :size="16" /> {{ error }}</div>
      <p class="muted hint">
        每个技能 = 一段 Markdown 指令。Agent 平时只看到技能的名称与描述，需要时调用 <code class="mono">use_skill</code> 拉取完整步骤再执行。
      </p>

      <div class="card scripts-toggle">
        <div class="st-text">
          <div class="st-title">允许脚本执行（<code class="mono">run_script</code>）</div>
          <div class="st-warn">⚠️ 开启后 Agent 可运行技能附带的脚本——以你的权限执行代码，仅对信任的技能开启。技能变量（密钥）只注入脚本环境，不进对话。</div>
        </div>
        <button class="switch" :class="{ on: allowScripts }" role="switch" :aria-checked="allowScripts" @click="toggleAllowScripts">
          <span class="switch-knob" />
        </button>
      </div>

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

        <!-- variables + scripts (existing skills only) -->
        <div v-if="editing !== 'new'" class="vars-box">
          <div class="vars-head">变量（密钥脱敏，作为脚本环境变量；值不显示、不进对话）</div>
          <div v-if="form.varKeys.length" class="var-chips">
            <span v-for="k in form.varKeys" :key="k" class="badge mono var-chip">
              {{ k }} <button class="chip-x" title="删除变量" @click="removeVar(k)">✕</button>
            </span>
          </div>
          <textarea v-model="form.varsText" class="textarea mono vars-text" rows="3" placeholder="KEY=VALUE，每行一个；已有变量留空值即保留，填新值则更新&#10;例如：API_TOKEN=sk-xxxx" />
          <div class="vars-actions">
            <span v-if="form.scripts.length" class="muted scripts-list mono">脚本：{{ form.scripts.join('、') }}</span>
            <span v-else class="muted scripts-list">（无脚本；ZIP 导入的技能可附带脚本）</span>
            <button class="btn btn-mini" @click="saveVars" :disabled="form.savingVars">
              <span v-if="form.savingVars" class="spinner" /> 保存变量
            </button>
          </div>
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
.topbar-r {
  display: flex;
  gap: var(--sp-2);
}
.hidden-file {
  display: none;
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

.scripts-toggle {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-4);
  padding: var(--sp-3) var(--sp-4);
}
.st-title {
  font-weight: 600;
  font-size: 14px;
}
.st-warn {
  font-size: 12px;
  color: var(--warning);
  margin-top: 2px;
}
.switch {
  position: relative;
  flex-shrink: 0;
  width: 40px;
  height: 22px;
  border-radius: 999px;
  border: 1px solid var(--border);
  background: var(--surface-3);
  cursor: pointer;
  padding: 0;
  transition: background 0.18s ease, border-color 0.18s ease;
}
.switch.on {
  background: var(--accent);
  border-color: var(--accent);
}
.switch-knob {
  position: absolute;
  top: 2px;
  left: 2px;
  width: 16px;
  height: 16px;
  border-radius: 50%;
  background: #fff;
  transition: transform 0.18s ease;
}
.switch.on .switch-knob {
  transform: translateX(18px);
}

.vars-box {
  border-top: 1px solid var(--border);
  margin-top: var(--sp-4);
  padding-top: var(--sp-4);
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}
.vars-head {
  font-size: 12px;
  color: var(--text-dim);
}
.var-chips {
  display: flex;
  flex-wrap: wrap;
  gap: var(--sp-1);
}
.var-chip {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.chip-x {
  border: none;
  background: transparent;
  color: var(--text-muted);
  cursor: pointer;
  font-size: 11px;
  padding: 0;
}
.chip-x:hover {
  color: var(--danger);
}
.vars-text {
  width: 100%;
}
.vars-actions {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--sp-3);
}
.scripts-list {
  font-size: 12px;
}
.btn-mini {
  padding: 2px var(--sp-2);
  font-size: 12px;
  display: inline-flex;
  align-items: center;
  gap: 4px;
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
