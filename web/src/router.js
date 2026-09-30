import { createRouter, createWebHashHistory } from 'vue-router'

// Hash history: the embedded Go server only serves index.html + assets, so
// hash routing avoids needing server-side deep-link rewrites.
const routes = [
  { path: '/', redirect: '/chat' },
  // group orders the sidebar: what you use daily first, setup after.
  { path: '/chat', name: 'chat', component: () => import('./views/ChatView.vue'), meta: { title: '对话', icon: 'chat', group: '使用' } },
  { path: '/tasks', name: 'tasks', component: () => import('./views/TasksView.vue'), meta: { title: '执行记录', icon: 'spark', group: '使用' } },
  // Sessions are listed beside the conversation now; old links still land there.
  { path: '/sessions', redirect: { path: '/chat', query: { sessions: '1' } } },
  { path: '/schedules', name: 'schedules', component: () => import('./views/SchedulesView.vue'), meta: { title: '周期任务', icon: 'chart', group: '使用' } },
  { path: '/agents', name: 'agents', component: () => import('./views/AgentsView.vue'), meta: { title: 'Agent', icon: 'bot', group: '能力' } },
  { path: '/skills', name: 'skills', component: () => import('./views/SkillsView.vue'), meta: { title: '技能', icon: 'book', group: '能力' } },
  { path: '/tools', name: 'tools', component: () => import('./views/ToolsView.vue'), meta: { title: '工具', icon: 'tool', group: '能力' } },
  { path: '/mcp', name: 'mcp', component: () => import('./views/McpView.vue'), meta: { title: 'MCP', icon: 'plug', group: '能力' } },
  { path: '/code', name: 'code', component: () => import('./views/CodeView.vue'), meta: { title: '代码', icon: 'code', group: '能力' } },
  { path: '/memory', name: 'memory', component: () => import('./views/MemoryView.vue'), meta: { title: '记忆', icon: 'memory', group: '能力' } },
  { path: '/config', name: 'config', component: () => import('./views/ConfigView.vue'), meta: { title: '模型 Provider', icon: 'settings', group: '接入' } },
  { path: '/messaging', name: 'messaging', component: () => import('./views/MessagingView.vue'), meta: { title: '消息绑定', icon: 'message', group: '接入' } },
  { path: '/monitor', name: 'monitor', component: () => import('./views/MonitorView.vue'), meta: { title: '用量统计', icon: 'chart', group: '系统' } },
]

export const navItems = routes.filter((r) => r.meta)

// Sidebar sections in first-appearance order, each with its routes.
export const navGroups = navItems.reduce((groups, item) => {
  const last = groups[groups.length - 1]
  if (last && last.title === item.meta.group) last.items.push(item)
  else groups.push({ title: item.meta.group, items: [item] })
  return groups
}, [])

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
