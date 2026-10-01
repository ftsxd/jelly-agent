// Execution-profile helpers for the settings form. Only variable NAMES pass
// through here; the server never sends values.
//
// The four API maps become one list of rows, because they differ in exactly
// two ways — where a value comes from and when it is injected — and four
// look-alike groups is how credentials ended up in the wrong one:
//
//   env             from 'server', when 'always'
//   agent_env       from 'agent',  when 'always'
//   write_env       from 'server', when 'approved'
//   write_agent_env from 'agent',  when 'approved'
const MAPS = [
  ['env', 'server', 'always'],
  ['agent_env', 'agent', 'always'],
  ['write_env', 'server', 'approved'],
  ['write_agent_env', 'agent', 'approved'],
]

export function toRows(p) {
  const rows = []
  for (const [field, from, when] of MAPS) {
    for (const [key, source] of Object.entries(p[field] || {})) rows.push({ key, from, source, when })
  }
  return rows
}

// Back to the API maps. Approved rows only exist while write approval is on:
// turning it off revokes them. A child variable may appear once per timing —
// the server rejects one fed from both sources.
export function fromRows(rows, writeApproval) {
  const out = { env: {} }
  const seen = { always: new Set(), approved: new Set() }
  for (const r of rows) {
    if (r.when === 'approved' && !writeApproval) continue
    const key = (r.key || '').trim(), source = (r.source || '').trim()
    if (!key || !source) throw new Error('变量映射的子进程变量和来源名称都必须填写')
    if (seen[r.when].has(key)) throw new Error(`变量 ${key} 在「${r.when === 'approved' ? '仅审批后' : '每次执行'}」里重复映射`)
    seen[r.when].add(key)
    const field = MAPS.find(([, from, when]) => from === r.from && when === r.when)[0]
    ;(out[field] ||= {})[key] = source
  }
  if (writeApproval) out.write_env ||= {}
  return out
}

const rowsOf = (p, from, when) => (p.mappings || []).filter(r => r.from === from && r.when === when && (when === 'always' || p.write_approval))
const sourcesOf = (rows) => new Set(rows.map(r => (r.source || '').trim()).filter(Boolean))

// Mirrors dangerousEnv in internal/execution/policy.go: names that would change
// the execution environment are never injected, inherited or not.
const DANGEROUS = new Set(['PATH', 'HOME', 'TMPDIR', 'ENV', 'BASH_ENV', 'SHELLOPTS', 'BASHOPTS', 'CDPATH', 'IFS', 'PYTHONPATH', 'PYTHONHOME', 'NODE_OPTIONS', 'PERL5OPT', 'RUBYOPT', 'GIT_CONFIG', 'GIT_CONFIG_COUNT', 'KUBECONFIG'])
const dangerous = (k) => DANGEROUS.has(k) || /^(LD_|DYLD_|BASH_FUNC_|DOCKER_)/.test(k)

// Saved variables the server will add by name when inheritance is on — the
// same exclusions as inheritedAgentEnv: approval-only sources, anything mapped
// explicitly (as a child variable or a source), and environment-altering names.
export function inheritedVars(p, varKeys = {}) {
  if (p.inherit === false) return []
  const reserved = sourcesOf(rowsOf(p, 'agent', 'approved'))
  const explicit = new Set()
  for (const r of (p.mappings || []).filter(r => r.when === 'always')) {
    explicit.add((r.key || '').trim()); explicit.add((r.source || '').trim())
  }
  const out = new Set()
  for (const agent of p.agents || []) {
    for (const k of varKeys[agent] || []) if (!reserved.has(k) && !explicit.has(k) && !dangerous(k)) out.add(k)
  }
  return [...out].sort()
}

// Saved variables not injected at all yet — offered as one-click same-name
// rows when inheritance is off. Approval-only sources are never offered.
export function unreferencedAgentVars(p, varKeys = {}) {
  return inheritedVars({ ...p, inherit: true }, varKeys)
}

// Mistakes worth catching before a run does. They warn, not block: the
// server's Validate is the authority, and a variable may be saved afterwards.
export function profileIssues(p, varKeys = {}) {
  const issues = []
  const agentRows = [...rowsOf(p, 'agent', 'always'), ...rowsOf(p, 'agent', 'approved')]
  const serverRows = [...rowsOf(p, 'server', 'always'), ...rowsOf(p, 'server', 'approved')]
  for (const agent of p.agents || []) {
    if (agent === 'root') {
      if (agentRows.length) issues.push('root（单 Agent）不能保存 Agent 变量，引用的 Agent 变量永远取不到值；请改为分配给具名 Agent。')
      continue
    }
    const have = new Set(varKeys[agent] || [])
    // Inheritance on and nothing saved: the run will carry no credentials,
    // and a CLI then fails with its own, misleading error (tccli: "secretId
    // is invalid"). Say where they go before that happens.
    if (p.inherit !== false && !have.size && !agentRows.length) {
      issues.push(`${agent} 还没有保存任何变量，命令执行时不会带凭据。到「Agent」页编辑 ${agent}，在「变量」里保存。`)
    }
    for (const source of sourcesOf(agentRows)) {
      if (!have.has(source)) issues.push(`${agent} 没有保存变量 ${source}，执行时会报「未配置执行变量来源」。`)
    }
    // The easiest mistake: a server-environment row naming a variable the
    // agent saved. It reads the service process environment, never saved values.
    for (const source of sourcesOf(serverRows)) {
      if (have.has(source)) issues.push(`服务端环境变量 ${source} 读取的是服务进程的环境变量，不读 ${agent} 保存的同名变量；如果要用 Agent 变量，请把来源改为「Agent 变量」。`)
    }
  }
  return issues
}
