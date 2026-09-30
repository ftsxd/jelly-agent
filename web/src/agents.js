// The save endpoint rebuilds the whole record, so a one-field change must
// resend every field as stored. skills is tri-state: null (every skill) must
// stay null and [] (none) must stay [] — dropping it turns "none" into "all".
export function agentRecord(a, patch = {}) {
  return {
    name: a.name,
    description: a.description || '',
    provider: a.provider || '',
    instruction: a.instruction || '',
    mcp: a.mcp || [],
    required_tools: a.required_tools || [],
    required_suites: a.required_suites || [],
    skills: a.skills == null ? null : [...a.skills],
    sub_agents: a.sub_agents || [],
    enabled: a.enabled,
    ...patch,
  }
}

// The agent page shows execution as one switch, not as profiles. Behind it
// each agent gets its own profile, named after it and assigned to it alone;
// profiles shared with other agents stay a Tools-page concept and are only
// reported here.
export function agentExecution(config, agent) {
  const mine = (config?.profiles || []).filter((p) => p.agents.includes(agent))
  const solo = mine.filter((p) => p.agents.length === 1)
  const own = solo.find((p) => p.name === agent) || solo[0] || null
  return { on: mine.length > 0, own, shared: mine.filter((p) => p !== own) }
}

// setAgentExecution returns { config, removed } for the switch's new state,
// or null when nothing would change. Turning on creates the agent's own
// profile — network as chosen, and able to ask for approval, since without it
// a command outside the read-only rules could never run at all. Turning off
// removes the agent everywhere; a profile left with no agent is deleted
// (`removed` names them, so the caller can confirm first), and with no profile
// left the executor is switched off — the server refuses it enabled but empty.
export function setAgentExecution(config, agent, { on, network }) {
  const now = agentExecution(config, agent)
  const cfg = JSON.parse(JSON.stringify(config || { profiles: [] }))
  cfg.profiles ||= []
  if (!on) {
    if (!now.on) return null
    const removed = []
    cfg.profiles = cfg.profiles
      .map((p) => ({ ...p, agents: p.agents.filter((a) => a !== agent) }))
      .filter((p) => (p.agents.length ? true : (removed.push(p.name), false)))
    if (!cfg.profiles.length) cfg.enabled = false
    return { config: cfg, removed }
  }
  if (now.own) {
    if (!!now.own.network === !!network && cfg.enabled) return null
    cfg.profiles = cfg.profiles.map((p) => (p.name === now.own.name ? { ...p, network: !!network } : p))
  } else if (!now.on) {
    const taken = new Set(cfg.profiles.map((p) => p.name))
    let name = agent
    for (let i = 2; taken.has(name); i++) name = `${agent}-${i}`
    cfg.profiles.push({ name, agents: [agent], network: !!network, write_approval: true, env: {}, rules: [] })
  } else if (cfg.enabled) {
    return null // only shared profiles: their settings belong to the Tools page
  }
  cfg.enabled = true
  return { config: cfg, removed: [] }
}
