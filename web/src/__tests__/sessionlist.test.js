// @vitest-environment jsdom
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { createApp, h, nextTick, reactive } from 'vue'
import SessionList from '../components/SessionList.vue'
import { api } from '../api'

vi.mock('../api', () => ({ api: { sessions: vi.fn(), sessionIds: vi.fn(), deleteSession: vi.fn(), deleteSessions: vi.fn() } }))
let app, host, props, events
const rows = [
  { id: 'web-1', preview: '查 CLS 主题', events: 12, last_update: 1790000000 },
  { id: 'web-2', preview: '巡检 Pod', events: 4, last_update: 1790000100 },
]
async function settle() { for (let i = 0; i < 8; i++) { await Promise.resolve(); await nextTick() } }
async function mount(extra = {}) {
  api.sessions.mockResolvedValue({ sessions: rows, total: 2, has_more: false })
  props = reactive({ selected: 'web-2', disabled: false, refreshKey: 0, ...extra })
  events = []
  host = document.createElement('div'); document.body.append(host)
  app = createApp({ render: () => h(SessionList, { ...props, onOpen: (id) => events.push(['open', id]), onDeleted: (ids) => events.push(['deleted', ids]) }) })
  app.mount(host); await settle()
}
beforeEach(() => { vi.resetAllMocks(); vi.stubGlobal('confirm', () => true) })
afterEach(() => { app?.unmount(); host?.remove(); vi.unstubAllGlobals() })

describe('session list beside the conversation', () => {
  it('lists sessions, marks the open one, and reports picks without opening anything itself', async () => {
    await mount()
    const items = [...host.querySelectorAll('.sess')]
    expect(items.map(i => i.querySelector('.sess-title').textContent)).toEqual(['查 CLS 主题', '巡检 Pod'])
    expect(items[1].classList.contains('active')).toBe(true)
    items[0].click(); await settle()
    expect(events).toEqual([['open', 'web-1']])
  })
  it('ignores picks while a turn is running', async () => {
    await mount({ disabled: true })
    host.querySelector('.sess').click(); await settle()
    expect(events).toEqual([])
  })
  it('filters loaded rows and batch-deletes the ticked ones', async () => {
    await mount()
    const search = host.querySelector('.list-search input')
    search.value = 'pod'; search.dispatchEvent(new Event('input')); await settle()
    expect(host.querySelectorAll('.sess')).toHaveLength(1)
    const pick = host.querySelector('.sess .pick'); pick.checked = true; pick.dispatchEvent(new Event('change')); await settle()
    api.deleteSessions.mockResolvedValue({})
    ;[...host.querySelectorAll('button')].find(b => b.textContent.includes('删除选中')).click(); await settle()
    expect(api.deleteSessions).toHaveBeenCalledWith(['web-2'])
    expect(events).toContainEqual(['deleted', ['web-2']])
  })
  it('reloads when the chat page bumps its refresh key', async () => {
    await mount()
    props.refreshKey++; await settle()
    expect(api.sessions).toHaveBeenCalledTimes(2)
  })
})
