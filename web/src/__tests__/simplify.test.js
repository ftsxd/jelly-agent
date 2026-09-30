// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { agentRecord } from '../agents'
import { profileIssues, unreferencedAgentVars } from '../execution'

describe('agent record resend', () => {
  it('keeps the tri-state skills choice on a one-field change', () => {
    const base = { name: 'coordinator', enabled: true, mcp: ['logs'] }
    expect(agentRecord({ ...base, skills: [] }, { enabled: false })).toMatchObject({ skills: [], enabled: false, mcp: ['logs'] })
    expect(agentRecord({ ...base, skills: null }).skills).toBeNull()
    expect(agentRecord({ ...base }).skills).toBeNull()
    expect(agentRecord({ ...base, skills: ['promql'] }, { make_default: true })).toMatchObject({ skills: ['promql'], make_default: true, enabled: true })
  })
})

describe('sidebar groups', () => {
  it('lists every page once, grouped in order', async () => {
    const { navGroups, navItems } = await import('../router')
    expect(navGroups.map(g => g.title)).toEqual(['使用', '能力', '接入', '系统'])
    expect(navGroups.flatMap(g => g.items)).toEqual(navItems)
    expect(navGroups[0].items.map(i => i.meta.title)).toEqual(['对话', '任务', '会话', '周期任务'])
  })
})

describe('execution profile checks', () => {
  const varKeys = { TencentQuery: ['TENCENTCLOUD_REGION', 'TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY', 'WRITE_KEY'] }
  const profile = (extra) => ({ agents: ['TencentQuery'], variables: [], agentVariables: [], writeVariables: [], writeAgentVariables: [], ...extra })

  it('flags agent variables mapped as server environment variables', () => {
    const issues = profileIssues(profile({ write_approval: true, writeVariables: [{ key: 'TENCENTCLOUD_SECRET_ID', source: 'TENCENTCLOUD_SECRET_ID' }] }), varKeys)
    expect(issues).toHaveLength(1)
    expect(issues[0]).toContain('服务端环境变量 TENCENTCLOUD_SECRET_ID')
  })
  it('flags referenced agent variables that were never saved, and root', () => {
    expect(profileIssues(profile({ agentVariables: [{ key: 'TOKEN', source: 'MISSING' }] }), varKeys)[0]).toContain('没有保存变量 MISSING')
    expect(profileIssues(profile({ agents: ['root'], agentVariables: [{ key: 'T', source: 'T' }] }), varKeys)[0]).toContain('root')
    expect(profileIssues(profile({ agentVariables: [{ key: 'TENCENTCLOUD_SECRET_ID', source: 'TENCENTCLOUD_SECRET_ID' }] }), varKeys)).toEqual([])
  })
  it('suggests unmapped saved variables but never approval-only ones', () => {
    const p = profile({ agentVariables: [{ key: 'TENCENTCLOUD_REGION', source: 'TENCENTCLOUD_REGION' }], writeAgentVariables: [{ key: 'KEY', source: 'WRITE_KEY' }] })
    expect(unreferencedAgentVars(p, varKeys)).toEqual(['TENCENTCLOUD_SECRET_ID', 'TENCENTCLOUD_SECRET_KEY'])
  })
})
