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
  providers: () => jget('/api/providers'),
  saveProvider: (p) => jpost('/api/providers', p),
  deleteProvider: (name) => jdelete(`/api/providers/${encodeURIComponent(name)}`),
  tools: () => jget('/api/tools'),
  testTool: (query, max) => jpost('/api/tools/test', { query, max }),
  sessions: () => jget('/api/sessions'),
  session: (id) => jget(`/api/sessions/${encodeURIComponent(id)}`),
  deleteSession: (id) => jdelete(`/api/sessions/${encodeURIComponent(id)}`),
  skills: () => jget('/api/skills'),
  skill: (name) => jget(`/api/skills/${encodeURIComponent(name)}`),
  saveSkill: (p) => jpost('/api/skills', p),
  deleteSkill: (name) => jdelete(`/api/skills/${encodeURIComponent(name)}`),
  uploadSkill: async (file) => {
    const fd = new FormData()
    fd.append('file', file)
    const res = await fetch('/api/skills/upload', { method: 'POST', body: fd })
    const body = await res.json().catch(() => ({}))
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`)
    return body
  },
  memoryCore: () => jget('/api/memory/core'),
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
}

// streamChat POSTs a message and parses the SSE response. onEvent receives each
// decoded event ({type, ...}). Returns a promise that resolves when the stream
// closes. Pass an AbortSignal to cancel mid-stream.
export async function streamChat({ message, sessionId, provider }, onEvent, signal) {
  const res = await fetch('/api/chat/stream', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ message, session_id: sessionId || '', provider: provider || '' }),
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
      } catch {
        /* ignore malformed frame */
      }
    }
  }
}
