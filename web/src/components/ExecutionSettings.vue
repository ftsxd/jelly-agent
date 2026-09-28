<script setup>
import { onMounted, ref } from 'vue'
import { api } from '../api'

const emit = defineEmits(['saved'])
const loading = ref(true), saving = ref(false), checking = ref(false)
const error = ref(''), notice = ref(''), info = ref(null), agents = ref(['root'])
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
    const c = data.config
    config.value = {
      enabled: !!c.enabled, backend: c.backend || 'os', image: c.image || '',
      timeout_sec: c.timeout_sec || data.defaults.timeout_sec,
      max_output_kb: c.max_output_kb || data.defaults.max_output_kb,
      profiles: (c.profiles || []).map(p => ({ ...p, agents: [...p.agents], network: !!p.network,
        variables: Object.entries(p.env || {}).map(([key, source]) => ({ key, source })),
        writeVariables: Object.entries(p.write_env || {}).map(([key, source]) => ({ key, source })),
        agentVariables: Object.entries(p.agent_env || {}).map(([key, source]) => ({ key, source })),
        writeAgentVariables: Object.entries(p.write_agent_env || {}).map(([key, source]) => ({ key, source })),
        rules: (p.rules || []).map(r => ({ ...r, pattern: [...r.pattern] })) })),
    }
    probe.value.profile = config.value.profiles[0]?.name || ''
  } catch (e) { error.value = e.message } finally { loading.value = false }
}
function addProfile() {
  config.value.profiles.push({ name: '', agents: [], network: false, variables: [], writeVariables: [], agentVariables: [], writeAgentVariables: [], rules: [] })
}
function sources(variables) {
  const env = {}
  for (const v of variables) {
    const key = v.key.trim(), source = v.source.trim()
    if (!key || !source || key in env) throw new Error('变量名和来源名称必须填写，变量名不能重复')
    env[key] = source
  }
  return env
}
async function save() {
  if (saving.value) return
  saving.value = true; error.value = ''; notice.value = ''; checked.value = null
  try {
    const profiles = config.value.profiles.map(p => {
      const env = sources(p.variables)
      return { name: p.name.trim(), agents: p.agents, network: p.network, env,
        ...(p.agentVariables.length ? { agent_env: sources(p.agentVariables) } : {}),
        ...(config.value.backend === 'os' && p.tool_dir ? { tool_dir: p.tool_dir.trim() } : {}), ...(p.kubeconfig_env ? { kubeconfig_env: p.kubeconfig_env.trim() } : {}),
        ...(p.write_approval ? { write_approval: true, write_env: sources(p.writeVariables), ...(p.writeAgentVariables.length ? { write_agent_env: sources(p.writeAgentVariables) } : {}), ...(p.write_kubeconfig_env ? { write_kubeconfig_env: p.write_kubeconfig_env.trim() } : {}) } : {}),
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
    <div v-if="loading" class="muted">加载中…</div>
    <template v-else-if="info">
      <form @submit.prevent="save">
        <label class="check"><input v-model="config.enabled" type="checkbox" /> 启用通用执行器</label>
        <div class="limits">
          <label>隔离后端<select v-model="config.backend" class="input"><option value="os">系统沙箱</option><option value="docker">Docker</option></select></label>
          <label>超时秒数<input v-model="config.timeout_sec" class="input" type="number" min="1" max="300" required /></label>
          <label>每路输出上限 KiB<input v-model="config.max_output_kb" class="input" type="number" min="1" max="1024" required /></label>
        </div>
        <p class="muted detail">{{ info.os_detail }}。沙箱不可用时停止执行。</p>
        <label v-if="config.backend === 'docker'">诊断工具镜像<input v-model="config.image" class="input mono" placeholder="已预装所需 CLI 的本地镜像" /></label>
        <p v-if="config.backend === 'docker' && !info.docker_available" class="warning">服务端尚未检测到 Docker，执行时不会降级到宿主机。</p>

        <div v-for="(p, i) in config.profiles" :key="i" class="profile">
          <div class="heading"><h3>执行配置 {{ i + 1 }}</h3><button class="btn" type="button" @click="config.profiles.splice(i, 1)">移除配置</button></div>
          <label>配置名称<input v-model="p.name" class="input mono" placeholder="cloud-readonly" pattern="[A-Za-z_][A-Za-z0-9_-]*" required /></label>
          <fieldset><legend>分配给 Agent</legend><div class="assignments">
            <label v-for="agent in agents" :key="agent" class="check"><input v-model="p.agents" :value="agent" type="checkbox" /> {{ agent === 'root' ? 'root（单 Agent）' : agent }}</label>
          </div></fieldset>
          <label v-if="config.backend === 'os'">独立 CLI 运行目录<input v-model="p.tool_dir" class="input mono" placeholder="管理员预装 CLI 的目录，包含 bin 和 lib（可选）" /></label>
          <label class="check"><input v-model="p.network" type="checkbox" /> 允许此配置访问网络</label>
          <p v-if="p.network" class="warning">当前版本不限制目标域名。自动诊断使用只读凭据；生产出口白名单需由部署环境配置。</p>
          <label class="check"><input v-model="p.write_approval" type="checkbox" /> 允许申请写操作审批（每条命令批准一次）</label>
          <div v-if="p.write_approval" class="write-credentials">
            <p class="warning">用户将在对话中看到完整命令并确认。审批十分钟失效；命令、执行配置改变后需重新申请。禁止规则无法通过审批放行。</p>
            <label>审批执行 Kubeconfig 变量名称<input v-model="p.write_kubeconfig_env" class="input mono" placeholder="SRE_WRITE_KUBECONFIG（可选）" /></label>
            <p class="muted detail">可引用权限限定到目标资源的独立写凭据。审批专用 Agent 来源不会注入技能脚本。获批后覆盖同名诊断变量，执行结束后清理；留空沿用诊断凭据。当前未自动签发临时凭据。</p>
            <div v-for="(v, j) in p.writeVariables" :key="j" class="variable-row">
              <label>审批子进程变量<input v-model="v.key" class="input mono" placeholder="TENCENTCLOUD_SECRET_KEY" required /></label>
              <label>审批服务端变量名称<input v-model="v.source" class="input mono" placeholder="TENCENT_WRITE_SECRET_KEY" required /></label>
              <button class="btn" type="button" @click="p.writeVariables.splice(j, 1)">移除审批变量</button>
            </div>
            <button class="btn" type="button" @click="p.writeVariables.push({ key: '', source: '' })">添加审批变量映射</button>
            <div v-for="(v, j) in p.writeAgentVariables" :key="'wa' + j" class="variable-row">
              <label>审批子进程变量<input v-model="v.key" class="input mono" required /></label>
              <label>该 Agent 保存的写变量名称<input v-model="v.source" class="input mono" required /></label>
              <button class="btn" type="button" @click="p.writeAgentVariables.splice(j, 1)">移除审批 Agent 变量</button>
            </div>
            <button class="btn" type="button" @click="p.writeAgentVariables.push({ key: '', source: '' })">引用 Agent 写变量</button>
          </div>
          <details>
            <summary>凭据与附加命令规则</summary>
            <p class="muted detail">凭据通过变量映射注入，可引用服务端环境变量或当前 Agent 已保存的变量名称。不要在命令或此处填写密钥值。</p>
            <label>Kubeconfig 服务端变量名称<input v-model="p.kubeconfig_env" class="input mono" placeholder="SRE_READONLY_KUBECONFIG（可选）" /></label>
            <p class="muted detail">此变量应包含只读 kubeconfig 正文。执行时写入私有目录并在结束后删除，不挂载宿主机凭据文件。</p>
            <div v-for="(v, j) in p.variables" :key="j" class="variable-row">
              <label>子进程变量<input v-model="v.key" class="input mono" placeholder="TENCENTCLOUD_SECRET_KEY" required /></label>
              <label>服务端变量名称<input v-model="v.source" class="input mono" placeholder="TENCENT_RO_SECRET_KEY" required /></label>
              <button class="btn" type="button" @click="p.variables.splice(j, 1)">移除变量</button>
            </div>
            <button class="btn" type="button" @click="p.variables.push({ key: '', source: '' })">添加变量映射</button>
            <p class="muted detail">引用 Agent 变量后，执行器从当前 Agent 的变量设置中取值；每个分配的 Agent 都需要保存对应来源。密钥值不会发送给模型。</p>
            <div v-for="(v, j) in p.agentVariables" :key="'av' + j" class="variable-row">
              <label>子进程变量<input v-model="v.key" class="input mono" placeholder="TENCENTCLOUD_SECRET_KEY" required /></label>
              <label>该 Agent 保存的变量名称<input v-model="v.source" class="input mono" placeholder="TENCENTCLOUD_SECRET_KEY" required /></label>
              <button class="btn" type="button" @click="p.agentVariables.splice(j, 1)">移除 Agent 变量</button>
            </div>
            <button class="btn" type="button" @click="p.agentVariables.push({ key: '', source: '' })">引用 Agent 变量</button>
            <p class="muted detail">内置只读规则包括 kubectl get / describe / logs / top、tccli CLS Describe* / List* / SearchLog 和 CLI help。未知命令需要审批。附加规则按 token 前缀匹配，匹配结果取最严格的一项。</p>
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
.result { margin-top: var(--sp-3); font-size: 13px; } pre { overflow: auto; font-size: 12px; }
@media (max-width: 640px) { .limits, .variable-row { grid-template-columns: 1fr; } }
</style>
