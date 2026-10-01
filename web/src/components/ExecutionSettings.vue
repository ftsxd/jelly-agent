<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'
import { fromRows, inheritedVars, profileIssues, toRows, unreferencedAgentVars } from '../execution'

const emit = defineEmits(['saved'])
const loading = ref(true), saving = ref(false), checking = ref(false)
const error = ref(''), notice = ref(''), info = ref(null), agents = ref(['root'])
// agent → saved variable NAMES, for picking sources and catching misplacement.
const varKeys = ref({})
const config = ref({ enabled: false, backend: 'os', image: '', timeout_sec: 30, max_output_kb: 64, profiles: [] })
const probe = ref({ agent: 'root', profile: '', purpose: '检查诊断命令是否符合执行规则', command: '' })
const checked = ref(null)
const decisionName = { allow: '允许执行', prompt: '需要审批，当前不执行', forbidden: '禁止执行' }

onMounted(load)
async function load() {
  loading.value = true; error.value = ''
  try {
    const [data, list] = await Promise.all([api.execution(), api.agents()])
    info.value = data
    agents.value = ['root', ...(list.agents || []).map(a => a.name).filter(n => n !== 'root')]
    varKeys.value = list.var_keys || {}
    const c = data.config
    config.value = {
      enabled: !!c.enabled, backend: c.backend || 'os', image: c.image || '',
      timeout_sec: c.timeout_sec || data.defaults.timeout_sec,
      max_output_kb: c.max_output_kb || data.defaults.max_output_kb,
      profiles: (c.profiles || []).map(p => ({ ...p, agents: [...p.agents], network: !!p.network,
        inherit: p.inherit_agent_vars !== false, mappings: toRows(p),
        rules: (p.rules || []).map(r => ({ ...r, pattern: [...r.pattern] })) })),
    }
    probe.value.profile = config.value.profiles[0]?.name || ''
  } catch (e) { error.value = e.message } finally { loading.value = false }
}
function addProfile() {
  config.value.profiles.push({ name: '', agents: [], network: false, inherit: true, mappings: [], rules: [] })
}
const savedKeys = (p) => [...new Set(p.agents.flatMap(a => varKeys.value[a] || []))].sort()
const unreferenced = (p) => unreferencedAgentVars(p, varKeys.value)
const inherited = (p) => inheritedVars(p, varKeys.value)
const issues = (p) => profileIssues(p, varKeys.value)
const addMapping = (p) => p.mappings.push({ key: '', from: 'agent', source: '', when: 'always' })
// Open the advanced section only when something in it is already set, so a
// configured value is never hidden behind a closed fold.
const hasAdvanced = (p) => !!(p.tool_dir || p.kubeconfig_env || p.mappings.length || p.rules.length || p.allow_unconfined_with_approval ||
  (p.write_approval && p.write_kubeconfig_env))
// Same-name references for every saved variable not yet mapped — the common
// case, which otherwise means typing each name twice.
function referenceSaved(p) {
  for (const key of unreferencedAgentVars(p, varKeys.value)) p.mappings.push({ key, from: 'agent', source: key, when: 'always' })
}
async function save() {
  if (saving.value) return
  saving.value = true; error.value = ''; notice.value = ''; checked.value = null
  try {
    const profiles = config.value.profiles.map(p => {
      return { name: p.name.trim(), agents: p.agents, network: p.network, ...fromRows(p.mappings, p.write_approval),
        ...(p.inherit === false ? { inherit_agent_vars: false } : {}),
        ...(config.value.backend === 'os' && p.tool_dir ? { tool_dir: p.tool_dir.trim() } : {}), ...(p.kubeconfig_env ? { kubeconfig_env: p.kubeconfig_env.trim() } : {}),
        ...(p.write_approval ? { write_approval: true, ...(p.write_kubeconfig_env ? { write_kubeconfig_env: p.write_kubeconfig_env.trim() } : {}) } : {}),
        ...(p.allow_unconfined_with_approval ? { allow_unconfined_with_approval: true } : {}),
        rules: p.rules.map(r => ({ name: r.name.trim(), pattern: r.pattern, decision: r.decision, reason: r.reason || '' })) }
    })
    await api.setExecution({ ...config.value, profiles, timeout_sec: Number(config.value.timeout_sec), max_output_kb: Number(config.value.max_output_kb) })
    notice.value = '已保存，下一轮对话使用新的执行配置。'; emit('saved')
    await load()
  } catch (e) { error.value = e.message } finally { saving.value = false }
}
async function check() {
  if (checking.value) return
  checking.value = true; error.value = ''; checked.value = null
  try { checked.value = await api.checkExecution(probe.value) }
  catch (e) { error.value = e.message } finally { checking.value = false }
}
</script>

<template>
  <section class="card execution">
    <div class="heading"><h2>通用诊断执行器</h2><span class="badge">shell_exec</span></div>
    <p class="muted">缺少专用工具时，让已分配的 Agent 使用 CLI 帮助和诊断命令。默认关闭；写操作需单独启用审批，并由用户逐次批准。</p>
    <p class="hint-bar">让某个 Agent 执行命令，只要到 <RouterLink to="/agents">Agent</RouterLink> 页编辑它、打开「允许此 Agent 执行命令」——凭据用它自己保存的变量，执行配置会自动建好。这里用于全局开关、隔离后端，以及多个 Agent 共用的执行配置。</p>
    <div v-if="loading" class="muted">加载中…</div>
    <template v-else-if="info">
      <form @submit.prevent="save">
        <label class="check"><input v-model="config.enabled" type="checkbox" /> 启用通用执行器</label>
        <label class="backend">隔离后端<select v-model="config.backend" class="input"><option value="os">系统沙箱</option><option value="docker">Docker</option></select></label>
        <p class="muted detail">{{ info.os_detail }}。沙箱不可用时停止执行。</p>
        <label v-if="config.backend === 'docker'">诊断工具镜像<input v-model="config.image" class="input mono" placeholder="已预装所需 CLI 的本地镜像" /></label>
        <p v-if="config.backend === 'docker' && !info.docker_available" class="warning">服务端尚未检测到 Docker，执行时不会降级到宿主机。</p>
        <details class="advanced">
          <summary>高级：超时与输出上限</summary>
          <div class="limits">
            <label>超时秒数<input v-model="config.timeout_sec" class="input" type="number" min="1" max="300" required /></label>
            <label>每路输出上限 KiB<input v-model="config.max_output_kb" class="input" type="number" min="1" max="1024" required /></label>
          </div>
          <p v-if="config.backend === 'os'" class="hint">需要崩溃后的资源回收和完整子进程终止保障时，请使用 Docker。系统沙箱无法保证终止脱离进程组的子进程。</p>
        </details>

        <div v-for="(p, i) in config.profiles" :key="i" class="profile">
          <div class="heading"><h3>执行配置 {{ i + 1 }}</h3><button class="btn" type="button" @click="config.profiles.splice(i, 1)">移除配置</button></div>
          <label>配置名称<input v-model="p.name" class="input mono" placeholder="cloud-readonly" pattern="[A-Za-z_][A-Za-z0-9_-]*" required /></label>
          <fieldset><legend>分配给 Agent</legend><div class="assignments">
            <label v-for="agent in agents" :key="agent" class="check"><input v-model="p.agents" :value="agent" type="checkbox" /> {{ agent === 'root' ? 'root（单 Agent）' : agent }}</label>
          </div></fieldset>
          <label class="check"><input v-model="p.network" type="checkbox" /> 允许此配置访问网络</label>
          <p v-if="p.network" class="warning">当前版本不限制目标域名。自动诊断使用只读凭据；生产出口白名单需由部署环境配置。</p>
          <label class="check"><input v-model="p.write_approval" type="checkbox" /> 允许申请写操作审批（每条命令批准一次）</label>
          <p v-if="p.write_approval" class="warning">用户将在对话中看到完整命令并确认。审批十分钟失效；命令、执行配置改变后需重新申请。禁止规则无法通过审批放行。</p>

          <fieldset class="group">
            <legend>Agent 变量</legend>
            <label class="check"><input v-model="p.inherit" type="checkbox" /> 自动注入分配的 Agent 在「Agent」页保存的变量（同名）</label>
            <p v-if="p.inherit && inherited(p).length" class="muted detail">每次执行注入：<span class="mono">{{ inherited(p).join('、') }}</span>。审批专用的变量不在其中；密钥值不会发送给模型。</p>
            <p v-else-if="p.inherit" class="muted detail">分配的 Agent 还没有保存变量。到「Agent」页编辑该 Agent，在「变量」里保存凭据后即可自动注入。</p>
            <div v-else class="actions">
              <span class="muted detail">已关闭自动注入，只注入「高级」里显式映射的变量。</span>
              <button v-if="unreferenced(p).length" class="btn" type="button" @click="referenceSaved(p)">引用已保存的变量（{{ unreferenced(p).join('、') }}）</button>
            </div>
          </fieldset>
          <ul v-if="issues(p).length" class="issues" role="note">
            <li v-for="(msg, k) in issues(p)" :key="k" class="warning">{{ msg }}</li>
          </ul>

          <details class="advanced" :open="hasAdvanced(p)">
            <summary>高级：变量映射、Kubeconfig、CLI 目录、无沙箱执行、命令规则</summary>
            <label v-if="config.backend === 'os'">独立 CLI 运行目录<input v-model="p.tool_dir" class="input mono" placeholder="管理员预装 CLI 的目录，包含 bin 和 lib（可选）" /></label>

            <fieldset class="group">
              <legend>变量映射</legend>
              <p class="muted detail">只在需要改名、读取服务进程环境变量，或只在审批后注入写凭据时才用。「服务端环境变量」读的是 jelly-agent 服务进程自己的环境变量（如容器 environment），不读 Agent 页保存的变量。</p>
              <datalist :id="'agent-vars-' + i"><option v-for="k in savedKeys(p)" :key="k" :value="k" /></datalist>
              <div v-for="(m, j) in p.mappings" :key="j" class="mapping-row">
                <label>子进程变量<input v-model="m.key" class="input mono" placeholder="TENCENTCLOUD_SECRET_KEY" required /></label>
                <label>来源<select v-model="m.from" class="input"><option value="agent">Agent 变量</option><option value="server">服务端环境变量</option></select></label>
                <label>来源名称<input v-model="m.source" :list="m.from === 'agent' ? 'agent-vars-' + i : null" class="input mono" required /></label>
                <label>时机<select v-model="m.when" class="input"><option value="always">每次执行</option><option value="approved" :disabled="!p.write_approval">仅审批后</option></select></label>
                <button class="btn" type="button" @click="p.mappings.splice(j, 1)">移除</button>
                <p v-if="m.when === 'approved' && !p.write_approval" class="warning span">未启用写操作审批，这一行保存时会被移除。</p>
              </div>
              <button class="btn" type="button" @click="addMapping(p)">添加变量映射</button>
            </fieldset>

            <fieldset class="group">
              <legend>Kubeconfig</legend>
              <label>Kubeconfig 服务端变量名称<input v-model="p.kubeconfig_env" class="input mono" placeholder="SRE_READONLY_KUBECONFIG（可选）" /></label>
              <label v-if="p.write_approval">审批执行 Kubeconfig 变量名称<input v-model="p.write_kubeconfig_env" class="input mono" placeholder="SRE_WRITE_KUBECONFIG（可选）" /></label>
              <p class="muted detail">变量内容是 kubeconfig 正文（服务进程环境变量）。执行时写入私有目录并在结束后删除，不挂载宿主机凭据文件。</p>
            </fieldset>

            <label class="check"><input v-model="p.allow_unconfined_with_approval" type="checkbox" /> 沙箱不可用时允许经审批后无隔离执行</label>
            <p v-if="p.allow_unconfined_with_approval" class="warning">仅用于 docker、bubblewrap、Landlock 都不可用的主机：获批的命令将不受任何文件系统或网络隔离，并在审计中标记为无隔离。</p>

            <fieldset class="group">
              <legend>附加命令规则</legend>
              <p class="muted detail">内置只读规则包括 kubectl get / describe / logs / top 等只读子命令、tccli 各产品的 Describe* / List*（返回凭据或访问入口的除外）、CLS SearchLog 和 CLI help。其他命令需要审批。附加规则按 token 前缀匹配，匹配结果取最严格的一项。</p>
              <div v-for="(rule, j) in p.rules" :key="j" class="rule">
                <div class="variable-row">
                  <label>规则名称<input v-model="rule.name" class="input" required /></label>
                  <label>执行决策<select v-model="rule.decision" class="input"><option value="allow">允许</option><option value="prompt">需审批</option><option value="forbidden">禁止</option></select></label>
                  <button class="btn" type="button" @click="p.rules.splice(j, 1)">移除规则</button>
                </div>
                <div class="tokens"><label v-for="(_, k) in rule.pattern" :key="k">前缀参数 {{ k + 1 }}<input v-model="rule.pattern[k]" class="input mono" required /></label>
                  <button class="btn" type="button" @click="rule.pattern.push('')">添加参数</button>
                  <button v-if="rule.pattern.length > 1" class="btn" type="button" @click="rule.pattern.pop()">移除末尾参数</button>
                </div>
                <label>原因<input v-model="rule.reason" class="input" /></label>
              </div>
              <button class="btn" type="button" @click="p.rules.push({ name: '', pattern: [''], decision: 'prompt', reason: '' })">添加命令规则</button>
            </fieldset>
          </details>
        </div>
        <div class="actions"><button class="btn" type="button" @click="addProfile">添加执行配置</button><button class="btn btn-primary" :disabled="saving">{{ saving ? '保存中…' : '保存执行配置' }}</button></div>
      </form>
      <details class="probe">
        <summary>检查已保存规则</summary>
        <p class="muted detail">仅返回策略判断，不运行命令，也不读取凭据。修改配置后请先保存。</p>
        <form @submit.prevent="check">
          <div class="limits">
            <label>Agent<select v-model="probe.agent" class="input"><option v-for="agent in agents" :key="agent">{{ agent }}</option></select></label>
            <label>执行配置<select v-model="probe.profile" class="input" required><option v-for="p in config.profiles" :key="p.name">{{ p.name }}</option></select></label>
          </div>
          <label>诊断命令<textarea v-model="probe.command" class="input mono" rows="2" placeholder="tccli cls DescribeTopics --Region ap-shanghai" required /></label>
          <button class="btn" :disabled="checking">{{ checking ? '检查中…' : '检查规则' }}</button>
        </form>
        <div v-if="checked" class="result" aria-live="polite"><strong>{{ decisionName[checked.decision] }}</strong><p>{{ checked.reason }}</p><pre>{{ JSON.stringify(checked.segments || [], null, 2) }}</pre></div>
      </details>
      <p v-if="notice" class="notice" role="status">{{ notice }}</p>
    </template>
    <p v-if="error" class="warning" role="alert">{{ error }}</p>
    <button v-if="!loading && !info" class="btn" @click="load">重新加载</button>
  </section>
</template>

<style scoped>
.execution { padding: var(--sp-4); display: grid; gap: var(--sp-3); }
.heading, .actions, .assignments, .tokens { display: flex; gap: var(--sp-3); align-items: center; flex-wrap: wrap; }
.heading { justify-content: space-between; }
h2 { font-size: 15px; } h3 { font-size: 13px; }
form, .profile, .rule { display: grid; gap: var(--sp-3); }
label { display: grid; gap: var(--sp-1); font-size: 12px; color: var(--text-dim); }
.check { display: flex; align-items: center; gap: var(--sp-2); }
.limits { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr)); gap: var(--sp-3); }
.profile { padding: var(--sp-3); border: 1px solid var(--border); border-radius: var(--radius); }
fieldset { border: 0; padding: 0; } legend { font-size: 12px; margin-bottom: var(--sp-2); color: var(--text-dim); }
.variable-row { display: grid; grid-template-columns: 1fr 1fr auto; gap: var(--sp-2); align-items: end; margin: var(--sp-3) 0; }
.rule { padding: var(--sp-3) 0; border-top: 1px solid var(--border); margin-top: var(--sp-3); }
.tokens label { width: 140px; } summary { cursor: pointer; font-size: 13px; }
.detail, .warning, .notice, .muted { font-size: 12px; line-height: 1.7; margin: 0; }
.detail { margin: var(--sp-2) 0; } .warning { color: var(--warning); } .notice { color: var(--accent); }
.probe { border-top: 1px solid var(--border); padding-top: var(--sp-3); }
.backend { max-width: 240px; }
.hint-bar { margin: 0; padding: var(--sp-2) var(--sp-3); border-radius: var(--radius-sm); background: var(--primary-tint); font-size: 12px; line-height: 1.7; }
.group { display: grid; gap: var(--sp-2); padding: var(--sp-3) 0 0; border-top: 1px solid var(--border); }
.advanced { display: grid; gap: var(--sp-3); }
.advanced[open] > summary { margin-bottom: var(--sp-3); }
.mapping-row { display: grid; grid-template-columns: 1.2fr 1fr 1.2fr 0.9fr auto; gap: var(--sp-2); align-items: end; }
.mapping-row .span { grid-column: 1 / -1; }
.issues { margin: 0; padding-left: 18px; display: grid; gap: var(--sp-1); }
.result { margin-top: var(--sp-3); font-size: 13px; } pre { overflow: auto; font-size: 12px; }
@media (max-width: 640px) { .limits, .variable-row, .mapping-row { grid-template-columns: 1fr; } }
</style>
