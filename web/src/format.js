// Formatting for timeline steps.
//
// Pure functions, in their own module for the same reason as the reducer:
// ChatView and SessionsView each grew a copy of fmtArgs and resultSummary, and
// the copies drifted. One of them decided a tool was still running whenever
// its response was empty — so a tool that legitimately answers nothing
// displayed "执行中…" forever.
//
// The rule that replaces it: success is the server's judgement, carried on the
// step as `status`. Nothing here re-derives it.

/** Tool arguments as a compact `k=v, k=v` line. */
export function fmtArgs(args, max = 120) {
  if (!args || typeof args !== 'object') return ''
  const parts = []
  for (const [k, v] of Object.entries(args)) {
    parts.push(`${k}=${compact(v)}`)
  }
  return truncate(parts.join(', '), max)
}

/** A step's result in one line, driven by status rather than by content. */
export function resultSummary(step, max = 160) {
  if (!step || step.kind !== 'tool') return ''
  switch (step.status) {
    case 'pending':
      return '执行中…'
    case 'failed':
      return step.error || '调用失败'
    default:
      return truncate(summarizeResponse(step.response), max)
  }
}

// summarizeResponse prefers the gateway's own summary. Every gatewayed tool
// answers {evidence_id, summary, data, truncated}, and that summary was written
// to be read — a re-serialized data blob is not.
function summarizeResponse(resp) {
  if (resp === null || resp === undefined) return '已完成'
  if (typeof resp !== 'object') return String(resp)
  if (typeof resp.summary === 'string' && resp.summary !== '') return resp.summary
  const data = 'data' in resp ? resp.data : resp
  if (data === null || data === undefined) return '已完成'
  if (typeof data !== 'object') return String(data)
  const keys = Object.keys(data)
  if (keys.length === 0) return '已完成'
  return keys.map((k) => `${k}=${compact(data[k])}`).join(', ')
}

/** Evidence id, when the tool went through the gateway. */
export function evidenceId(step) {
  const r = step?.response
  return r && typeof r === 'object' && typeof r.evidence_id === 'string' ? r.evidence_id : ''
}

/** True when the gateway had to cut the payload down. */
export function isTruncated(step) {
  return !!(step?.response && typeof step.response === 'object' && step.response.truncated)
}

/** Full payload, pretty-printed, for the expanded view. */
export function prettyJSON(value) {
  if (value === null || value === undefined) return ''
  try {
    return JSON.stringify(value, null, 2)
  } catch {
    return String(value)
  }
}

/** Token counts as a short label: 1.2k rather than 1234. */
export function fmtTokens(n) {
  if (!n) return '0'
  if (n < 1000) return String(n)
  return `${(n / 1000).toFixed(n < 10000 ? 1 : 0)}k`
}

/**
 * Elapsed time between two server timestamps, in milliseconds.
 *
 * Both arguments must come from the server. The client's clock never
 * participates, so a skewed browser cannot produce a negative or absurd
 * duration.
 *
 * Note that tool steps do not have one: ADK reuses the first tool's timestamp
 * for a merged parallel response, so the gap between a call and its result
 * describes neither the tool's runtime nor the delivery gap. Real per-call
 * timing arrives from tool_calls later.
 */
export function fmtDuration(ms) {
  if (typeof ms !== 'number' || !Number.isFinite(ms) || ms < 0) return ''
  if (ms < 1000) return `${Math.round(ms)}ms`
  if (ms < 60000) return `${(ms / 1000).toFixed(1)}s`
  const m = Math.floor(ms / 60000)
  return `${m}m${Math.round((ms % 60000) / 1000)}s`
}

/** A step's label for the timeline row. */
export function stepLabel(step) {
  switch (step?.kind) {
    case 'tool':
      return step.name || '（未命名工具）'
    case 'transfer':
      return `${step.from || '?'} → ${step.to || '?'}`
    case 'thought':
      return '思考'
    case 'error':
      return '错误'
    default:
      return step?.agent || ''
  }
}

function compact(v) {
  if (v === null || v === undefined) return 'null'
  if (typeof v === 'string') return truncate(v, 40)
  if (typeof v === 'object') {
    try {
      return truncate(JSON.stringify(v), 40)
    } catch {
      return '[…]'
    }
  }
  return String(v)
}

function truncate(s, max) {
  const str = String(s)
  return [...str].length > max ? `${[...str].slice(0, max).join('')}…` : str
}
