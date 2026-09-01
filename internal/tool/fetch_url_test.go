package tool

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withUnguardedClient swaps in a client without the private-address guard for
// the duration of a test, since httptest servers listen on loopback.
func withUnguardedClient(t *testing.T) {
	t.Helper()
	prev := fetchClient
	fetchClient = newFetchClient(nil)
	t.Cleanup(func() { fetchClient = prev })
}

func TestFetchURLExtractsHTMLText(t *testing.T) {
	withUnguardedClient(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>  测试标题 </title>
			<style>body{color:red}</style></head>
			<body><script>var x = "不该出现";</script>
			<h1>大标题</h1>
			<p>第一段    正文。</p><p>第二段正文。</p>
			<ul><li>条目一</li><li>条目二</li></ul>
			</body></html>`))
	}))
	defer srv.Close()

	got, err := FetchURL(context.Background(), srv.URL, 0)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if got.Title != "测试标题" {
		t.Errorf("title = %q, want %q", got.Title, "测试标题")
	}
	for _, want := range []string{"大标题", "第一段 正文。", "第二段正文。", "条目一", "条目二"} {
		if !strings.Contains(got.Content, want) {
			t.Errorf("content missing %q:\n%s", want, got.Content)
		}
	}
	// script/style bodies must not leak into the extracted text.
	for _, bad := range []string{"不该出现", "color:red"} {
		if strings.Contains(got.Content, bad) {
			t.Errorf("content leaked %q:\n%s", bad, got.Content)
		}
	}
	if got.Truncated {
		t.Error("short page reported as truncated")
	}
}

func TestFetchURLTruncatesByRunes(t *testing.T) {
	withUnguardedClient(t)
	long := strings.Repeat("中", 500)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte("<html><body><p>" + long + "</p></body></html>"))
	}))
	defer srv.Close()

	got, err := FetchURL(context.Background(), srv.URL, 100)
	if err != nil {
		t.Fatalf("FetchURL: %v", err)
	}
	if !got.Truncated {
		t.Fatal("want Truncated = true")
	}
	// Cutting on runes, not bytes, keeps multi-byte characters intact.
	if n := strings.Count(got.Content, "中"); n != 100 {
		t.Errorf("kept %d 中 runes, want 100", n)
	}
	if strings.ContainsRune(got.Content, '\uFFFD') {
		t.Error("truncation split a multi-byte rune")
	}
}

func TestFetchURLPlainTextAndRejectedTypes(t *testing.T) {
	withUnguardedClient(t)
	var ctype, body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", ctype)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ctype, body = "application/json", `{"ok":true}`
	got, err := FetchURL(context.Background(), srv.URL, 0)
	if err != nil {
		t.Fatalf("json fetch: %v", err)
	}
	if got.Content != `{"ok":true}` {
		t.Errorf("json content = %q", got.Content)
	}

	ctype, body = "image/png", "\x89PNG\r\n"
	if _, err := FetchURL(context.Background(), srv.URL, 0); err == nil {
		t.Error("want error for image/png, got nil")
	}
}

func TestFetchURLRejectsNonPublicTargets(t *testing.T) {
	// The real guarded client: an httptest server on loopback must be refused
	// even though it is reachable.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>secret</body></html>"))
	}))
	defer srv.Close()

	if _, err := FetchURL(context.Background(), srv.URL, 0); err == nil {
		t.Error("loopback target was allowed, want refusal")
	}
}

func TestFetchURLRejectsBadInput(t *testing.T) {
	for _, tc := range []struct{ name, url string }{
		{"empty", "  "},
		{"scheme", "file:///etc/passwd"},
		{"no host", "http://"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := FetchURL(context.Background(), tc.url, 0); err == nil {
				t.Errorf("FetchURL(%q) = nil error, want refusal", tc.url)
			}
		})
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"2001:4860:4860::8888", true},
		{"127.0.0.1", false},
		{"10.1.2.3", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"169.254.169.254", false}, // cloud metadata endpoint
		{"100.64.0.1", false},      // CGNAT
		{"0.0.0.0", false},
		{"::1", false},
		{"fd00::1", false},
	} {
		if got := isPublicIP(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("isPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestFetchKind(t *testing.T) {
	for _, tc := range []struct {
		ctype string
		kind  fetchContentKind
		ok    bool
	}{
		{"text/html; charset=utf-8", fetchHTML, true},
		{"application/xhtml+xml", fetchHTML, true},
		{"", fetchHTML, true},
		{"text/plain", fetchPlain, true},
		{"application/json", fetchPlain, true},
		{"application/ld+json", fetchPlain, true},
		{"application/pdf", fetchPlain, false},
		{"image/jpeg", fetchPlain, false},
	} {
		kind, ok := fetchKind(tc.ctype)
		if ok != tc.ok || (ok && kind != tc.kind) {
			t.Errorf("fetchKind(%q) = (%v, %v), want (%v, %v)", tc.ctype, kind, ok, tc.kind, tc.ok)
		}
	}
}
