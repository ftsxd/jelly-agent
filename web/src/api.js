// API client for the jelly-agent backend. Relative URLs so it works both behind
// the embedded server and the Vite dev proxy.

// signal is optional and threaded through so a caller can abandon a request
// it no longer wants — switching sessions while one is in flight, mainly. A
// guard on the response alone leaves the request running and the connection
// held; aborting says so to the browser as well.
async function jget(path, signal) {
  const res = await fetch(path, signal ? { signal } : undefined)
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
  // What each tool is declared to be. The registry could always read these
  // from a file; this is the console's way in.
  toolMetadata: () => jget('/api/tools/metadata'),
  saveToolMetadata: (decl) => jpost('/api/tools/metadata', decl),
  testTool: (query, max) => jpost('/api/tools/test', { query, max }),
  fetchUrl: (url, maxChars) => jpost('/api/tools/fetch', { url, max_chars: maxChars }),
  history: () => jget('/api/history'),
  setHistory: (body) => jput('/api/history', body),
  sessions: (limit = 50, offset = 0) => jget(`/api/sessions?limit=${limit}&offset=${offset}`),
  sessionIds: () => jget('/api/sessions/ids'),
  session: (id, signal) => jget(`/api/sessions/${encodeURIComponent(id)}`, signal),
  // The run as a frame list, folded by timeline.js — the same vocabulary the
  // live stream sends, so replay and live share one reducer.
  // The fixed part of every prompt and what it costs. Answers "what do we
  // inject?", which the per-turn token figure cannot.
  prompt: (provider = '') => jget(`/api/prompt${provider ? `?provider=${encodeURIComponent(provider)}` : ''}`),
  sessionTimeline: (id, signal) => jget(`/api/sessions/${encodeURIComponent(id)}/timeline`, signal),

  // The task centre. A task is one invocation, so its id is the session and
  // the round joined — the same pair every backend table is keyed by.
  tasks: (params = {}, signal) => {
    const q = new URLSearchParams()
    for (const [k, v] of Object.entries(params)) if (v !== '' && v != null) q.set(k, v)
    const s = q.toString()
    return jget(`/api/tasks${s ? `?${s}` : ''}`, signal)
  },
  task: (session, round, signal) =>
    jget(`/api/tasks/${encodeURIComponent(session)}/${encodeURIComponent(round)}`, signal),

  // Reading a stored tool result, a window at a time. The endpoint has existed
  // since the delivery store was built and had no client until now, which is
  // why the console could show that a result was retrievable but not retrieve
  // it.
  //
  // Addressed by the delivery's handle (e7), not by the call id. A call id is
  // unique within one run; a task that folds a follow-up run holds two runs
  // whose first calls are both c1, and reading by call id served whichever the
  // database happened to return.
  readResult: (session, ref, { offset = 0, limit } = {}, signal) => {
    const q = new URLSearchParams({ offset: String(offset) })
    if (limit) q.set('limit', String(limit))
    return jget(
      `/api/sessions/${encodeURIComponent(session)}/results/${encodeURIComponent(ref)}?${q}`,
      signal,
    )
  },
  // Searching one instead of paging through it — the same Store.Search the
  // model's search_result tool uses, so the two report the same counts.
  searchResult: (session, ref, q, { limit, context } = {}, signal) => {
    const p = new URLSearchParams({ q })
    if (limit) p.set('limit', String(limit))
    if (context != null) p.set('context', String(context))
    return jget(
      `/api/sessions/${encodeURIComponent(session)}/results/${encodeURIComponent(ref)}/search?${p}`,
      signal,
    )
  },
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
export async function streamChat({ message, sessionId, provider, agent, taskId }, onEvent, signal) {
  const res = await fetch('/api/chat/stream', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      message, session_id: sessionId || '', provider: provider || '', agent: agent || '',
      // Carried so a follow-up joins the task it continues instead of opening
      // a second one that tells half the story.
      task_id: taskId || '',
    }),
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
