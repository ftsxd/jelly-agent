// Markdown rendering for agent replies.
//
// Every string that reaches this module is untrusted, and not only because a
// model wrote it: the agent has fetch_url and web_search, so a reply can quote
// a page someone else controls. Rendering that through v-html without
// sanitising is a stored-XSS hole in an operator console — one that would
// execute with whatever session the operator is logged in with. So marked
// never hands its output straight to the DOM; DOMPurify sits in between, and
// the allow-list below is deliberately narrow.
import { marked } from 'marked'
import DOMPurify from 'dompurify'

marked.setOptions({
  // GitHub-flavoured line breaks: a model writes list items and paragraphs
  // separated by single newlines, and strict CommonMark joins those into one
  // run-on line.
  breaks: true,
  gfm: true,
})

// The tags an agent reply legitimately needs. Everything else — script, style,
// iframe, form, object, svg, event handlers — is dropped rather than escaped,
// because an operator has no use for it and an attacker does.
const ALLOWED_TAGS = [
  'p', 'br', 'hr',
  'strong', 'em', 'del', 'code', 'pre',
  'ul', 'ol', 'li',
  'blockquote',
  'h1', 'h2', 'h3', 'h4', 'h5', 'h6',
  'a',
  'table', 'thead', 'tbody', 'tr', 'th', 'td',
]

// href is the only attribute worth keeping, plus the class marked puts on code
// blocks for language hints. No style, no id, no target — style would let
// injected content reposition itself over the page furniture.
const ALLOWED_ATTR = ['href', 'class']

// Links open in a new tab and disown the opener, so a quoted page cannot reach
// back into the console through window.opener.
DOMPurify.addHook('afterSanitizeAttributes', (node) => {
  if (node.tagName !== 'A') return
  node.setAttribute('target', '_blank')
  node.setAttribute('rel', 'noopener noreferrer nofollow')
})

/**
 * Render markdown to sanitised HTML.
 *
 * Returns escaped plain text if anything throws: a rendering failure must not
 * blank a reply the operator is waiting on, and unrendered markdown is still
 * readable.
 */
export function renderMarkdown(src) {
  if (!src) return ''
  try {
    const html = marked.parse(String(src))
    return DOMPurify.sanitize(html, {
      ALLOWED_TAGS,
      ALLOWED_ATTR,
      // Only schemes that cannot execute. javascript: and data: are the two
      // that turn a link into script execution.
      ALLOWED_URI_REGEXP: /^(?:https?:|mailto:|#|\/)/i,
    })
  } catch {
    return escapeHtml(String(src))
  }
}

function escapeHtml(s) {
  return s
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}
