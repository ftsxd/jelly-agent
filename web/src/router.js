import { createRouter, createWebHashHistory } from 'vue-router'

// Hash history: the embedded Go server only serves index.html + assets, so
// hash routing avoids needing server-side deep-link rewrites.
const routes = [
  { path: '/', redirect: '/chat' },
  { path: '/chat', name: 'chat', component: () => import('./views/ChatView.vue'), meta: { title: '对话', icon: 'chat' } },
  { path: '/tools', name: 'tools', component: () => import('./views/ToolsView.vue'), meta: { title: '工具', icon: 'tool' } },
  { path: '/sessions', name: 'sessions', component: () => import('./views/SessionsView.vue'), meta: { title: '会话', icon: 'sessions' } },
  { path: '/monitor', name: 'monitor', component: () => import('./views/MonitorView.vue'), meta: { title: '监控', icon: 'chart' } },
  { path: '/memory', name: 'memory', component: () => import('./views/MemoryView.vue'), meta: { title: '记忆', icon: 'memory' } },
  { path: '/skills', name: 'skills', component: () => import('./views/SkillsView.vue'), meta: { title: '技能', icon: 'book' } },
  { path: '/agents', name: 'agents', component: () => import('./views/AgentsView.vue'), meta: { title: 'Agent', icon: 'bot' } },
  { path: '/mcp', name: 'mcp', component: () => import('./views/McpView.vue'), meta: { title: 'MCP', icon: 'plug' } },
  { path: '/messaging', name: 'messaging', component: () => import('./views/MessagingView.vue'), meta: { title: '消息绑定', icon: 'message' } },
  { path: '/config', name: 'config', component: () => import('./views/ConfigView.vue'), meta: { title: '配置', icon: 'settings' } },
]

export const navItems = routes.filter((r) => r.meta)

export default createRouter({
  history: createWebHashHistory(),
  routes,
})
