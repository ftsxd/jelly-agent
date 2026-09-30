// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApp, nextTick } from 'vue'
import ExecutionSettings from '../components/ExecutionSettings.vue'
import { api } from '../api'
vi.mock('../api', () => ({ api: { execution: vi.fn(), agents: vi.fn(), setExecution: vi.fn(), checkExecution: vi.fn() } }))
let app, host
const data = { config: { enabled: false, profiles: [] }, defaults: { timeout_sec: 30, max_output_kb: 64 }, os_detail: '系统沙箱' }
async function settle() { for (let i = 0; i < 8; i++) { await Promise.resolve(); await nextTick() } }
function button(text) { return [...host.querySelectorAll('button')].find(b => b.textContent.trim() === text) }
async function mount(config = data.config) {
  api.execution.mockResolvedValue({ ...data, config }); api.agents.mockResolvedValue({ agents: [{ name: 'expert' }] })
  host = document.createElement('div'); document.body.append(host); app = createApp(ExecutionSettings); app.mount(host); await settle()
}
async function fill(label, value) {
  const el = [...host.querySelectorAll('label')].find(l => l.textContent.trim().startsWith(label)).querySelector('input,textarea,select')
  el.value = value; el.dispatchEvent(new Event(el.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true })); await settle()
}
beforeEach(() => vi.resetAllMocks())
afterEach(() => { app?.unmount(); host?.remove() })
describe('execution management', () => {
  it('does not assign a new profile implicitly', async () => {
    await mount(); button('添加执行配置').click(); await settle(); await fill('配置名称', 'readonly')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution).toHaveBeenCalledWith(expect.objectContaining({ enabled: false, profiles: [{ name: 'readonly', agents: [], network: false, env: {}, rules: [] }] }))
  })
  it('checks saved policy without calling an execution endpoint', async () => {
    await mount({ enabled: true, profiles: [{ name: 'cloud', agents: ['root'], network: true }] })
    api.checkExecution.mockResolvedValue({ decision: 'prompt', segments: [['kubectl', 'scale']], reason: '需要审批' })
    await fill('诊断命令', 'kubectl scale deployment/x --replicas=2')
    host.querySelector('.probe form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.checkExecution).toHaveBeenCalledWith(expect.objectContaining({ agent: 'root', profile: 'cloud', command: 'kubectl scale deployment/x --replicas=2' }))
    expect(host.textContent).toContain('需要审批，当前不执行'); expect(api.setExecution).not.toHaveBeenCalled()
  })
  it('preserves credential references and exact token patterns when saving', async () => {
    await mount({ enabled: true, profiles: [{ name: 'cloud', agents: ['expert'], network: false, env: { TOKEN: 'SERVER_TOKEN' }, rules: [{ name: 'literal', pattern: ['echo', 'a b'], decision: 'allow' }] }] })
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution.mock.calls[0][0].profiles[0]).toEqual({ name: 'cloud', agents: ['expert'], network: false, env: { TOKEN: 'SERVER_TOKEN' }, rules: [{ name: 'literal', pattern: ['echo', 'a b'], decision: 'allow', reason: '' }] })
  })
  it('shows save failures instead of claiming configuration is active', async () => {
    await mount(); api.setExecution.mockRejectedValue(new Error('执行后端无效'))
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(host.querySelector('[role=alert]').textContent).toContain('执行后端无效'); expect(host.querySelector('[role=status]')).toBeNull()
  })
  it('preserves agent variable references and the installed CLI runtime on save', async () => {
    await mount({ enabled: true, backend: 'os', profiles: [{ name: 'cloud', agents: ['expert'], network: true,
      tool_dir: '/opt/sre-tools', agent_env: { TOKEN: 'CLOUD_READ_KEY' }, write_approval: true, write_agent_env: { TOKEN: 'CLOUD_WRITE_KEY' } }] })
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution.mock.calls[0][0].profiles[0]).toMatchObject({ tool_dir: '/opt/sre-tools', agent_env: { TOKEN: 'CLOUD_READ_KEY' }, write_agent_env: { TOKEN: 'CLOUD_WRITE_KEY' } })
  })
  it('injects saved agent variables by default, flags misplaced ones, and keeps the escape hatch on save', async () => {
    api.agents.mockResolvedValue({ agents: [{ name: 'expert' }], var_keys: { expert: ['CLOUD_ID', 'CLOUD_KEY'] } })
    api.execution.mockResolvedValue({ ...data, config: { enabled: true, profiles: [{ name: 'cloud', agents: ['expert'], network: true, allow_unconfined_with_approval: true, write_approval: true, write_env: { CLOUD_ID: 'CLOUD_ID' } }] } })
    host = document.createElement('div'); document.body.append(host); app = createApp(ExecutionSettings); app.mount(host); await settle()
    expect(host.querySelector('.issues').textContent).toContain('服务端环境变量 CLOUD_ID')
    expect(host.textContent).toContain('每次执行注入：CLOUD_ID、CLOUD_KEY')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    const saved = api.setExecution.mock.calls[0][0].profiles[0]
    expect(saved).toMatchObject({ allow_unconfined_with_approval: true, write_env: { CLOUD_ID: 'CLOUD_ID' } })
    expect(saved).not.toHaveProperty('inherit_agent_vars')
    expect(saved).not.toHaveProperty('agent_env')
  })
  it('can turn inheritance off and reference saved variables explicitly', async () => {
    api.agents.mockResolvedValue({ agents: [{ name: 'expert' }], var_keys: { expert: ['CLOUD_ID', 'CLOUD_KEY'] } })
    api.execution.mockResolvedValue({ ...data, config: { enabled: true, profiles: [{ name: 'cloud', agents: ['expert'], network: true }] } })
    host = document.createElement('div'); document.body.append(host); app = createApp(ExecutionSettings); app.mount(host); await settle()
    const inherit = [...host.querySelectorAll('label')].find(l => l.textContent.includes('自动注入分配的 Agent')).querySelector('input')
    inherit.checked = false; inherit.dispatchEvent(new Event('change', { bubbles: true })); await settle()
    button('引用已保存的变量（CLOUD_ID、CLOUD_KEY）').click(); await settle()
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution.mock.calls[0][0].profiles[0]).toMatchObject({ inherit_agent_vars: false, agent_env: { CLOUD_ID: 'CLOUD_ID', CLOUD_KEY: 'CLOUD_KEY' } })
  })
  it('preserves separate write credential references and can revoke write approval', async () => {
    await mount({ enabled: true, profiles: [{ name: 'write', agents: ['expert'], network: true, write_approval: true, env: { TOKEN: 'READ_SOURCE' }, write_env: { TOKEN: 'WRITE_SOURCE' }, write_kubeconfig_env: 'WRITE_KUBE' }] })
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution.mock.calls[0][0].profiles[0]).toMatchObject({ write_approval: true, write_env: { TOKEN: 'WRITE_SOURCE' }, write_kubeconfig_env: 'WRITE_KUBE', env: { TOKEN: 'READ_SOURCE' } })
    const checkbox = [...host.querySelectorAll('label')].find(l => l.textContent.includes('允许申请写操作审批')).querySelector('input')
    checkbox.checked = false; checkbox.dispatchEvent(new Event('change', { bubbles: true })); await settle()
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.setExecution.mock.calls[1][0].profiles[0]).not.toHaveProperty('write_env')
    expect(api.setExecution.mock.calls[1][0].profiles[0]).not.toHaveProperty('write_approval')
  })
})
