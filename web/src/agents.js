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

// assignExecution returns the execution config with agent's assignment set to
// exactly `chosen` (profile names), plus — when create is given — a new
// profile named after the agent. It switches the executor on when the agent
// ends up assigned: assigning from the agent page means "this agent runs".
// null when nothing changed, so an untouched form never rewrites the config.
export function assignExecution(config, agent, before, chosen, create) {
  const was = new Set(before), now = new Set(chosen)
  if (!create && was.size === now.size && [...now].every((p) => was.has(p))) return null
  const cfg = JSON.parse(JSON.stringify(config))
  cfg.profiles = (cfg.profiles || []).map((p) => {
    const others = p.agents.filter((a) => a !== agent)
    return { ...p, agents: now.has(p.name) ? [...others, agent] : others }
  })
  if (create) {
    const taken = new Set(cfg.profiles.map((p) => p.name))
    let name = agent
    for (let i = 2; taken.has(name); i++) name = `${agent}-${i}`
    cfg.profiles.push({ name, agents: [agent], network: !!create.network, env: {}, rules: [] })
  }
  if (cfg.profiles.some((p) => p.agents.includes(agent))) cfg.enabled = true
  return cfg
}
