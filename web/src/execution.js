// Checks that catch execution-profile mistakes before a run does, using only
// variable NAMES (the server never sends values). They warn, not block: the
// server's Validate is the authority, and a variable may be saved afterwards.
//
// p uses the settings form's shape: agents, variables (env), agentVariables
// (agent_env), writeVariables (write_env), writeAgentVariables (write_agent_env).
export function profileIssues(p, varKeys = {}) {
  const issues = []
  const saved = (agent) => new Set(varKeys[agent] || [])
  const agentRows = [...(p.agentVariables || []), ...(p.write_approval ? p.writeAgentVariables || [] : [])]
  for (const agent of p.agents || []) {
    if (agent === 'root') {
      if (agentRows.length) issues.push('root（单 Agent）不能保存 Agent 变量，引用的 Agent 变量永远取不到值；请改为分配给具名 Agent。')
      continue
    }
    const have = saved(agent)
    for (const v of agentRows) {
      const source = (v.source || '').trim()
      if (source && !have.has(source)) issues.push(`${agent} 没有保存变量 ${source}，执行时会报「未配置执行变量来源」。`)
    }
    // The mistake that is easiest to make: a server-environment mapping whose
    // source is the name of a variable the agent saved. It reads the service
    // process environment, never the agent's saved values.
    const serverRows = [...(p.variables || []), ...(p.write_approval ? p.writeVariables || [] : [])]
    for (const v of serverRows) {
      const source = (v.source || '').trim()
      if (source && have.has(source)) issues.push(`服务端环境变量 ${source} 读取的是服务进程的环境变量，不读 ${agent} 保存的同名变量；如果要用 Agent 变量，请改填在「诊断时注入的 Agent 变量」里。`)
    }
  }
  return issues
}

// Saved variable names of the profile's agents that are not referenced yet,
// excluding those reserved for approved runs: a source may not serve both.
export function unreferencedAgentVars(p, varKeys = {}) {
  const referenced = new Set((p.agentVariables || []).map(v => (v.source || '').trim()))
  const reserved = new Set((p.writeAgentVariables || []).map(v => (v.source || '').trim()))
  const out = new Set()
  for (const agent of p.agents || []) {
    for (const key of varKeys[agent] || []) {
      if (!referenced.has(key) && !reserved.has(key)) out.add(key)
    }
  }
  return [...out].sort()
}
