// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApp, nextTick } from 'vue'
import CodeProjects from '../components/CodeProjects.vue'
import { api } from '../api'
vi.mock('../api', () => ({ api: {
  codeProjects: vi.fn(), agents: vi.fn(), saveCodeProject: vi.fn(),
  grantCodeProject: vi.fn(), syncCodeProject: vi.fn(), deleteCodeProject: vi.fn(),
  checkCodeProjectUpdate: vi.fn(), setCodeProjectAnnotation: vi.fn(), resolveCodeProjectDrafts: vi.fn(),
  annotateCodeProject: vi.fn(), approveCodeProjectSync: vi.fn(), rejectCodeProjectSync: vi.fn(),
} }))
let app, host
const project = { id: 'orders', name: '订单服务', url: 'https://git.example.com/orders.git', branch: 'main', grants: [] }
const settle = async () => { for (let i = 0; i < 8; i++) { await Promise.resolve(); await nextTick() } }
function button(text) { return [...host.querySelectorAll('button')].find(b => b.textContent.trim() === text) }
async function click(text) { button(text).click(); await settle() }
// Some buttons carry a count or badge inside them, so the label is a prefix.
function buttonStarting(text) { return [...host.querySelectorAll('button')].find(b => b.textContent.trim().startsWith(text)) }
async function clickStarting(text) { buttonStarting(text).click(); await settle() }
async function input(label, value) {
  const el = [...host.querySelectorAll('label')].find(l => l.textContent.trim().startsWith(label)).querySelector('input, select')
  el.value = value; el.dispatchEvent(new Event(el.tagName === 'SELECT' ? 'change' : 'input', { bubbles: true })); await settle()
}
async function mount(projects = [project]) {
  api.codeProjects.mockResolvedValue({ projects }); api.agents.mockResolvedValue({ agents: [{ name: 'analyst', enabled: true }] })
  host = document.createElement('div'); document.body.append(host); app = createApp(CodeProjects); app.mount(host); await settle()
}
beforeEach(() => { vi.resetAllMocks() })
afterEach(() => { app?.unmount(); host?.remove(); vi.useRealTimers() })
describe('code project management', () => {
  it('creates a whole-repo project without implicitly assigning an agent', async () => {
    await mount([]); await click('新建第一个项目')
    await input('项目名称', '订单服务'); await input('项目标识', 'orders'); await input('Git 仓库地址', project.url)
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.saveCodeProject).toHaveBeenCalledWith({
      id: 'orders', name: '订单服务', url: project.url, branch: 'main', history_depth: 0, token_env: '', username: '',
      root_path: '.', reference_paths: [], directory_metadata: {},
    })
    expect(api.grantCodeProject).not.toHaveBeenCalled()
    expect(api.syncCodeProject).not.toHaveBeenCalled()
  })
  it('saves the analysis directories with their business names, and syncs on demand', async () => {
    await mount([]); await click('新建第一个项目')
    await input('项目名称', 'Silkworm 优惠券'); await input('项目标识', 'silkworm-coupon'); await input('Git 仓库地址', project.url)
    await input('目录路径', 'services/discount_coupon'); await input('业务名称', '优惠券服务')
    await click('添加参考目录')
    const paths = [...host.querySelectorAll('label')].filter(l => l.textContent.trim().startsWith('目录路径'))
    const ref = paths[1].querySelector('input')
    ref.value = 'common'; ref.dispatchEvent(new Event('input', { bubbles: true })); await settle()
    await click('保存并同步')
    expect(api.saveCodeProject).toHaveBeenCalledWith(expect.objectContaining({
      root_path: 'services/discount_coupon',
      reference_paths: ['common'],
      directory_metadata: { 'services/discount_coupon': { name: '优惠券服务', description: '' } },
    }))
    expect(api.syncCodeProject).toHaveBeenCalledWith('silkworm-coupon')
  })
  // How far back git log can look is a property of the repository, not of the
  // server, so it is set here. Blank has to stay blank: a 0 typed by the form
  // on every unrelated edit would read as a deliberate depth of zero.
  it('sends the history depth only as a number the operator actually chose', async () => {
    await mount([]); await click('新建第一个项目')
    await input('项目名称', 'silkworm'); await input('项目标识', 'silkworm'); await input('Git 仓库地址', project.url)
    await input('保留提交历史', '300')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.saveCodeProject).toHaveBeenCalledWith(expect.objectContaining({ history_depth: 300 }))
  })

  // Empty means the whole repository, the same as ".". Marking the field
  // required contradicted the backend, which already reads an empty root_path
  // as whole-repo, and blocked the form on a value the user had no reason to
  // type.
  it('treats an empty main directory as whole-repo instead of blocking the form', async () => {
    await mount([]); await click('新建第一个项目')
    await input('项目名称', 'silkworm'); await input('项目标识', 'silkworm'); await input('Git 仓库地址', project.url)
    await input('目录路径', '')
    const path = [...host.querySelectorAll('label')].find(l => l.textContent.trim().startsWith('目录路径')).querySelector('input')
    expect(path.required).toBe(false)
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.saveCodeProject).toHaveBeenCalledWith(expect.objectContaining({ root_path: '.' }))
  })
  // The field can never be prefilled, so an untouched one must not be read as
  // "clear it" — otherwise every unrelated edit silently drops the credential.
  it('sends the token only when typed, and clearing is explicit', async () => {
    await mount([{ ...project, has_token: true }])
    await click('编辑')
    const tokenInput = [...host.querySelectorAll('label')].find(l => l.textContent.trim().startsWith('访问令牌')).querySelector('input')
    expect(tokenInput.type).toBe('password')
    expect(tokenInput.value).toBe('')
    expect(tokenInput.placeholder).toContain('已保存')

    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    const sent = api.saveCodeProject.mock.calls[0][0]
    expect(sent.token).toBeUndefined()
    expect(sent.clear_token).toBeUndefined()

    await click('编辑'); await click('清除令牌')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.saveCodeProject).toHaveBeenLastCalledWith(expect.objectContaining({ clear_token: true }))
  })
  // Annotations are edited one row at a time: a monorepo project holds one per
  // service, and resending the whole catalogue to rename one is how the rest
  // quietly disappear.
  it('edits one annotation row without resending the catalogue', async () => {
    const annotated = { ...project, directory_metadata: {
      'services/order': { name: '订单服务', tags: ['核心链路'] },
      'services/pay': { name: '支付服务' },
    } }
    await mount([annotated]); await clickStarting('目录标注')
    expect(host.textContent).toContain('services/order')
    expect(host.textContent).toContain('支付服务')

    await input('目录路径', 'services/order')
    await input('业务名称', '订单中心')
    await input('标签', '核心链路, 订单')
    await click('更新这条标注')
    expect(api.setCodeProjectAnnotation).toHaveBeenCalledWith('orders', 'services/order', {
      name: '订单中心', description: '', tags: ['核心链路', '订单'],
    })
  })
  // Agent-written labels stay suggestions until a person accepts them.
  it('reviews agent drafts before they count', async () => {
    const withDrafts = { ...project, directory_drafts: {
      'services/order': { name: '订单服务（草稿）', description: '下单与状态流转。' },
    } }
    await mount([withDrafts]); await clickStarting('目录标注')
    expect(host.textContent).toContain('1 条 Agent 草稿待审核')
    expect(host.textContent).toContain('采纳前不会被任何分析读到')

    await click('采纳')
    expect(api.resolveCodeProjectDrafts).toHaveBeenCalledWith('orders', { accept: ['services/order'] })

    await click('全部丢弃')
    expect(api.resolveCodeProjectDrafts).toHaveBeenLastCalledWith('orders', { reject: ['services/order'] })
  })
  // Generating labels reads across the whole repository, so the cost is stated
  // before the click rather than discovered on the bill.
  it('warns about token cost and requires an assigned agent before generating labels', async () => {
    await mount([project]) // no grants
    await click('自动生成标注')
    expect(host.textContent).toContain('消耗较多 token')
    expect(host.textContent).toContain('还没分配 Agent')
    expect(button('知道了，开始生成').disabled).toBe(true)
    expect(api.annotateCodeProject).not.toHaveBeenCalled()

    api.codeProjects.mockResolvedValue({ projects: [{ ...project, grants: [{ agent: 'analyst' }] }] })
    await click('刷新'); await click('自动生成标注')
    expect(button('知道了，开始生成').disabled).toBe(false)
    api.annotateCodeProject.mockResolvedValue({ ok: true, agent: 'analyst' })
    await click('知道了，开始生成')
    expect(api.annotateCodeProject).toHaveBeenCalledWith('orders')
  })
  it('shows a running generation as busy and reports the outcome', async () => {
    await mount([{ ...project, grants: [{ agent: 'analyst' }], annotate: { state: 'running' } }])
    expect(button('生成标注中…').disabled).toBe(true)

    await mount([{ ...project, annotate: { state: 'succeeded', proposed: 42 } }])
    expect(host.textContent).toContain('新增 42 条草稿待审核')

    await mount([{ ...project, annotate: { state: 'failed', error: '模型调用超时' } }])
    expect(host.textContent).toContain('标注生成失败：模型调用超时')
  })
  it('assigns one agent with no expiry, and can unassign', async () => {
    await mount(); await click('分配 Agent'); await input('负责 Agent', 'analyst')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.grantCodeProject).toHaveBeenCalledWith('orders', [{ agent: 'analyst' }])
    api.codeProjects.mockResolvedValue({ projects: [{ ...project, grants: [{ agent: 'analyst' }] }] })
    await click('刷新'); await click('分配 Agent'); await input('负责 Agent', '')
    host.querySelector('form').dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); await settle()
    expect(api.grantCodeProject).toHaveBeenLastCalledWith('orders', [])
  })
  it('keeps showing a server-side sync as busy so the button cannot start a second pull', async () => {
    await mount([{ ...project, sync_state: 'running' }])
    expect(host.textContent).toContain('拉取代码中')
    expect(button('同步中…').disabled).toBe(true)
  })
  it('reports directories missing from the synced code', async () => {
    await mount([{ ...project, synced_at: '2026-09-15T00:00:00Z', root_path: 'services/gone', dir_issues: ['services/gone（不存在）'] }])
    expect(host.textContent).toContain('services/gone（不存在）')
  })
  it('prevents duplicate syncs and reports a rejected request', async () => {
    let reject
    api.syncCodeProject.mockReturnValue(new Promise((_, r) => { reject = r }))
    await mount(); await click('首次同步')
    expect(button('同步中…').disabled).toBe(true)
    reject(new Error('仓库无法访问')); await settle()
    expect(host.querySelector('[role="alert"]').textContent).toContain('仓库无法访问')
    expect(button('首次同步').disabled).toBe(false)
  })

  // An agent can say the snapshot is stale; pulling is still a person's click.
  it('shows an agent sync request with its reason, and only syncs when approved', async () => {
    const asked = {
      ...project, synced_at: new Date().toISOString(), revision: 'e33c30622411aaa',
      sync_request: { agent: 'CodeAnalyzer', reason: '要回答最近的改动', remote_revision: '5f2720774f4bbbb', local_revision: 'e33c30622411aaa', requested_at: new Date().toISOString() },
    }
    await mount([asked])
    expect(host.textContent).toContain('CodeAnalyzer')
    expect(host.textContent).toContain('要回答最近的改动')
    expect(host.textContent).toContain('5f2720774f4b')

    api.rejectCodeProjectSync.mockResolvedValue({ ok: true })
    await click('忽略')
    expect(api.rejectCodeProjectSync).toHaveBeenCalledWith('orders')
    expect(api.syncCodeProject).not.toHaveBeenCalled()

    await mount([asked])
    api.approveCodeProjectSync.mockResolvedValue({ ok: true, task_id: 't1' })
    await click('同意并同步')
    expect(api.approveCodeProjectSync).toHaveBeenCalledWith('orders')
  })
})
