<script setup>
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { api } from '../api'
import Icon from './Icon.vue'

const projects = ref([])
const agents = ref([])
const loading = ref(true)
const error = ref('')
const notice = ref('')
const saving = ref(false)
const syncing = reactive(new Set())
const editing = ref(null)
const assigning = ref(null)
const deleting = ref(null)
const query = ref('')
const blank = { id: '', name: '', url: '', branch: 'main', token_env: '', username: '', token: '', clear_token: false }
const form = reactive({ ...blank })
// dirs[0] is always the main analysis directory; the rest are references.
const dirs = ref([{ path: '.', name: '', description: '' }])
const chosenAgent = ref('')
const tokenSaved = ref(false)
const checking = reactive(new Set())
const remote = reactive({})   // project id -> { behind, remote_revision } | { error }
const labeling = ref(null)    // project id whose annotation catalogue is open
const labelQuery = ref('')
const labelDraft = reactive({ path: '', name: '', description: '', tags: '' })
const confirmingAnnotate = ref(null)
let poll
let disposed = false

// A project stuck in "queued"/"running" after a restart is marked interrupted by
// the server, so the page never has to guess whether a pull is still alive.
const STATES = {
  queued: '排队中', running: '拉取代码中', succeeded: '代码已就绪',
  failed: '最近同步失败', interrupted: '同步中断，可重试',
}
const visible = computed(() => projects.value.filter(p => `${p.name} ${p.id} ${p.url}`.toLowerCase().includes(query.value.toLowerCase())))
const ready = computed(() => projects.value.filter(p => p.synced_at && !p.dir_issues?.length).length)
const fmt = date => date ? new Date(date).toLocaleString('zh-CN', { hour12: false }) : '尚未同步'
// Nothing syncs automatically, so how old the snapshot is belongs on the card
// next to the commit — not only in a timestamp the reader has to subtract from.
function age(date) {
  if (!date) return ''
  const mins = Math.floor((Date.now() - Date.parse(date)) / 60000)
  if (mins < 1) return '刚刚'
  if (mins < 60) return `${mins} 分钟前`
  if (mins < 1440) return `${Math.floor(mins / 60)} 小时前`
  return `${Math.floor(mins / 1440)} 天前`
}
const stale = p => p.synced_at && Date.now() - Date.parse(p.synced_at) > 7 * 86400000
// One ls-remote, no download — "am I current" is a much cheaper question than
// "make me current", which for a real monorepo means re-fetching hundreds of MB.
async function checkUpdate(p) {
  if (checking.has(p.id)) return
  checking.add(p.id); delete remote[p.id]
  try { remote[p.id] = await api.checkCodeProjectUpdate(p.id) }
  catch (e) { remote[p.id] = { error: e.message } }
  finally { checking.delete(p.id) }
}
const busy = p => p.syncing || syncing.has(p.id) || p.sync_state === 'queued' || p.sync_state === 'running'
const assignedTo = p => (p.grants || []).filter(g => !g.expires_at || Date.parse(g.expires_at) > Date.now()).map(g => g.agent)
function state(p) {
  if (busy(p)) return STATES[p.sync_state] || '排队中'
  if (p.sync_state && STATES[p.sync_state]) return STATES[p.sync_state]
  return p.synced_at ? '代码已就绪' : '待同步'
}
function tone(p) {
  if (busy(p)) return ''
  if (p.last_error || p.dir_issues?.length) return 'badge-warn'
  return p.synced_at ? 'badge-ok' : ''
}

onMounted(async () => {
  await load()
  if (disposed) return
  poll = setInterval(() => { if (!disposed) refresh().catch(() => {}) }, 5000)
})
onUnmounted(() => { disposed = true; clearInterval(poll) })
async function refresh() {
  const result = await api.codeProjects()
  if (!disposed) projects.value = result.projects || []
}
async function load() {
  loading.value = true; error.value = ''
  try {
    const [result, ag] = await Promise.all([api.codeProjects(), api.agents()])
    if (disposed) return
    projects.value = result.projects || []
    agents.value = ag.agents || []
    if (!agents.value.some(a => a.name === 'root')) agents.value = [{ name: 'root', description: '默认单 Agent', enabled: true }, ...agents.value]
  } catch (e) { error.value = e.message }
  finally { loading.value = false }
}
function edit(project = null) {
  editing.value = project ? project.id : ''
  // has_token is the only thing the server ever says about a stored token.
  tokenSaved.value = !!project?.has_token
  Object.assign(form, blank, project || {}, { token: '', clear_token: false })
  const meta = project?.directory_metadata || {}
  const paths = [project?.root_path || '.', ...(project?.reference_paths || [])]
  dirs.value = paths.map(path => ({ path, name: meta[path]?.name || '', description: meta[path]?.description || '' }))
  error.value = ''; notice.value = ''; assigning.value = null; deleting.value = null
}
function addDir() { dirs.value.push({ path: '', name: '', description: '' }) }
function removeDir(i) { if (i > 0) dirs.value.splice(i, 1) }
// The payload keeps paths and metadata keys in step; the server normalizes both
// the same way and rejects a description that names an unconfigured directory.
function payload() {
  const rows = dirs.value.map(d => ({ ...d, path: d.path.trim() })).filter((d, i) => i === 0 || d.path)
  const directory_metadata = {}
  for (const d of rows) {
    const name = d.name.trim(), description = d.description.trim()
    if (name || description) directory_metadata[d.path || '.'] = { name, description }
  }
  const body = {
    id: form.id, name: form.name, url: form.url, branch: form.branch,
    token_env: form.token_env, username: form.username,
    root_path: rows[0].path || '.',
    reference_paths: rows.slice(1).map(d => d.path),
    directory_metadata,
  }
  // Send the token only when one was typed: an untouched field must not be
  // read as "clear it", because the field can never be prefilled.
  if (form.token) body.token = form.token
  if (form.clear_token) body.clear_token = true
  return body
}
async function save(thenSync = false) {
  if (saving.value) return
  saving.value = true; error.value = ''
  const id = form.id
  try {
    await api.saveCodeProject(payload())
    editing.value = null
    notice.value = thenSync ? '项目已保存，正在后台同步。' : '项目已保存。同步代码后，把它分配给需要分析的 Agent。'
    if (thenSync) await api.syncCodeProject(id)
    await refresh()
  } catch (e) { error.value = e.message }
  finally { saving.value = false }
}
// Sync no longer waits for the pull: the server returns a task id immediately
// and the 5s poll above reports progress, so closing the page cannot cancel it.
async function sync(project) {
  if (syncing.has(project.id)) return
  syncing.add(project.id); error.value = ''; notice.value = ''
  try {
    await api.syncCodeProject(project.id)
    delete remote[project.id]
    notice.value = `${project.name} 已在后台同步，关闭页面不会中断。`
  } catch (e) { error.value = e.message }
  finally { syncing.delete(project.id); await refresh().catch(e => { error.value = e.message }) }
}
// An agent can ask for a pull but cannot run one. This is where that ask gets
// answered — approving starts exactly the sync the button above would.
async function resolveSyncRequest(project, approve) {
  if (syncing.has(project.id)) return
  syncing.add(project.id); error.value = ''; notice.value = ''
  try {
    if (approve) {
      await api.approveCodeProjectSync(project.id)
      delete remote[project.id]
      notice.value = `${project.name} 已在后台同步，关闭页面不会中断。`
    } else {
      await api.rejectCodeProjectSync(project.id)
      notice.value = `已忽略 ${project.name} 的同步请求。`
    }
  } catch (e) { error.value = e.message }
  finally { syncing.delete(project.id); await refresh().catch(e => { error.value = e.message }) }
}
function openAssign(p) {
  assigning.value = p.id; chosenAgent.value = assignedTo(p)[0] || ''
  editing.value = null; deleting.value = null; error.value = ''; notice.value = ''
}
async function saveAssign() {
  saving.value = true; error.value = ''
  try {
    await api.grantCodeProject(assigning.value, chosenAgent.value ? [{ agent: chosenAgent.value }] : [])
    assigning.value = null; notice.value = '分配已更新，后续代码读取立即按新的关联执行。'; await refresh()
  } catch (e) { error.value = e.message }
  finally { saving.value = false }
}
// The annotation catalogue: business names, descriptions and tags on any
// directory inside the analysis scope. Separate from the project form because a
// monorepo holds one row per service, and resending all of them to edit one is
// how the other 129 quietly disappear.
function openLabels(p) {
  labeling.value = p.id; labelQuery.value = ''
  Object.assign(labelDraft, { path: '', name: '', description: '', tags: '' })
  editing.value = null; assigning.value = null; deleting.value = null; error.value = ''; notice.value = ''
}
function annotationsOf(p) {
  const meta = p.directory_metadata || {}
  const q = labelQuery.value.toLowerCase()
  return Object.entries(meta)
    .map(([path, info]) => ({ path, ...info, tags: info.tags || [] }))
    .filter(a => !q || `${a.path} ${a.name || ''} ${a.description || ''} ${a.tags.join(' ')}`.toLowerCase().includes(q))
    .sort((a, b) => a.path.localeCompare(b.path))
}
function draftsOf(p) {
  return Object.entries(p.directory_drafts || {})
    .map(([path, info]) => ({ path, ...info, tags: info.tags || [] }))
    .sort((a, b) => a.path.localeCompare(b.path))
}
function editLabel(a) {
  Object.assign(labelDraft, { path: a.path, name: a.name || '', description: a.description || '', tags: (a.tags || []).join(', ') })
}
async function saveLabel(id) {
  if (!labelDraft.path.trim()) { error.value = '请填写目录路径'; return }
  saving.value = true; error.value = ''
  try {
    await api.setCodeProjectAnnotation(id, labelDraft.path.trim(), {
      name: labelDraft.name.trim(),
      description: labelDraft.description.trim(),
      tags: labelDraft.tags.split(/[,，]/).map(s => s.trim()).filter(Boolean),
    })
    Object.assign(labelDraft, { path: '', name: '', description: '', tags: '' })
    await refresh()
  } catch (e) { error.value = e.message }
  finally { saving.value = false }
}
async function deleteLabel(id, path) {
  saving.value = true; error.value = ''
  try { await api.setCodeProjectAnnotation(id, path, { name: '', description: '', tags: [] }); await refresh() }
  catch (e) { error.value = e.message }
  finally { saving.value = false }
}
// Agent-written labels are suggestions until a person says otherwise.
async function resolveDrafts(id, body) {
  saving.value = true; error.value = ''
  try {
    const res = await api.resolveCodeProjectDrafts(id, body)
    notice.value = body.accept_all || body.accept ? `已采纳 ${res.accepted} 条草稿。` : '草稿已丢弃。'
    await refresh()
  } catch (e) { error.value = e.message }
  finally { saving.value = false }
}

const annotating = p => p.annotate?.state === 'running'
// Generating labels reads across the whole repository. That is real money, so
// the cost is stated before the click rather than discovered on the bill.
async function startAnnotate(p) {
  saving.value = true; error.value = ''; notice.value = ''
  try {
    const res = await api.annotateCodeProject(p.id)
    confirmingAnnotate.value = null
    notice.value = `已让 ${res.agent} 在后台生成标注，完成后回到「目录标注」审核。关闭页面不会中断。`
    await refresh()
  } catch (e) { error.value = e.message }
  finally { saving.value = false }
}

async function remove(p) {
  saving.value = true; error.value = ''
  try { await api.deleteCodeProject(p.id); deleting.value = null; notice.value = '项目已移除，Agent 无法再读取。'; await refresh() }
  catch (e) { error.value = e.message }
  finally { saving.value = false }
}
</script>

<template>
  <section class="code-projects" aria-labelledby="projects-heading">
    <div class="section-head">
      <div><h2 id="projects-heading">项目代码</h2><p class="muted">配置仓库、同步代码，再把项目分配给负责分析的 Agent。</p></div>
      <button class="btn btn-primary" @click="edit()" :disabled="saving"><Icon name="plus" :size="16" /> 新建项目</button>
    </div>
    <div v-if="error" class="error-bar" role="alert">{{ error }}</div>
    <div v-if="notice" class="notice-bar" role="status">{{ notice }}</div>
    <div class="summary">
      <span><strong>{{ projects.length }}</strong> 个项目</span><span><strong>{{ ready }}</strong> 个已就绪</span>
      <span><Icon name="lock" :size="15" /> 默认不分配 · 只读分析 · 范围限于配置目录</span>
    </div>

    <form v-if="editing !== null" class="card editor" @submit.prevent="save(false)">
      <h3>{{ editing === '' ? '新建代码项目' : '编辑项目' }}</h3>
      <div class="fields">
        <label>项目名称<input class="input" v-model.trim="form.name" required maxlength="100" placeholder="Silkworm 优惠券" /></label>
        <label>项目标识<input class="input mono" v-model.trim="form.id" :disabled="editing !== ''" required pattern="[A-Za-z0-9_-]{1,80}" placeholder="silkworm-coupon" /><small>字母、数字、下划线或连字符，创建后不可修改</small></label>
        <label class="wide">Git 仓库地址<input class="input mono" v-model.trim="form.url" type="url" required placeholder="https://git.example.com/team/silkworm.git" /><small>支持 HTTPS 公有或私有仓库</small></label>
        <label>分支<input class="input mono" v-model.trim="form.branch" required placeholder="main" /></label>
        <label>访问令牌（私有仓库填写）
          <input class="input mono" type="password" v-model="form.token" autocomplete="new-password" :placeholder="tokenSaved ? '已保存，留空则不修改' : '粘贴 Git 平台的访问令牌'" />
          <small class="token-note">
            <template v-if="tokenSaved"><span class="badge badge-ok">已保存令牌</span> 留空即保持不变。<button type="button" class="link-btn" @click="form.clear_token = !form.clear_token">{{ form.clear_token ? '取消清除' : '清除令牌' }}</button><span v-if="form.clear_token" class="warn-text"> 保存后将清除。</span></template>
            <template v-else>公有仓库留空。</template>
            令牌存在服务端仅属主可读的文件里（0600），不写进项目配置，页面和 Agent 都读不回原文。
          </small>
        </label>
        <label>Git 用户名（选填）<input class="input" v-model.trim="form.username" placeholder="oauth2" /><small>使用令牌认证时，按 Git 服务要求填写用户名</small></label>
        <label class="wide">凭据环境变量名（高级，选填）<input class="input mono" v-model.trim="form.token_env" pattern="[A-Za-z_][A-Za-z0-9_]*" title="填变量名（如 CODING_TOKEN），不是令牌本身" placeholder="CODING_TOKEN" /><small>改为从服务端环境变量读取令牌。上面直接填了令牌时以令牌为准。容器部署不建议用这个——<code>docker inspect</code> 能看到环境变量。</small></label>
      </div>

      <h4 class="dirs-title">可分析的目录</h4>
      <p class="muted tiny">同步始终拉取整个仓库；这里决定 Agent 能浏览、搜索和读取哪些目录。业务名称和描述让 Agent 把「订单服务」对应到实际路径，改动无需重新同步。</p>
      <div v-for="(d, i) in dirs" :key="i" class="dir-row">
        <div class="dir-head">
          <span class="badge">{{ i === 0 ? '主分析目录' : '参考目录' }}</span>
          <button v-if="i > 0" class="btn btn-icon" type="button" title="移除" @click="removeDir(i)"><Icon name="trash" :size="15" /></button>
        </div>
        <div class="dir-fields">
          <label>目录路径<input class="input mono" v-model.trim="d.path" :placeholder="i === 0 ? 'services/discount_coupon' : 'common'" /><small v-if="i === 0">仓库内相对路径；留空或填 <code>.</code> 表示整仓</small></label>
          <label>业务名称（选填）<input class="input" v-model.trim="d.name" placeholder="优惠券服务" /></label>
          <label class="wide">业务描述（选填）<input class="input" v-model.trim="d.description" placeholder="负责优惠券发放、核销与过期处理；结算在独立支付服务中。" /></label>
        </div>
      </div>
      <button class="btn" type="button" @click="addDir"><Icon name="plus" :size="15" /> 添加参考目录</button>

      <p class="muted tiny">修改仓库地址或分支需要重新同步；只改目录、名称或描述不需要。</p>
      <div class="actions">
        <button class="btn" type="button" :disabled="saving" @click="editing = null">取消</button>
        <button class="btn" type="submit" :disabled="saving">{{ saving ? '保存中…' : '保存' }}</button>
        <button class="btn btn-primary" type="button" :disabled="saving" @click="save(true)">保存并同步</button>
      </div>
    </form>

    <div class="search-row"><label class="search-label">搜索项目<input class="input" v-model="query" type="search" placeholder="名称、标识或仓库地址" /></label><button class="btn" @click="load" :disabled="loading"><Icon name="refresh" :size="16" /> 刷新</button></div>
    <p v-if="loading && !projects.length" class="empty" role="status">正在加载项目…</p>
    <div v-else-if="!projects.length" class="card empty"><Icon name="code" :size="28" /><h3>把需要分析的项目接进来</h3><p>创建项目、同步代码后，选择负责分析的 Agent。</p><button class="btn" @click="edit()">新建第一个项目</button></div>
    <p v-else-if="!visible.length" class="empty">没有找到匹配的项目。</p>
    <article v-for="p in visible" :key="p.id" class="card project">
      <div class="project-head"><div class="project-title"><h3>{{ p.name }}</h3><code class="muted">{{ p.id }}</code></div><span class="badge" :class="tone(p)">{{ state(p) }}</span></div>
      <p class="repo-url mono">{{ p.url }}</p>
      <div class="meta"><span>分支 <code>{{ p.branch }}</code></span><span v-if="p.revision">版本 <code>{{ p.revision.slice(0, 12) }}</code></span><span :class="{ 'stale-text': stale(p) }">{{ p.synced_at ? `${age(p.synced_at)}同步 · ${fmt(p.synced_at)}` : '尚未同步' }}</span><span>{{ p.has_token ? '已保存访问令牌' : p.token_env ? `凭据取自 ${p.token_env}` : '无凭据' }}</span></div>
      <p v-if="stale(p)" class="stale-note">这份快照已超过一周。代码不会自动同步，分析前建议先点「同步最新代码」。</p>
      <div v-if="remote[p.id]" class="remote-line">
        <template v-if="remote[p.id].error"><span class="sync-error">检查更新失败：{{ remote[p.id].error }}</span></template>
        <template v-else-if="remote[p.id].behind"><span class="badge badge-warn">远端已更新</span> 远端 <code>{{ remote[p.id].remote_revision.slice(0, 12) }}</code>，本地还是 <code>{{ (remote[p.id].local_revision || '').slice(0, 12) || '无' }}</code>，同步一下再分析。</template>
        <template v-else><span class="badge badge-ok">已是最新</span> 与远端 <code>{{ remote[p.id].remote_revision.slice(0, 12) }}</code> 一致。</template>
      </div>
      <div v-if="p.sync_request" class="inline-panel sync-request" role="alert">
        <p><span class="badge badge-warn">待确认</span> <strong>{{ p.sync_request.agent }}</strong> 请求同步这个项目的代码。</p>
        <p class="reason">{{ p.sync_request.reason }}</p>
        <p v-if="p.sync_request.remote_revision" class="muted tiny">
          远端 <code>{{ p.sync_request.remote_revision.slice(0, 12) }}</code>
          <template v-if="p.sync_request.local_revision">，本地 <code>{{ p.sync_request.local_revision.slice(0, 12) }}</code></template>
          · 请求于 {{ fmt(p.sync_request.requested_at) }}
        </p>
        <div class="actions">
          <button class="btn" :disabled="busy(p) || saving" @click="resolveSyncRequest(p, false)">忽略</button>
          <button class="btn btn-primary" :disabled="busy(p) || saving" @click="resolveSyncRequest(p, true)">同意并同步</button>
        </div>
      </div>
      <div class="dir-summary">
        <strong>可分析目录</strong>
        <span class="badge mono" :title="p.directory_metadata?.[p.root_path || '.']?.description">
          {{ p.directory_metadata?.[p.root_path || '.']?.name || '主目录' }} · {{ p.root_path || '.' }}
        </span>
        <span v-for="r in p.reference_paths || []" :key="r" class="badge mono" :title="p.directory_metadata?.[r]?.description">
          {{ p.directory_metadata?.[r]?.name || '参考' }} · {{ r }}
        </span>
      </div>
      <p v-if="p.dir_issues?.length" class="sync-error">配置的目录在最近一次同步的代码里找不到：{{ p.dir_issues.join('、') }}。请编辑项目修正路径。</p>
      <p v-if="p.last_error" class="sync-error">{{ p.last_error }}<span v-if="p.synced_at">；仍可分析上次成功同步的版本。</span></p>
      <div class="grant-summary"><strong>负责分析的 Agent</strong><template v-if="assignedTo(p).length"><span v-for="a in assignedTo(p)" :key="a" class="badge">{{ a }}</span></template><span v-else class="muted">尚未分配</span></div>
      <div class="actions project-actions">
        <button class="btn btn-primary" :disabled="busy(p) || saving" @click="sync(p)">{{ busy(p) ? '同步中…' : p.synced_at ? '同步最新代码' : '首次同步' }}</button>
        <button class="btn" :disabled="checking.has(p.id) || busy(p)" @click="checkUpdate(p)">{{ checking.has(p.id) ? '检查中…' : '检查更新' }}</button>
        <button class="btn" :disabled="saving || annotating(p) || busy(p)" @click="confirmingAnnotate = p.id; labeling = null; assigning = null; deleting = null">{{ annotating(p) ? '生成标注中…' : '自动生成标注' }}</button>
        <button class="btn" :disabled="saving" @click="openLabels(p)">目录标注<span v-if="Object.keys(p.directory_metadata || {}).length" class="count">{{ Object.keys(p.directory_metadata).length }}</span><span v-if="draftsOf(p).length" class="badge badge-warn draft-dot">{{ draftsOf(p).length }} 待审</span></button>
        <button class="btn" :disabled="saving" @click="openAssign(p)">分配 Agent</button>
        <button class="btn" :disabled="busy(p) || saving" @click="edit(p)">编辑</button>
        <button class="btn delete-button" :disabled="busy(p) || saving" @click="deleting = p.id; assigning = null">移除</button>
      </div>
      <div v-if="deleting === p.id" class="inline-panel" role="alert"><p>移除「{{ p.name }}」并收回 Agent 的代码读取权限？</p><div class="actions"><button class="btn" :disabled="saving" @click="deleting = null">取消</button><button class="btn btn-danger" :disabled="saving" @click="remove(p)">确认移除</button></div></div>
      <div v-if="confirmingAnnotate === p.id" class="inline-panel">
        <h4>让 Agent 自动生成目录标注</h4>
        <p class="cost-warn">⚠️ <strong>这会消耗较多 token。</strong>Agent 要通读仓库来判断每个目录是干什么的，服务越多消耗越大——像 Silkworm 这种 130 个服务的仓库，一次跑下来可能是几十万 token 量级。提示词里已经要求它优先用少量批量搜索、而不是逐个翻文件，但具体消耗取决于仓库结构和模型。</p>
        <ul class="cost-list">
          <li>以项目已分配的 Agent 身份运行，读取范围和平时分析完全一样</li>
          <li>产出的是<strong>草稿</strong>，你审核采纳后才生效，采纳前不影响任何分析</li>
          <li>后台执行，关闭页面不会中断；最多跑 30 分钟</li>
          <li>建议先小范围试：把主分析目录临时改成某一个服务，跑通了再放开整仓</li>
        </ul>
        <p v-if="!assignedTo(p).length" class="sync-error">这个项目还没分配 Agent，先分配再生成。</p>
        <div class="actions">
          <button class="btn" :disabled="saving" @click="confirmingAnnotate = null">取消</button>
          <button class="btn btn-primary" :disabled="saving || !assignedTo(p).length" @click="startAnnotate(p)">知道了，开始生成</button>
        </div>
      </div>

      <p v-if="p.annotate && p.annotate.state !== 'running'" class="annotate-result" :class="{ 'sync-error': p.annotate.state === 'failed' }">
        <template v-if="p.annotate.state === 'failed'">标注生成失败：{{ p.annotate.error }}</template>
        <template v-else>标注生成完成{{ p.annotate.proposed ? `，新增 ${p.annotate.proposed} 条草稿待审核` : '，没有新增草稿' }}。</template>
      </p>

      <div v-if="labeling === p.id" class="inline-panel">
        <h4>目录标注：{{ p.name }}</h4>
        <p class="muted">给分析范围内的任意目录写上业务名称、职责和标签。Agent 浏览目录时会看到它们，也能按名称或标签直接定位——不用先读代码猜这个目录是干什么的。标注只是业务背景，不扩大任何读取范围，改动也不需要重新同步。</p>

        <template v-if="draftsOf(p).length">
          <div class="draft-head">
            <strong>{{ draftsOf(p).length }} 条 Agent 草稿待审核</strong>
            <span class="muted">采纳前不会被任何分析读到。</span>
            <button class="btn" :disabled="saving" @click="resolveDrafts(p.id, { accept_all: true })">全部采纳</button>
            <button class="btn" :disabled="saving" @click="resolveDrafts(p.id, { reject: draftsOf(p).map(d => d.path) })">全部丢弃</button>
          </div>
          <ul class="label-list">
            <li v-for="d in draftsOf(p)" :key="'d-' + d.path" class="draft-row">
              <div class="label-main">
                <code class="mono">{{ d.path }}</code>
                <strong v-if="d.name">{{ d.name }}</strong>
                <span v-for="t in d.tags" :key="t" class="badge">{{ t }}</span>
              </div>
              <p v-if="d.description" class="muted label-desc">{{ d.description }}</p>
              <div class="label-actions">
                <button class="btn" :disabled="saving" @click="resolveDrafts(p.id, { accept: [d.path] })">采纳</button>
                <button class="btn" :disabled="saving" @click="resolveDrafts(p.id, { reject: [d.path] })">丢弃</button>
              </div>
            </li>
          </ul>
        </template>

        <form class="label-form" @submit.prevent="saveLabel(p.id)">
          <div class="label-fields">
            <label>目录路径<input class="input mono" v-model.trim="labelDraft.path" placeholder="services/order" /></label>
            <label>业务名称<input class="input" v-model.trim="labelDraft.name" placeholder="订单服务" /></label>
            <label>标签<input class="input" v-model.trim="labelDraft.tags" placeholder="核心链路, 订单" /><small>逗号分隔</small></label>
            <label class="wide">业务描述<input class="input" v-model.trim="labelDraft.description" placeholder="负责下单、查询、取消与状态流转；支付在独立服务中。" /></label>
          </div>
          <div class="actions"><button class="btn btn-primary" :disabled="saving">{{ labelDraft.path && (p.directory_metadata || {})[labelDraft.path] ? '更新这条标注' : '添加标注' }}</button></div>
        </form>

        <label class="search-label">搜索标注<input class="input" v-model="labelQuery" type="search" placeholder="路径、名称、描述或标签" /></label>
        <p v-if="!annotationsOf(p).length" class="muted">{{ labelQuery ? '没有匹配的标注。' : '还没有标注。可以在这里逐条添加，或者让已分配的 Agent 扫描仓库提交草稿。' }}</p>
        <ul v-else class="label-list">
          <li v-for="a in annotationsOf(p)" :key="a.path">
            <div class="label-main">
              <code class="mono">{{ a.path }}</code>
              <strong v-if="a.name">{{ a.name }}</strong>
              <span v-for="t in a.tags" :key="t" class="badge">{{ t }}</span>
            </div>
            <p v-if="a.description" class="muted label-desc">{{ a.description }}</p>
            <div class="label-actions">
              <button class="btn" :disabled="saving" @click="editLabel(a)">编辑</button>
              <button class="btn delete-button" :disabled="saving" @click="deleteLabel(p.id, a.path)">删除</button>
            </div>
          </li>
        </ul>
        <div class="actions"><button class="btn" @click="labeling = null">关闭</button></div>
      </div>

      <form v-if="assigning === p.id" class="inline-panel" @submit.prevent="saveAssign">
        <h4>分配 Agent：{{ p.name }} / 只读分析</h4><p class="muted">被分配的 Agent 只能读取上面列出的目录，子 Agent 需要单独分配。取消分配不会清除已经生成的分析内容。</p>
        <div class="grant-add"><label>负责 Agent<select class="input" v-model="chosenAgent"><option value="">不分配</option><option v-for="a in agents" :key="a.name" :value="a.name">{{ a.name }}{{ a.enabled === false ? '（已禁用）' : '' }}</option></select></label></div>
        <div class="actions"><button type="button" class="btn" :disabled="saving" @click="assigning = null">取消</button><button class="btn btn-primary" :disabled="saving">{{ saving ? '保存中…' : '保存分配' }}</button></div>
      </form>
    </article>
  </section>
</template>

<style scoped>
.code-projects { margin-bottom: 24px; }
h2, h3, h4, p { margin: 0; }
h2 { font-size: 18px; } h3 { font-size: 16px; } h4 { font-size: 14px; }
.section-head, .project-head, .project-title, .actions, .search-row { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; }
.section-head, .project-head { justify-content: space-between; }
.section-head p { margin-top: 6px; }
.summary { display: flex; gap: 24px; flex-wrap: wrap; margin: 20px 0; font-size: 13px; color: var(--text-dim); }
.summary span { display: inline-flex; align-items: center; gap: 6px; }
.summary strong { font-size: 20px; color: var(--text); }
.card { margin: 12px 0; padding: 20px; }
.fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; margin: 20px 0; }
label { display: flex; flex-direction: column; gap: 8px; font-size: 13px; min-width: 0; }
small { font-size: 12px; color: var(--text-dim); }
.wide { grid-column: 1 / -1; }
.input { width: 100%; min-height: 40px; }
.actions { justify-content: flex-end; margin-top: 16px; }
.search-row { margin: 20px 0 16px; align-items: end; }
.search-label { flex: 1; }
.repo-url { margin: 12px 0; overflow-wrap: anywhere; color: var(--text-dim); font-size: 13px; }
.meta { display: flex; gap: 16px; flex-wrap: wrap; font-size: 12px; color: var(--text-dim); }
.sync-request .reason { margin: 6px 0; }
.sync-request .actions { margin-top: 12px; }
.grant-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: 18px; font-size: 13px; }
.grant-summary strong { font-weight: 500; margin-right: 4px; }
.project-actions { justify-content: flex-start; }
.delete-button { margin-left: auto; color: var(--danger); }
.inline-panel { background: var(--surface-2); padding: 16px; border: 1px solid var(--border); border-radius: 8px; margin-top: 16px; }
.inline-panel p { margin: 8px 0 12px; line-height: 1.6; font-size: 13px; }
.grant-list { list-style: none; margin: 0; padding: 0; }
.grant-list li { display: flex; align-items: center; gap: 12px; flex-wrap: wrap; padding: 8px 0; font-size: 13px; border-bottom: 1px solid var(--border); }
.grant-list li span { flex: 1; color: var(--text-dim); }
.grant-add { display: flex; align-items: end; gap: 12px; flex-wrap: wrap; margin-top: 16px; }
.dirs-title { margin-top: 20px; }
.token-note { line-height: 1.7; }
.token-note code { font-size: 11px; }
.link-btn { background: none; border: none; padding: 0; color: var(--accent); cursor: pointer; font-size: 12px; text-decoration: underline; }
.warn-text { color: var(--warning); }
.tiny { font-size: 12px; margin: 6px 0 12px; }
.dir-row { border: 1px solid var(--border); border-radius: 8px; padding: 12px; margin-bottom: 12px; }
.dir-head { display: flex; align-items: center; justify-content: space-between; margin-bottom: 10px; }
.dir-fields { display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 12px; }
.dir-summary { display: flex; align-items: center; flex-wrap: wrap; gap: 8px; margin-top: 14px; font-size: 13px; }
.dir-summary strong { font-weight: 500; margin-right: 4px; }
.grant-add label { flex: 1; min-width: 150px; }
.badge-ok { color: var(--accent); background: var(--accent-tint); }
.badge-warn, .sync-error { color: var(--warning); }
.stale-text { color: var(--warning); }
.stale-note { margin-top: 10px; font-size: 13px; color: var(--warning); }
.remote-line { margin-top: 10px; font-size: 13px; display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
.count { margin-left: 6px; opacity: .65; font-variant-numeric: tabular-nums; }
.draft-dot { margin-left: 6px; }
.draft-head { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; padding: 10px 12px; margin-bottom: 12px; border: 1px solid var(--border); border-radius: 8px; font-size: 13px; }
.draft-row { background: var(--surface-2); }
.label-form { margin: 14px 0; }
.label-fields { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 12px; }
.label-list { list-style: none; margin: 12px 0 0; padding: 0; }
.label-list li { padding: 10px 12px; border: 1px solid var(--border); border-radius: 8px; margin-bottom: 8px; }
.label-main { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; font-size: 13px; }
.label-desc { margin: 6px 0 0; font-size: 12px; line-height: 1.6; }
.label-actions { display: flex; gap: 8px; margin-top: 8px; }
.cost-warn { color: var(--warning); line-height: 1.7; }
.cost-list { margin: 10px 0 0; padding-left: 20px; font-size: 13px; line-height: 1.9; color: var(--text-dim); }
.annotate-result { margin-top: 10px; font-size: 13px; }
.sync-error { margin-top: 12px; font-size: 13px; }
.empty { text-align: center; padding: 36px 16px; color: var(--text-dim); }
.empty h3 { margin-top: 12px; color: var(--text); }
.empty p { margin: 12px 0 20px; }
.notice-bar, .error-bar { margin-top: 12px; }
.btn { min-height: 40px; }
@media (max-width: 640px) { .fields, .dir-fields, .label-fields { grid-template-columns: 1fr; } .summary { gap: 12px; } .card { padding: 16px; } .grant-list li span { flex-basis: 100%; } .delete-button { margin-left: 0; } .btn { min-height: 44px; } }
</style>
