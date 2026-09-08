import { describe, it, expect } from 'vitest'
import { declPatch, declaredOf, draftOf, inheritedOf, parseLines } from '../toolmeta'

// A row as /api/tool-metadata answers it: top-level fields are what the
// registry resolved, `declared` is what the console file itself says.
const row = {
  name: 'query_instant',
  server: 'n9e-mcp',
  description: 'Run a PromQL instant query against a Prometheus-compatible datasource.',
  suites: ['promql'],
  declared: { suites: ['promql'] },
}

describe('draftOf', () => {
  it('fills the boxes from the declaration, not from the resolved value', () => {
    // The resolved description comes from the MCP server. Putting it in the
    // box is how it ended up frozen into the override file.
    expect(draftOf(row).description).toBe('')
    expect(draftOf(row).suites).toBe('promql')
  })

  it('leaves every box empty for a tool the console does not declare', () => {
    const d = draftOf({ name: 'x', description: 'from the server', suites: ['promql'] })
    expect(d).toEqual({
      description: '',
      use_cases: '',
      examples: '',
      anti_examples: '',
      suites: '',
    })
  })
})

describe('inheritedOf', () => {
  it('reports the effective value only for fields the console leaves alone', () => {
    const h = inheritedOf(row)
    expect(h.description).toBe(row.description)
    expect(h.suites).toBeUndefined() // declared, so it is in the box already
  })

  it('says nothing when neither side has a value', () => {
    expect(inheritedOf({ name: 'x' })).toEqual({})
  })
})

describe('declPatch', () => {
  const base = draftOf(row)

  it('is null when nothing moved, so no request is sent', () => {
    expect(declPatch({ ...base }, base)).toBeNull()
  })

  it('carries only the field that changed', () => {
    expect(declPatch({ ...base, use_cases: '查询瞬时指标' }, base)).toEqual({
      use_cases: ['查询瞬时指标'],
    })
  })

  it('does not resend an untouched field, so a stale page cannot clear it', () => {
    // This is the regression: a page loaded before the registry knew the
    // suites had an empty box, and saving a description wrote that emptiness
    // back over `suites: [promql]`.
    const stale = draftOf({ name: 'query_instant', server: 'n9e-mcp' })
    const patch = declPatch({ ...stale, description: '瞬时 PromQL 查询' }, stale)
    expect(patch).toEqual({ description: '瞬时 PromQL 查询' })
    expect(patch).not.toHaveProperty('suites')
  })

  it('does carry an intentional clear', () => {
    expect(declPatch({ ...base, suites: '' }, base)).toEqual({ suites: [] })
  })

  it('sees a reordering as a change', () => {
    const b = draftOf({ declared: { suites: ['a', 'b'] } })
    expect(declPatch({ ...b, suites: 'b\na' }, b)).toEqual({ suites: ['b', 'a'] })
  })
})

describe('parseLines', () => {
  it('drops blanks and trims, and tolerates a missing value', () => {
    expect(parseLines('  a \n\n b ')).toEqual(['a', 'b'])
    expect(parseLines(undefined)).toEqual([])
  })
})

describe('declaredOf', () => {
  it('treats an undeclared row as empty, never as the resolved value', () => {
    expect(declaredOf({ suites: ['promql'] }).suites).toEqual([])
  })
})
