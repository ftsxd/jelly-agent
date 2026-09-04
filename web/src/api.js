// API client for the jelly-agent backend. Relative URLs so it works both behind
// the embedded server and the Vite dev proxy.

async function jget(path) {
  const res = await fetch(path)
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
  return body
}

async function jpost(path, payload) {
  const res = await fetch(path, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
  return body
}

async function jput(path, payload) {
  const res = await fetch(path, {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(payload),
  })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
  return body
}

async function jdelete(path) {
  const res = await fetch(path, { method: 'DELETE' })
  const body = await res.json().catch(() => ({}))
  if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
  return body
}

export const api = {
  health: () => jget('/api/health'),
  authStatus: () => jget('/api/auth/status'),
  login: (username, password) => jpost('/api/auth/login', { username, password }),
  logout: () => jpost('/api/auth/logout', {}),
  changePassword: (currentPassword, newPassword) => jpost('/api/auth/password', { current_password: currentPassword, new_password: newPassword }),
  providers: () => jget('/api/providers'),
  saveProvider: (p) => jpost('/api/providers', p),
  deleteProvider: (name) => jdelete(`/api/providers/${encodeURIComponent(name)}`),
  tools: () => jget('/api/tools'),
  testTool: (query, max) => jpost('/api/tools/test', { query, max }),
  fetchUrl: (url, maxChars) => jpost('/api/tools/fetch', { url, max_chars: maxChars }),
  history: () => jget('/api/history'),
  setHistory: (body) => jput('/api/history', body),
  sessions: (limit = 50, offset = 0) => jget(`/api/sessions?limit=${limit}&offset=${offset}`),
  sessionIds: () => jget('/api/sessions/ids'),
  session: (id) => jget(`/api/sessions/${encodeURIComponent(id)}`),
  // The run as a frame list, folded by timeline.js — the same vocabulary the
  // live stream sends, so replay and live share one reducer.
  sessionTimeline: (id) => jget(`/api/sessions/${encodeURIComponent(id)}/timeline`),
  deleteSession: (id) => jdelete(`/api/sessions/${encodeURIComponent(id)}`),
  deleteSessions: (ids) => jpost('/api/sessions/delete', { ids }),
  skills: () => jget('/api/skills'),
  skill: (name) => jget(`/api/skills/${encodeURIComponent(name)}`),
  saveSkill: (p) => jpost('/api/skills', p),
  deleteSkill: (name) => jdelete(`/api/skills/${encodeURIComponent(name)}`),
  setAllowScripts: (enabled) => jpost('/api/skills/allow-scripts', { enabled }),
  sandbox: () => jget('/api/sandbox'),
  setSandbox: (p) => jpost('/api/sandbox', p),
  agents: () => jget('/api/agents'),
  saveAgent: (a) => jpost('/api/agents', a),
  deleteAgent: (name) => jdelete(`/api/agents/${encodeURIComponent(name)}`),
  setSkillVars: (name, vars) => jpost(`/api/skills/${encodeURIComponent(name)}/vars`, { vars }),
  deleteSkillVar: (name, key) => jdelete(`/api/skills/${encodeURIComponent(name)}/vars/${encodeURIComponent(key)}`),
  uploadSkill: async (file) => {
    const fd = new FormData()
    fd.append('file', file)
    const res = await fetch('/api/skills/upload', { method: 'POST', body: fd })
    const body = await res.json().catch(() => ({}))
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
    return body
  },
  memoryCore: () => jget('/api/memory/core'),
  setMemoryCore: (target, content) => jpost('/api/memory/core', { target, content }),
  memorySearch: (q) => jget(`/api/memory/search?q=${encodeURIComponent(q)}`),
  setMemorySearch: (payload) => jput('/api/memory/search', payload),
  stats: () => jget('/api/stats'),
  platforms: () => jget('/api/platforms'),
  savePlatform: (p) => jpost('/api/platforms', p),
  deletePlatform: (name) => jdelete(`/api/platforms/${encodeURIComponent(name)}`),
  mcp: () => jget('/api/mcp'),
  saveMCP: (s) => jpost('/api/mcp', s),
  testMCP: (s) => jpost('/api/mcp/test', s),
  deleteMCP: (name) => jdelete(`/api/mcp/${encodeURIComponent(name)}`),
  schedules: () => jget('/api/schedules'),
  saveSchedule: (task) => jpost('/api/schedules', task),
  deleteSchedule: (name) => jdelete(`/api/schedules/${encodeURIComponent(name)}`),
  runSchedule: (name) => jpost(`/api/schedules/${encodeURIComponent(name)}/run`, {}),
  scheduleRuns: (task = '') => jget(`/api/schedules/runs?task=${encodeURIComponent(task)}`),
}

// streamChat POSTs a message and parses the SSE response. onEvent receives each
// decoded event ({type, ...}). Returns a promise that resolves when the stream
// closes. Pass an AbortSignal to cancel mid-stream.
export async function streamChat({ message, sessionId, provider, agent }, onEvent, signal) {
  const res = await fetch('/api/chat/stream', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ message, session_id: sessionId || '', provider: provider || '', agent: agent || '' }),
    signal,
  })
  if (!res.ok) {
    const body = await res.json().catch(() => ({}))
    throw new Error(body.error || `HTTP ${res.status}`)
  }

  const reader = res.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    // SSE frames are separated by a blank line.
    let sep
    while ((sep = buf.indexOf('\n\n')) !== -1) {
      const frame = buf.slice(0, sep)
      buf = buf.slice(sep + 2)
      const line = frame.split('\n').find((l) => l.startsWith('data:'))
      if (!line) continue
      const json = line.slice(5).trim()
      if (!json) continue
      try {
        onEvent(JSON.parse(json))
        // A proxy/browser can deliver several SSE frames in one read. Yielding
        // here gives Vue a paint opportunity between text deltas instead of
        // rendering the whole accumulated answer in one visual update.
        await new Promise((resolve) => setTimeout(resolve, 0))
      } catch {
        /* ignore malformed frame */
      }
    }
  }
}
