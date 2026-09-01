package tool

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
)

// FetchURLArgs are the parameters the model fills when calling fetch_url.
type FetchURLArgs struct {
	URL      string `json:"url" jsonschema:"要抓取的网页地址，必须是 http:// 或 https://"`
	MaxChars int    `json:"max_chars,omitempty" jsonschema:"正文最大字符数，默认 8000，上限 40000"`
}

// FetchURLResult is the structured tool output handed back to the model.
type FetchURLResult struct {
	URL         string `json:"url"`
	FinalURL    string `json:"final_url,omitempty"` // set when redirected
	Title       string `json:"title,omitempty"`
	ContentType string `json:"content_type,omitempty"`
	Content     string `json:"content"`
	Truncated   bool   `json:"truncated,omitempty"`
}

const (
	defaultFetchChars = 8000
	maxFetchChars     = 40000
	// maxFetchBytes caps what we read off the wire regardless of max_chars, so a
	// huge or endless response can't exhaust memory.
	maxFetchBytes = 4 << 20 // 4 MiB
	maxRedirects  = 5
)

// fetchClient dials through guardPrivateAddr so every connection — including
// each redirect hop — is checked against the private-address blocklist after
// DNS resolution. Checking the resolved IP rather than the hostname is what
// makes DNS rebinding ineffective. It is a var so tests can swap in a client
// without the guard (httptest listens on loopback, which the guard refuses).
var fetchClient = newFetchClient(guardPrivateAddr)

// dialControl matches net.Dialer.Control; a nil control disables the guard.
type dialControl func(network, address string, c syscall.RawConn) error

func newFetchClient(control dialControl) *http.Client {
	return &http.Client{
		Timeout: 25 * time.Second,
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
				Control:   control,
			}).DialContext,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("重定向次数超过 %d 次", maxRedirects)
			}
			if s := req.URL.Scheme; s != "http" && s != "https" {
				return fmt.Errorf("拒绝重定向到非 http(s) 地址: %s", req.URL.Scheme)
			}
			return nil
		},
	}
}

// NewFetchURLTool builds the fetch_url tool. It closes the loop left open by
// web_search, which returns only titles/snippets/links: the model can now open
// a result and read the page body.
func NewFetchURLTool() (adktool.Tool, error) {
	return functiontool.New(
		functiontool.Config{
			Name:        "fetch_url",
			Description: "抓取网页并返回正文纯文本（自动去除脚本/样式/标签）。用于读取 web_search 返回的链接，或用户直接给出的网址。仅支持 http/https 的公网地址。",
		},
		func(tc adktool.Context, args FetchURLArgs) (FetchURLResult, error) {
			// tc is a context.Context, so the fetch is cancelled together with
			// the upstream model call / SSE connection.
			return FetchURL(tc, args.URL, args.MaxChars)
		},
	)
}

// FetchURL retrieves a page and reduces it to plain text. It is exported so the
// web server's tool-test endpoint can exercise the tool without going through
// the model, mirroring Search. A non-positive max uses the default; an oversized
// max is clamped.
func FetchURL(ctx context.Context, raw string, max int) (FetchURLResult, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return FetchURLResult{}, fmt.Errorf("url 不能为空")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return FetchURLResult{}, fmt.Errorf("url 解析失败: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return FetchURLResult{}, fmt.Errorf("只支持 http/https，收到 %q", u.Scheme)
	}
	if u.Host == "" {
		return FetchURLResult{}, fmt.Errorf("url 缺少主机名: %q", raw)
	}
	switch {
	case max <= 0:
		max = defaultFetchChars
	case max > maxFetchChars:
		max = maxFetchChars
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return FetchURLResult{}, err
	}
	// Some sites serve a bare error page to unknown clients; a conventional UA
	// and Accept keep us on the normal HTML path.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; jelly-agent/1.0; +https://github.com/ftsxd/jelly-agent)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,text/plain;q=0.9,*/*;q=0.5")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := fetchClient.Do(req)
	if err != nil {
		return FetchURLResult{}, fmt.Errorf("抓取失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return FetchURLResult{}, fmt.Errorf("抓取失败: HTTP %d", resp.StatusCode)
	}

	ctype := resp.Header.Get("Content-Type")
	kind, ok := fetchKind(ctype)
	if !ok {
		return FetchURLResult{}, fmt.Errorf("不支持的内容类型 %q（仅支持 HTML / 纯文本 / JSON / XML）", ctype)
	}

	body := io.LimitReader(resp.Body, maxFetchBytes)
	out := FetchURLResult{URL: raw, ContentType: ctype}
	if final := resp.Request.URL.String(); final != raw {
		out.FinalURL = final
	}

	switch kind {
	case fetchHTML:
		// charset.NewReader sniffs the meta/BOM/header charset and transcodes to
		// UTF-8, so GB18030/Big5 pages come back readable rather than as mojibake.
		r, err := charset.NewReader(body, ctype)
		if err != nil {
			r = body // fall back to raw bytes rather than failing the whole fetch
		}
		doc, err := html.Parse(r)
		if err != nil {
			return FetchURLResult{}, fmt.Errorf("解析 HTML: %w", err)
		}
		out.Title, out.Content = extractText(doc)
	default:
		data, err := io.ReadAll(body)
		if err != nil {
			return FetchURLResult{}, fmt.Errorf("读取响应体: %w", err)
		}
		out.Content = strings.TrimSpace(string(data))
	}

	out.Content, out.Truncated = truncateRunes(out.Content, max)
	if out.Content == "" {
		out.Content = "（该页面没有可提取的文本内容，可能是脚本渲染的动态页面。）"
	}
	return out, nil
}

type fetchContentKind int

const (
	fetchHTML fetchContentKind = iota
	fetchPlain
)

// fetchKind maps a Content-Type to how the body should be decoded, rejecting
// binary payloads (images, PDFs, archives) that would be noise to the model.
func fetchKind(ctype string) (fetchContentKind, bool) {
	mime := strings.ToLower(strings.TrimSpace(ctype))
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = strings.TrimSpace(mime[:i])
	}
	switch {
	case mime == "":
		return fetchHTML, true // unlabelled: assume HTML, the parser tolerates text
	case mime == "text/html", mime == "application/xhtml+xml":
		return fetchHTML, true
	case strings.HasPrefix(mime, "text/"):
		return fetchPlain, true
	case mime == "application/json", mime == "application/xml",
		strings.HasSuffix(mime, "+json"), strings.HasSuffix(mime, "+xml"):
		return fetchPlain, true
	}
	return fetchPlain, false
}

// skipElements are subtrees whose text is never page content.
var skipElements = map[string]bool{
	"script": true, "style": true, "noscript": true, "template": true,
	"svg": true, "canvas": true, "iframe": true, "head": true,
}

// blockElements force a line break so paragraphs and list items don't run
// together into one wall of text.
var blockElements = map[string]bool{
	"p": true, "div": true, "br": true, "li": true, "tr": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"section": true, "article": true, "header": true, "footer": true,
	"blockquote": true, "pre": true, "ul": true, "ol": true, "table": true,
}

// extractText walks the parsed document and returns (title, body text). Text
// nodes are whitespace-collapsed and block elements become newlines.
func extractText(doc *html.Node) (title string, text string) {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch n.Type {
		case html.ElementNode:
			if n.Data == "title" && title == "" {
				title = strings.TrimSpace(nodeText(n))
				return
			}
			if skipElements[n.Data] {
				// <head> is skipped for its content but still searched for <title>.
				if n.Data == "head" {
					for c := n.FirstChild; c != nil; c = c.NextSibling {
						walk(c)
					}
				}
				return
			}
			if blockElements[n.Data] {
				b.WriteByte('\n')
			}
			for c := n.FirstChild; c != nil; c = c.NextSibling {
				walk(c)
			}
			if blockElements[n.Data] {
				b.WriteByte('\n')
			}
			return
		case html.TextNode:
			if s := collapseSpaces(n.Data); s != "" {
				b.WriteString(s)
				b.WriteByte(' ')
			}
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)
	return title, tidyLines(b.String())
}

// nodeText concatenates the raw text under a node (used for <title>).
func nodeText(n *html.Node) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	}
	return collapseSpaces(b.String())
}

// collapseSpaces squeezes every run of whitespace into a single space.
func collapseSpaces(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// tidyLines trims each line and collapses runs of blank lines into one, turning
// the walker's generous newlines into paragraph breaks.
func tidyLines(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := false
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		blank = false
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncateRunes cuts s to at most max runes (not bytes, so multi-byte CJK text
// isn't split mid-character) and reports whether it was cut.
func truncateRunes(s string, max int) (string, bool) {
	r := []rune(s)
	if len(r) <= max {
		return s, false
	}
	return strings.TrimSpace(string(r[:max])) + "\n\n…（内容超长已截断，如需后续部分可提高 max_chars）", true
}

// guardPrivateAddr is the dialer hook that blocks SSRF: it runs after DNS
// resolution with the concrete IP the connection would use, so a public
// hostname resolving to 127.0.0.1 or 169.254.169.254 is rejected here.
func guardPrivateAddr(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("解析目标地址 %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("无法解析目标 IP: %q", host)
	}
	if !isPublicIP(ip) {
		return fmt.Errorf("拒绝访问内网/环回地址 %s（fetch_url 只允许公网地址）", ip)
	}
	return nil
}

// cgnatNet is 100.64.0.0/10 (carrier-grade NAT), which net.IP has no helper for.
var cgnatNet = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// isPublicIP reports whether ip is routable on the public internet. Everything
// else — loopback, RFC1918, link-local (incl. the cloud metadata endpoint),
// unique-local IPv6, multicast, CGNAT — is refused.
func isPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	if v4 := ip.To4(); v4 != nil && cgnatNet.Contains(v4) {
		return false
	}
	return true
}
