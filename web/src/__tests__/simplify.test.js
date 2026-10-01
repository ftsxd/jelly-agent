// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { agentExecution, agentRecord, setAgentExecution } from '../agents'
import { fromRows, inheritedVars, profileIssues, toRows, unreferencedAgentVars } from '../execution'

describe('agent record resend', () => {
  it('keeps the tri-state skills choice on a one-field change', () => {
    const base = { name: 'coordinator', enabled: true, mcp: ['logs'] }
    expect(agentRecord({ ...base, skills: [] }, { enabled: false })).toMatchObject({ skills: [], enabled: false, mcp: ['logs'] })
    expect(agentRecord({ ...base, skills: null }).skills).toBeNull()
    expect(agentRecord({ ...base }).skills).toBeNull()
    expect(agentRecord({ ...base, skills: ['promql'] }, { make_default: true })).toMatchObject({ skills: ['promql'], make_default: true, enabled: true })
  })
})

describe('execution switch on the agent page', () => {
  const shared = { name: 'shared', agents: ['KubeInspector', 'MetricsQuery'], network: true, rules: [{ name: 'r' }] }
  const config = { enabled: false, profiles: [shared] }

  it('turning on creates the agent\'s own approving profile and enables the executor', () => {
    const { config: cfg, removed } = setAgentExecution(config, 'TencentQuery', { on: true, network: true })
    expect(removed).toEqual([])
    expect(cfg.enabled).toBe(true)
    expect(cfg.profiles.at(-1)).toEqual({ name: 'TencentQuery', agents: ['TencentQuery'], network: true, write_approval: true, env: {}, rules: [] })
    expect(config.profiles).toHaveLength(1) // input untouched
    expect(agentExecution(cfg, 'TencentQuery')).toMatchObject({ on: true, own: { name: 'TencentQuery' }, shared: [] })
  })
  it('the network box edits the own profile only, and an unchanged form writes nothing', () => {
    const on = setAgentExecution(config, 'TencentQuery', { on: true, network: true }).config
    expect(setAgentExecution(on, 'TencentQuery', { on: true, network: true })).toBeNull()
    const off = setAgentExecution(on, 'TencentQuery', { on: true, network: false }).config
    expect(off.profiles.find(p => p.name === 'TencentQuery').network).toBe(false)
    expect(off.profiles.find(p => p.name === 'shared')).toEqual(shared)
  })
  it('an agent on a shared profile is reported, not given a second profile', () => {
    const state = agentExecution({ ...config, enabled: true }, 'MetricsQuery')
    expect(state).toMatchObject({ on: true, own: null })
    expect(state.shared.map(p => p.name)).toEqual(['shared'])
    expect(setAgentExecution({ ...config, enabled: true }, 'MetricsQuery', { on: true, network: false })).toBeNull()
  })
  it('turning off removes the agent everywhere and names profiles left empty', () => {
    const on = setAgentExecution({ ...config, enabled: true }, 'KubeInspector', { on: true, network: true })
    expect(on).toBeNull() // already on through the shared profile
    const r = setAgentExecution({ ...config, enabled: true }, 'KubeInspector', { on: false })
    expect(r.removed).toEqual([])
    expect(r.config.profiles[0].agents).toEqual(['MetricsQuery'])
    const last = setAgentExecution({ enabled: true, profiles: [{ name: 'solo', agents: ['A'] }] }, 'A', { on: false })
    expect(last.removed).toEqual(['solo'])
    expect(last.config).toMatchObject({ enabled: false, profiles: [] })
    expect(setAgentExecution(config, 'Nobody', { on: false })).toBeNull()
  })
})

describe('sidebar groups', () => {
  it('lists every page once, grouped in order', async () => {
    const { navGroups, navItems } = await import('../router')
    expect(navGroups.map(g => g.title)).toEqual(['使用', '能力', '接入', '系统'])
    expect(navGroups.flatMap(g => g.items)).toEqual(navItems)
    expect(navGroups[0].items.map(i => i.meta.title)).toEqual(['对话', '执行记录', '周期任务'])
    expect(navItems.some(i => i.path === '/sessions')).toBe(false)
  })
})

describe('execution profile checks', () => {
  const varKeys = { TencentQuery: ['TENCENTCLOUD_REGION', 'TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY', 'WRITE_KEY', 'LD_PRELOAD'] }
  const row = (key, from, source, when = 'always') => ({ key, from, source, when })
  const profile = (extra) => ({ agents: ['TencentQuery'], mappings: [], ...extra })

  it('turns the four API maps into rows and back without loss', () => {
    const api = { env: { A: 'SRV_A' }, agent_env: { B: 'AGENT_B' }, write_env: { C: 'SRV_C' }, write_agent_env: { B: 'AGENT_W' } }
    const rows = toRows(api)
    expect(rows).toHaveLength(4)
    expect(fromRows(rows, true)).toEqual(api)
    expect(fromRows(rows, false)).toEqual({ env: { A: 'SRV_A' }, agent_env: { B: 'AGENT_B' } })
    expect(() => fromRows([row('A', 'agent', 'X'), row('A', 'server', 'Y')], false)).toThrow('重复')
  })
  it('flags agent variables mapped as server environment variables', () => {
    const issues = profileIssues(profile({ write_approval: true, mappings: [row('TENCENTCLOUD_SECRET_ID', 'server', 'TENCENTCLOUD_SECRET_ID', 'approved')] }), varKeys)
    expect(issues).toHaveLength(1)
    expect(issues[0]).toContain('服务端环境变量 TENCENTCLOUD_SECRET_ID')
  })
  it('flags referenced agent variables that were never saved, and root', () => {
    expect(profileIssues(profile({ mappings: [row('TOKEN', 'agent', 'MISSING')] }), varKeys)[0]).toContain('没有保存变量 MISSING')
    expect(profileIssues(profile({ agents: ['root'], mappings: [row('T', 'agent', 'T')] }), varKeys)[0]).toContain('root')
    expect(profileIssues(profile({ mappings: [row('TOKEN', 'agent', 'TENCENTCLOUD_SECRET_ID')] }), varKeys)).toEqual([])
  })
  it('flags assigned agents that saved nothing while inheritance is on', () => {
    expect(profileIssues(profile({ agents: ['Empty'] }), varKeys)[0]).toContain('Empty 还没有保存任何变量')
    expect(profileIssues(profile({ agents: ['Empty'], inherit: false }), varKeys)).toEqual([])
    expect(profileIssues(profile({}), varKeys)).toEqual([])
  })
  it('previews inherited variables with the server exclusions', () => {
    const p = profile({ write_approval: true, mappings: [row('REGION', 'agent', 'TENCENTCLOUD_REGION'), row('KEY', 'agent', 'WRITE_KEY', 'approved')] })
    expect(inheritedVars(p, varKeys)).toEqual(['TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY'])
    expect(inheritedVars({ ...p, inherit: false }, varKeys)).toEqual([])
    expect(unreferencedAgentVars({ ...p, inherit: false }, varKeys)).toEqual(['TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY'])
  })
})
