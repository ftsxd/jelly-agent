package platform

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	qrcode "github.com/skip2/go-qrcode"
)

// weChatPadProBot connects personal WeChat through a WeChatPadPro gateway (an
// iPad-protocol bridge the user runs locally; gewechat's successor). jelly-agent
// talks to its HTTP API + a message-push WebSocket, so no public URL is needed.
// First-batch scope is text only. Login is by QR scan, surfaced via Status.QR.
//
// ⚠️ Personal-WeChat automation uses an unofficial protocol and violates WeChat's
// ToS — there is a real account-ban risk. Use a secondary account.
type weChatPadProBot struct {
	name     string
	httpBase string // wechatpad_url, e.g. http://127.0.0.1:9090
	wsBase   string // wechatpad_ws
	adminKey string
	reply    ReplyFunc
	logf     Logf
	http     *http.Client

	mu     sync.Mutex
	token  string
	wxid   string // self wxid, discovered via GetProfile after login
	state  State
	detail string
	qr     string
	cancel context.CancelFunc
}

// NewWeChatPadProBot builds a WeChatPadPro-backed personal-WeChat bot from its
// settings map (wechatpad_url, wechatpad_ws, admin_key, token, wxid).
func NewWeChatPadProBot(name string, settings map[string]string, reply ReplyFunc, logf Logf) Bot {
	return &weChatPadProBot{
		name:     name,
		httpBase: settings["wechatpad_url"],
		wsBase:   settings["wechatpad_ws"],
		adminKey: settings["admin_key"],
		token:    settings["token"],
		wxid:     settings["wxid"],
		reply:    reply,
		logf:     logf,
		http:     &http.Client{Timeout: 30 * time.Second},
		state:    StateStopped,
	}
}

func (b *weChatPadProBot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Name: b.name, Type: "wechatpadpro", State: b.state, Detail: b.detail, QR: b.qr}
}

func (b *weChatPadProBot) setStatus(s State, detail, qr string) {
	b.mu.Lock()
	b.state, b.detail, b.qr = s, detail, qr
	b.mu.Unlock()
}

func (b *weChatPadProBot) selfWxid() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.wxid
}

// Start kicks off the connect/login lifecycle in the background and returns
// immediately; progress (awaiting-scan, online, error) shows via Status. The
// long QR-scan wait is why this doesn't block like the DingTalk bot.
func (b *weChatPadProBot) Start(_ context.Context) error {
	// Use a fresh background context so Stop (not the parent) controls teardown.
	ctx, cancel := context.WithCancel(context.Background())
	b.mu.Lock()
	b.cancel = cancel
	b.state = StateConnecting
	b.mu.Unlock()
	go b.run(ctx)
	return nil
}

func (b *weChatPadProBot) Stop() {
	b.mu.Lock()
	cancel := b.cancel
	b.cancel = nil
	b.state, b.detail, b.qr = StateStopped, "", ""
	b.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// run drives: obtain token → ensure logged in (QR) → open the message WebSocket,
// reconnecting on drop until Stop.
func (b *weChatPadProBot) run(ctx context.Context) {
	if b.token == "" {
		if b.adminKey == "" {
			b.fail("缺少 admin_key 或 token")
			return
		}
		if err := b.genToken(ctx); err != nil {
			b.fail("获取 token 失败: " + err.Error())
			return
		}
	}
	for ctx.Err() == nil {
		if err := b.ensureLoggedIn(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			b.fail("登录失败: " + err.Error())
			if !sleepCtx(ctx, 5*time.Second) {
				return
			}
			continue
		}
		err := b.runWS(ctx)
		if ctx.Err() != nil {
			return
		}
		b.logf("微信机器人 %q 连接断开，5s 后重连: %v", b.name, err)
		b.setStatus(StateConnecting, "重连中", "")
		if !sleepCtx(ctx, 5*time.Second) {
			return
		}
	}
}

// ensureLoggedIn returns once the gateway reports a logged-in account, fetching
// and surfacing a login QR (refreshed periodically) and polling until scanned.
func (b *weChatPadProBot) ensureLoggedIn(ctx context.Context) error {
	if wxid, ok := b.fetchProfile(ctx); ok {
		b.setSelfWxid(wxid)
		return nil
	}
	if err := b.refreshQR(ctx); err != nil {
		return err
	}
	b.logf("微信机器人 %q 等待扫码登录（二维码已在「消息绑定」页显示）", b.name)

	const qrTTL = 20 // re-fetch the QR roughly every 20 polls (~60s)
	for polls := 0; ctx.Err() == nil; polls++ {
		if !sleepCtx(ctx, 3*time.Second) {
			return ctx.Err()
		}
		if wxid, ok := b.fetchProfile(ctx); ok {
			b.setSelfWxid(wxid)
			return nil
		}
		if polls > 0 && polls%qrTTL == 0 {
			_ = b.refreshQR(ctx) // best-effort QR refresh on expiry
		}
	}
	return ctx.Err()
}

func (b *weChatPadProBot) refreshQR(ctx context.Context) error {
	content, err := b.fetchLoginQR(ctx)
	if err != nil {
		return err
	}
	qr := ""
	if png, err := qrcode.Encode(content, qrcode.Medium, 256); err == nil {
		qr = "data:image/png;base64," + base64.StdEncoding.EncodeToString(png)
	}
	b.setStatus(StateConnecting, "等待扫码登录", qr)
	return nil
}

// runWS opens the message-push WebSocket and dispatches inbound messages until
// the connection drops or ctx is cancelled.
func (b *weChatPadProBot) runWS(ctx context.Context) error {
	uri := fmt.Sprintf("%s/GetSyncMsg?key=%s", strings.TrimRight(b.wsBase, "/"), url.QueryEscape(b.token))
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, uri, nil)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() { <-ctx.Done(); conn.Close() }()

	b.setStatus(StateOnline, "", "")
	b.logf("微信机器人 %q 已上线（WeChatPadPro）", b.name)

	for {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		go b.handleMessage(ctx, raw) // don't block the read loop on the LLM turn
	}
}

// handleMessage parses one push frame and, if it's an actionable text message,
// runs an engine turn and sends the reply back to the same chat.
func (b *weChatPadProBot) handleMessage(ctx context.Context, raw []byte) {
	key, text, ok := parseInbound(raw, b.selfWxid())
	if !ok {
		return
	}
	answer, err := b.reply(ctx, key, text)
	if err != nil {
		b.logf("微信机器人 %q 处理消息失败: %v", b.name, err)
		answer = "处理消息出错：" + err.Error()
	}
	if strings.TrimSpace(answer) == "" {
		answer = "（无回复）"
	}
	if err := b.sendText(ctx, key, answer); err != nil {
		b.logf("微信机器人 %q 回复失败: %v", b.name, err)
	}
}

func (b *weChatPadProBot) setSelfWxid(wxid string) {
	b.mu.Lock()
	b.wxid = wxid
	b.mu.Unlock()
}

func (b *weChatPadProBot) fail(msg string) {
	b.setStatus(StateError, msg, "")
	b.logf("微信机器人 %q: %s", b.name, msg)
}

// --- WeChatPadPro HTTP API ---------------------------------------------------

// do issues a request to {httpBase}{path}?key={key} with an optional JSON body
// and decodes the JSON response into out. The gateway authenticates via the
// `key` query parameter (admin_key for /admin/*, the session token otherwise).
func (b *weChatPadProBot) do(ctx context.Context, method, path, key string, body, out any) error {
	u := fmt.Sprintf("%s%s?key=%s", strings.TrimRight(b.httpBase, "/"), path, url.QueryEscape(key))
	var rdr io.Reader
	if body != nil {
		bs, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(bs)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// genToken exchanges the admin key for a long-lived session token.
func (b *weChatPadProBot) genToken(ctx context.Context) error {
	var r struct {
		Code int               `json:"Code"`
		Data []json.RawMessage `json:"Data"`
		Text string            `json:"Text"`
	}
	if err := b.do(ctx, http.MethodPost, "/admin/GenAuthKey1", b.adminKey, map[string]any{"Count": 1, "Days": 365}, &r); err != nil {
		return err
	}
	if len(r.Data) == 0 {
		return fmt.Errorf("空响应: %s", r.Text)
	}
	var tok string
	if err := json.Unmarshal(r.Data[0], &tok); err != nil || tok == "" {
		return fmt.Errorf("无法解析 token")
	}
	b.mu.Lock()
	b.token = tok
	b.mu.Unlock()
	return nil
}

// fetchProfile returns the logged-in account's wxid; ok is false when the
// gateway has no logged-in session yet (the signal to show a login QR).
func (b *weChatPadProBot) fetchProfile(ctx context.Context) (string, bool) {
	var r struct {
		Code int `json:"Code"`
		Data struct {
			UserInfo struct {
				UserName struct {
					Str string `json:"str"`
				} `json:"userName"`
			} `json:"userInfo"`
		} `json:"Data"`
	}
	if err := b.do(ctx, http.MethodGet, "/user/GetProfile", b.token, nil, &r); err != nil {
		return "", false
	}
	wxid := r.Data.UserInfo.UserName.Str
	return wxid, wxid != ""
}

// fetchLoginQR returns the QR content string to encode for scanning.
func (b *weChatPadProBot) fetchLoginQR(ctx context.Context) (string, error) {
	var r struct {
		Code int `json:"Code"`
		Data struct {
			QrCodeUrl string `json:"QrCodeUrl"`
			Key       string `json:"Key"`
		} `json:"Data"`
		Text string `json:"Text"`
	}
	if err := b.do(ctx, http.MethodPost, "/login/GetLoginQrCodeNew", b.token, map[string]any{"Check": false, "Proxy": ""}, &r); err != nil {
		return "", err
	}
	if r.Data.QrCodeUrl == "" {
		return "", fmt.Errorf("二维码为空: %s", r.Text)
	}
	return r.Data.QrCodeUrl, nil
}

// sendText posts a plain-text reply to toWxid (a peer wxid or a @chatroom id).
func (b *weChatPadProBot) sendText(ctx context.Context, toWxid, content string) error {
	body := map[string]any{
		"MsgItem": []map[string]any{
			{"AtWxIDList": []string{}, "ImageContent": "", "MsgType": 0, "TextContent": content, "ToUserName": toWxid},
		},
	}
	return b.do(ctx, http.MethodPost, "/message/SendTextMessage", b.token, body, nil)
}

// --- inbound parsing (pure, unit-tested) -------------------------------------

// wsMessage is the subset of a WeChatPadPro push frame this batch handles.
type wsMessage struct {
	FromUserName nameStr `json:"from_user_name"`
	ToUserName   nameStr `json:"to_user_name"`
	Content      nameStr `json:"content"`
	MsgType      int     `json:"msg_type"`
	PushContent  string  `json:"push_content"`
}

type nameStr struct {
	Str string `json:"str"`
}

var (
	groupPrefixRe = regexp.MustCompile(`^[a-zA-Z0-9_\-]{5,20}:`)
	atMentionRe   = regexp.MustCompile(`@\S{1,20}`)
)

// parseInbound decides whether a push frame is an actionable text message and,
// if so, returns the session key (peer wxid, or group id for @chatroom) and the
// cleaned text. It filters non-text, system senders, the bot's own messages,
// and group messages that don't @ the bot. Pure: no I/O, so it is unit-tested.
func parseInbound(raw []byte, selfWxid string) (sessionKey, text string, ok bool) {
	var m wsMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", false
	}
	if m.MsgType != 1 { // text only this batch
		return "", "", false
	}
	from := m.FromUserName.Str
	if from == "" || from == selfWxid || isSystemSender(from) {
		return "", "", false
	}
	body := m.Content.Str
	if strings.HasSuffix(from, "@chatroom") {
		if !strings.Contains(m.PushContent, "@了你") { // only answer when @'d in groups
			return "", "", false
		}
		body = stripGroupPrefix(body)
		body = atMentionRe.ReplaceAllString(body, "")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return "", "", false
	}
	return from, body, true
}

// isSystemSender reports official-account / system senders to ignore.
func isSystemSender(from string) bool {
	return strings.HasPrefix(from, "gh_") || from == "weixin" || from == "newsapp"
}

// stripGroupPrefix drops the leading "sender_wxid:\n" that group messages carry.
func stripGroupPrefix(content string) string {
	if i := strings.IndexByte(content, '\n'); i >= 0 && groupPrefixRe.MatchString(content[:i]) {
		return content[i+1:]
	}
	return content
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
