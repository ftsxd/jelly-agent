// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApp, h, nextTick, reactive } from 'vue'
import ExecutionApprovals from '../components/ExecutionApprovals.vue'
import { api, streamChat } from '../api'

vi.mock('../api', async (original) => ({ ...(await original()), api: { executionApprovals: vi.fn() } }))
let app, host, props, decisions
const pending = { id: 'approval_one', agent: 'ops', session_id: 's', state: 'pending', reason: '变更需要审批', expires_ms: Date.now() + 600000, request: { profile: 'production', command: 'kubectl scale deployment/web -n app --replicas=2', purpose: '恢复容量' } }
async function settle() { for (let i = 0; i < 10; i++) { await Promise.resolve(); await nextTick() } }
async function mount(list = [pending]) {
  api.executionApprovals.mockResolvedValue({ approvals: list })
  props = reactive({ session: 's', actionable: true, disabled: false, refreshKey: 0 })
  decisions = []
  host = document.createElement('div'); document.body.append(host)
  app = createApp({ render: () => h(ExecutionApprovals, { ...props, onResolve: (d) => decisions.push(d) }) })
  app.mount(host); await settle()
}
function button(text) { return [...host.querySelectorAll('button')].find(b => b.textContent.trim() === text) }
beforeEach(() => vi.resetAllMocks())
afterEach(() => { app?.unmount(); host?.remove(); vi.unstubAllGlobals() })
describe('write approval controls', () => {
  it('shows exact command and emits only a decision bound to its stored id', async () => {
    await mount()
    expect(host.querySelector('pre').textContent).toBe(pending.request.command)
    expect(host.textContent).toContain('ops · production')
    button('批准并执行一次').click(); await settle()
    expect(decisions).toEqual([{ id: pending.id, approve: true }])
    button('拒绝').click(); await settle()
    expect(decisions[1]).toEqual({ id: pending.id, approve: false })
  })
  it('cannot approve expired or changed requests, or while a turn runs', async () => {
    await mount([{ ...pending, state: 'expired' }])
    expect(button('批准并执行一次')).toBeUndefined()
    expect(host.textContent).toContain('审批已过期')
    api.executionApprovals.mockResolvedValue({ approvals: [pending] })
    props.refreshKey++; props.disabled = true; await settle()
    expect(button('批准并执行一次').disabled).toBe(true)
    button('批准并执行一次').click(); expect(decisions).toEqual([])
  })
  it('renders command data as text and ignores a previous session response', async () => {
    let finish
    api.executionApprovals.mockImplementationOnce(() => new Promise(r => { finish = r }))
    await mount(); props.session = 'new-session'; await settle()
    const unsafe = '<img src=x onerror=alert(1)>'
    api.executionApprovals.mockResolvedValue({ approvals: [{ ...pending, request: { ...pending.request, command: unsafe } }] })
    props.refreshKey++; await settle()
    finish({ approvals: [{ ...pending, request: { ...pending.request, command: 'old-session-command' } }] }); await settle()
    expect(host.querySelector('pre').textContent).toBe(unsafe)
    expect(host.querySelector('img')).toBeNull()
    expect(host.textContent).not.toContain('old-session-command')
  })
  it('keeps the pending request as a card and folds settled history into rows', async () => {
    const done = (id, extra) => ({ ...pending, id, state: 'consumed', resolved_by: 'admin', resolved_ms: Date.now(), ...extra, request: { ...pending.request, command: `ls ${id}` } })
    await mount([pending, done('a', { outcome: 'failed' }), done('b', { outcome: 'succeeded' }), done('c', { state: 'rejected' }), done('d', { outcome: 'unknown' })])
    const cards = [...host.querySelectorAll('article')]
    expect(cards.map(c => c.querySelector('pre').textContent)).toEqual([pending.request.command, 'ls d'])
    const history = host.querySelector('details.history')
    expect(history.open).toBe(false)
    expect(history.querySelector('summary').textContent).toContain('3 条')
    expect([...history.querySelectorAll('.row code')].map(c => c.textContent)).toEqual(['ls a', 'ls b', 'ls c'])
    expect(button('批准并执行一次')).toBeDefined()
    expect(host.querySelectorAll('.actions').length).toBe(1)
  })
  it('reports API failures with a retry', async () => {
    api.executionApprovals.mockRejectedValueOnce(new Error('需要登录'))
    await mount()
    expect(host.querySelector('[role=alert]').textContent).toContain('需要登录')
    button('重新加载审批').click(); await settle()
    expect(host.querySelector('[role=alert]')).toBeNull()
  })
})
it('sends approval id and decision without a replacement command or payload', async () => {
  const fetch = vi.fn().mockResolvedValue({ ok: true, body: { getReader: () => ({ read: async () => ({ done: true }) }) } })
  vi.stubGlobal('fetch', fetch)
  await streamChat({ sessionId: 's', approvalId: pending.id, approve: false }, () => {})
  const body = JSON.parse(fetch.mock.calls[0][1].body)
  expect(body).toMatchObject({ session_id: 's', approval_id: pending.id, approve: false })
  expect(body).not.toHaveProperty('command')
  expect(body).not.toHaveProperty('payload')
})

it('refreshes a consumed approval until execution has an outcome', async () => {
  vi.useFakeTimers()
  try {
    api.executionApprovals.mockResolvedValueOnce({ approvals: [{ ...pending, state: 'consumed', outcome: '' }] })
    await mount([{ ...pending, state: 'consumed', outcome: 'succeeded' }])
    expect(host.textContent).toContain('执行结果未记录')
    await vi.advanceTimersByTimeAsync(3100)
    await settle()
    expect(host.textContent).toContain('执行成功')
    await vi.advanceTimersByTimeAsync(3100)
    expect(api.executionApprovals).toHaveBeenCalledTimes(2)
  } finally { vi.useRealTimers() }
})
