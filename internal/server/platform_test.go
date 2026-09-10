package server

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jelly-agent/jelly-agent/internal/platform"
)

// TestPlatformCreateUpdateDelete drives the /api/platforms CRUD end to end:
// create persists + masks the secret, update keeps the secret when blank, and
// delete removes the bot. Mirrors TestProviderCreateUpdateDelete.
func TestPlatformCreateUpdateDelete(t *testing.T) {
	s := newEmptyServer(t)

	// Create a DingTalk bot.
	w := do(t, s, "POST", "/api/platforms",
		`{"name":"dt","type":"dingtalk","client_id":"ding-app-key","client_secret":"super-secret-value","provider":"","enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	// List never leaks the secret, only that one is set; client_id is shown.
	w = do(t, s, "GET", "/api/platforms", "")
	body := w.Body.String()
	if strings.Contains(body, "super-secret-value") {
		t.Fatalf("client secret leaked: %s", body)
	}
	if !strings.Contains(body, "ding-app-key") || !strings.Contains(body, `"has_secret":true`) {
		t.Fatalf("list missing client_id / has_secret: %s", body)
	}

	// Update with blank secret keeps the stored one; provider changes.
	w = do(t, s, "POST", "/api/platforms",
		`{"name":"dt","type":"dingtalk","client_id":"ding-app-key","client_secret":"","provider":"deepseek","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", w.Code, w.Body.String())
	}
	got := s.engine().Config().Platforms
	if len(got) != 1 || got[0].ClientSecret != "super-secret-value" || got[0].Provider != "deepseek" || got[0].Enabled {
		t.Fatalf("update lost secret or fields: %+v", got)
	}

	// Delete clears it.
	w = do(t, s, "DELETE", "/api/platforms/dt", "")
	if w.Code != http.StatusOK {
		t.Fatalf("delete status = %d: %s", w.Code, w.Body.String())
	}
	if n := len(s.engine().Config().Platforms); n != 0 {
		t.Fatalf("platforms after delete = %d, want 0", n)
	}
}

// TestWeChatPadProCreateUpdate covers the wechatpadpro branch: settings persist,
// admin_key is masked in the list (only its key name surfaces), and a blank
// admin_key on update keeps the stored one.
func TestWeChatPadProCreateUpdate(t *testing.T) {
	s := newEmptyServer(t)

	w := do(t, s, "POST", "/api/platforms",
		`{"name":"wx","type":"wechatpadpro","enabled":true,"settings":{"wechatpad_url":"http://127.0.0.1:9090","wechatpad_ws":"ws://127.0.0.1:9090/ws","admin_key":"super-admin-secret"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}

	w = do(t, s, "GET", "/api/platforms", "")
	body := w.Body.String()
	if strings.Contains(body, "super-admin-secret") {
		t.Fatalf("admin_key leaked: %s", body)
	}
	if !strings.Contains(body, "127.0.0.1:9090") || !strings.Contains(body, `"secret_keys":["admin_key"]`) {
		t.Fatalf("list missing visible url / secret_keys: %s", body)
	}

	// Update url with blank admin_key keeps the stored secret.
	w = do(t, s, "POST", "/api/platforms",
		`{"name":"wx","type":"wechatpadpro","enabled":false,"settings":{"wechatpad_url":"http://127.0.0.1:7070","wechatpad_ws":"ws://127.0.0.1:7070/ws"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("update status = %d: %s", w.Code, w.Body.String())
	}
	got := s.engine().Config().Platforms
	if len(got) != 1 || got[0].Settings["admin_key"] != "super-admin-secret" || got[0].Settings["wechatpad_url"] != "http://127.0.0.1:7070" {
		t.Fatalf("update lost secret or url: %+v", got)
	}
}

func TestWeChatPadProRequiresSettings(t *testing.T) {
	s := newEmptyServer(t)
	// Missing wechatpad_ws and key/token.
	w := do(t, s, "POST", "/api/platforms",
		`{"name":"wx","type":"wechatpadpro","enabled":true,"settings":{"wechatpad_url":"http://x"}}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing ws/key)", w.Code)
	}
}

// TestDingTalkCardTemplatePersists checks the optional card_template_id (which
// enables streaming AI-card replies) round-trips through settings and is shown.
func TestDingTalkCardTemplatePersists(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/platforms",
		`{"name":"dt","type":"dingtalk","client_id":"k","client_secret":"sec","enabled":true,"settings":{"card_template_id":"tpl-123.schema"}}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	if got := s.engine().Config().Platforms; len(got) != 1 || got[0].Settings["card_template_id"] != "tpl-123.schema" {
		t.Fatalf("card_template_id not persisted: %+v", got)
	}
	w = do(t, s, "GET", "/api/platforms", "")
	if !strings.Contains(w.Body.String(), "tpl-123.schema") {
		t.Fatalf("card_template_id not surfaced in list: %s", w.Body.String())
	}
}

// TestPlatformMCPSelection checks a bot's selected MCP servers round-trip
// through config and surface in the list (selective per-bot MCP loading).
func TestPlatformMCPSelection(t *testing.T) {
	s := newEmptyServer(t)
	w := do(t, s, "POST", "/api/platforms",
		`{"name":"dt","type":"dingtalk","client_id":"k","client_secret":"sec","enabled":true,"mcp":["filesystem","github"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("create status = %d: %s", w.Code, w.Body.String())
	}
	got := s.engine().Config().Platforms
	if len(got) != 1 || len(got[0].MCP) != 2 || got[0].MCP[0] != "filesystem" || got[0].MCP[1] != "github" {
		t.Fatalf("mcp selection not persisted: %+v", got)
	}
	w = do(t, s, "GET", "/api/platforms", "")
	if !strings.Contains(w.Body.String(), `"mcp":["filesystem","github"]`) {
		t.Fatalf("mcp not surfaced in list: %s", w.Body.String())
	}
}

func TestPlatformCreateRequiresCredentials(t *testing.T) {
	s := newEmptyServer(t)
	// Missing client_secret on create is rejected.
	w := do(t, s, "POST", "/api/platforms", `{"name":"dt","type":"dingtalk","client_id":"x","client_secret":"","provider":"","enabled":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (missing secret)", w.Code)
	}
	// Unsupported type is rejected.
	w = do(t, s, "POST", "/api/platforms", `{"name":"w","type":"wechat","client_id":"a","client_secret":"b","provider":"","enabled":true}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (unsupported type)", w.Code)
	}
}

// fakeBot behaves the way both real bots do: Stop can only close what Start
// has already installed. dingTalkBot has nothing to Close until the Stream
// client is built; weChatPadProBot has no cancel until run is launched. A
// Stop that arrives first is a no-op on either.
type fakeBot struct {
	mu        sync.Mutex
	connected bool
	closed    bool
	starts    int
	release   chan struct{} // Start blocks here until the test lets it finish
	entered   chan struct{}
}

func (b *fakeBot) Start(context.Context) error {
	b.mu.Lock()
	b.starts++
	b.mu.Unlock()
	if b.entered != nil {
		close(b.entered)
	}
	if b.release != nil {
		<-b.release
	}
	b.mu.Lock()
	b.connected = true
	b.mu.Unlock()
	return nil
}

func (b *fakeBot) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.connected { // nothing to close before that, exactly like the real ones
		b.closed = true
		b.connected = false
	}
}

func (b *fakeBot) Status() platform.Status { return platform.Status{} }

func (b *fakeBot) state() (connected, closed bool, starts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connected, b.closed, b.starts
}

// A bot replaced before it finished connecting must not be left connected.
//
// Start is asynchronous, so a restart's Stop can land while the connection is
// still being made — and it finds nothing to close, because neither bot has
// anything to close until Start has installed it. The connection then comes
// up with nobody holding it: an orphan answering the same group chat next to
// its replacement, until the process exits.
func TestABotStoppedWhileConnectingDoesNotStayConnected(t *testing.T) {
	b := &fakeBot{release: make(chan struct{}), entered: make(chan struct{})}
	m := &managedBot{Bot: b}
	go m.run(context.Background(), "dt")

	<-b.entered // Start is in flight
	m.Stop()    // the restart, arriving mid-connect
	close(b.release)

	deadline := time.Now().Add(2 * time.Second)
	for {
		connected, closed, _ := b.state()
		if closed && !connected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("换掉的机器人连上之后没人关它 —— 它会和新的一起回同一个群（connected=%v closed=%v）",
				connected, closed)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// …and one stopped before it ever started must not connect at all.
func TestABotStoppedBeforeItStartsNeverConnects(t *testing.T) {
	b := &fakeBot{}
	m := &managedBot{Bot: b}
	m.Stop()
	m.run(context.Background(), "dt")

	if connected, _, starts := b.state(); connected || starts != 0 {
		t.Errorf("已经停掉的机器人还是连上去了: connected=%v starts=%d", connected, starts)
	}
}
