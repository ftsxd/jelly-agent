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
