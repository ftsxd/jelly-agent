// Tool metadata editing: what the console declares versus what applies.
//
// A row from /api/tool-metadata carries two different things. The top-level
// fields are what the registry *resolved* — the MCP server's own description,
// another file's declaration, a builtin's defaults. `declared` is what the
// console's own override file says, and only that.
//
// The editor must be filled from `declared`. Filling it from the resolved
// value made merely opening and saving an editor rewrite history: the MCP
// server's English description got frozen into the override file, and a field
// nobody touched got written back from whatever the page was showing — which,
// if the page had been loaded before a registry reload, was nothing at all.

// declaredOf is the console's own declaration for a row, with every field
// present. A row the console does not declare reads as empty, not as absent:
// the editor's boxes start empty because this file overrides nothing yet.
export function declaredOf(row) {
  const d = (row && row.declared) || {}
  return {
    description: d.description || '',
    use_cases: d.use_cases || [],
    examples: d.examples || [],
    anti_examples: d.anti_examples || [],
    suites: d.suites || [],
  }
}

// draftOf turns a declaration into the textareas' contents.
export function draftOf(row) {
  const d = declaredOf(row)
  return {
    description: d.description,
    use_cases: d.use_cases.join('\n'),
    examples: d.examples.join('\n'),
    anti_examples: d.anti_examples.join('\n'),
    suites: d.suites.join('\n'),
  }
}

// inheritedOf is what applies to a field that the console does not declare.
//
// Shown next to the box so the operator can see the effective value without
// it being in the box — a value in the box is a value that gets saved, and
// that is how an inherited description became an override.
export function inheritedOf(row) {
  const decl = declaredOf(row)
  const out = {}
  if (!decl.description && row?.description) out.description = row.description
  for (const f of ['use_cases', 'examples', 'anti_examples', 'suites']) {
    if (decl[f].length === 0 && (row?.[f] || []).length > 0) out[f] = row[f].join('、')
  }
  return out
}

export function parseLines(text) {
  return String(text ?? '')
    .split('\n')
    .map((l) => l.trim())
    .filter(Boolean)
}

// declPatch is the fields the operator actually changed, and nothing else.
//
// The server treats an absent field as "leave it alone", which is only useful
// if the page omits the fields it has nothing to say about. Sending all five
// every time is sending a snapshot, and a snapshot taken from a stale page
// overwrites whatever changed underneath it — the same failure the two
// dropdowns already had, so the same rule applies here.
//
// Returns null when nothing changed, so the caller can skip the request
// rather than write the file for no reason.
export function declPatch(draft, baseline) {
  const patch = {}
  if (draft.description.trim() !== baseline.description.trim()) {
    patch.description = draft.description.trim()
  }
  for (const f of ['use_cases', 'examples', 'anti_examples', 'suites']) {
    const next = parseLines(draft[f])
    const prev = parseLines(baseline[f])
    if (next.length !== prev.length || next.some((v, i) => v !== prev[i])) patch[f] = next
  }
  return Object.keys(patch).length === 0 ? null : patch
}
