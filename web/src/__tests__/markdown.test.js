// @vitest-environment jsdom
import { describe, expect, it } from 'vitest'
import { renderMarkdown } from '../markdown'

// The agent has fetch_url and web_search, so a reply can quote a page someone
// else controls. Rendering that through v-html is a stored-XSS hole in an
// operator console, executing with whatever session the operator is logged in
// with — so these are the cases that must stay closed if anyone later widens
// the allow-list or drops the sanitiser.
describe('sanitisation', () => {
  const payloads = {
    'script tag': 'hi <script>alert(1)</script> there',
    'img onerror': '<img src=x onerror=alert(1)>',
    'svg onload': '<svg onload=alert(1)>',
    'iframe': '<iframe src="//evil.test"></iframe>',
    'object': '<object data="//evil.test"></object>',
    'javascript: link': '[click](javascript:alert(1))',
    'data: link': '[click](data:text/html,<script>alert(1)</script>)',
    'html inside a list': '- item\n- <script>alert(1)</script>',
    'html inside a code fence': '```\n<script>alert(1)</script>\n```',
    'event handler on an allowed tag': '<p onmouseover="alert(1)">hover</p>',
  }

  for (const [name, src] of Object.entries(payloads)) {
    it(`neutralises ${name}`, () => {
      const out = renderMarkdown(src)
      expect(out).not.toMatch(/<script|<iframe|<object|<svg/i)
      expect(out).not.toMatch(/\bon[a-z]+\s*=/i)
      expect(out).not.toMatch(/javascript:|data:text\/html/i)
    })
  }

  // style would let injected content position itself over the page furniture —
  // an invisible overlay capturing clicks meant for a real control.
  it('drops style attributes', () => {
    const out = renderMarkdown('<div style="position:fixed;inset:0">covered</div>')
    expect(out).not.toMatch(/style\s*=/i)
  })

  // A quoted page must not reach back into the console through window.opener.
  it('disowns the opener on links', () => {
    const out = renderMarkdown('[docs](https://example.com/a)')
    expect(out).toContain('href="https://example.com/a"')
    expect(out).toContain('rel="noopener noreferrer nofollow"')
    expect(out).toContain('target="_blank"')
  })
})

// The formatting an agent actually produces has to survive the sanitiser —
// a filter that also ate the content would just be a worse plain-text view.
describe('rendering', () => {
  it('renders the markdown a reply actually uses', () => {
    const out = renderMarkdown(
      '**bold** and `code`\n- one\n- two\n\n```go\nfmt.Println("x")\n```',
    )
    expect(out).toContain('<strong>bold</strong>')
    expect(out).toContain('<code>code</code>')
    expect(out).toMatch(/<ul>[\s\S]*<li>one<\/li>[\s\S]*<li>two<\/li>/)
    expect(out).toContain('<pre>')
    // Code content is escaped, not executed or dropped.
    expect(out).toContain('fmt.Println')
  })

  // Models separate list items and paragraphs with single newlines; strict
  // CommonMark joins those into one run-on line.
  it('honours single newlines as breaks', () => {
    expect(renderMarkdown('line one\nline two')).toContain('<br>')
  })

  it('escapes rather than blanks when given nothing useful', () => {
    expect(renderMarkdown('')).toBe('')
    expect(renderMarkdown(null)).toBe('')
    expect(renderMarkdown('a < b & c')).toContain('&lt;')
  })
})
