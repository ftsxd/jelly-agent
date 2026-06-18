package server

import (
	"net/http"
	"strings"
	"testing"
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
