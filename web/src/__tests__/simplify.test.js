// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { agentRecord, assignExecution } from '../agents'
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

describe('execution assignment from the agent page', () => {
  const config = { enabled: false, backend: 'os', profiles: [{ name: 'cloud', agents: ['other'], network: true, agent_env: { A: 'A' } }, { name: 'ops', agents: ['ops', 'TencentQuery'], network: false }] }
  it('returns null when nothing changed', () => {
    expect(assignExecution(config, 'TencentQuery', ['ops'], ['ops'], null)).toBeNull()
  })
  it('sets exactly the chosen profiles, keeps other fields, and enables the executor', () => {
    const cfg = assignExecution(config, 'TencentQuery', ['ops'], ['cloud'], null)
    expect(cfg.profiles.map(p => p.agents)).toEqual([['other', 'TencentQuery'], ['ops']])
    expect(cfg.profiles[0].agent_env).toEqual({ A: 'A' })
    expect(cfg.enabled).toBe(true)
    expect(config.profiles[0].agents).toEqual(['other']) // input untouched
  })
  it('creates a profile named after the agent, avoiding taken names', () => {
    const cfg = assignExecution({ ...config, profiles: [...config.profiles, { name: 'TencentQuery', agents: ['x'] }] }, 'TencentQuery', [], [], { network: true })
    expect(cfg.profiles.at(-1)).toEqual({ name: 'TencentQuery-2', agents: ['TencentQuery'], network: true, env: {}, rules: [] })
  })
  it('does not switch the executor on when the agent ends up unassigned', () => {
    expect(assignExecution(config, 'TencentQuery', ['ops'], [], null).enabled).toBe(false)
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
  it('previews inherited variables with the server exclusions', () => {
    const p = profile({ write_approval: true, mappings: [row('REGION', 'agent', 'TENCENTCLOUD_REGION'), row('KEY', 'agent', 'WRITE_KEY', 'approved')] })
    expect(inheritedVars(p, varKeys)).toEqual(['TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY'])
    expect(inheritedVars({ ...p, inherit: false }, varKeys)).toEqual([])
    expect(unreferencedAgentVars({ ...p, inherit: false }, varKeys)).toEqual(['TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY'])
  })
})
