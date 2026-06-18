package platform

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"
)

// dingTalkBot is a DingTalk Stream-mode chatbot. It connects out over a
// WebSocket (no public callback URL/IP/domain), so it works from a purely local
// install. The conversation id keys the jelly session, so each DingTalk chat
// keeps its own multi-turn context.
type dingTalkBot struct {
	name         string
	clientID     string
	clientSecret string
	reply        ReplyFunc
	logf         Logf

	mu     sync.Mutex
	cli    *client.StreamClient
	state  State
	detail string
}

// NewDingTalkBot builds a DingTalk bot. reply runs one engine turn for an
// incoming message; logf receives operational notices.
func NewDingTalkBot(name, clientID, clientSecret string, reply ReplyFunc, logf Logf) Bot {
	return &dingTalkBot{
		name: name, clientID: clientID, clientSecret: clientSecret,
		reply: reply, logf: logf, state: StateStopped,
	}
}

func (b *dingTalkBot) setState(s State, detail string) {
	b.mu.Lock()
	b.state, b.detail = s, detail
	b.mu.Unlock()
}

func (b *dingTalkBot) Status() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Status{Name: b.name, Type: "dingtalk", State: b.state, Detail: b.detail}
}

// Start connects the Stream client and registers the chatbot handler. It returns
// after the connection is established (or fails); the SDK then delivers messages
// on its own goroutine until Stop. AutoReconnect keeps the bot alive across
// transient drops.
func (b *dingTalkBot) Start(ctx context.Context) error {
	b.setState(StateConnecting, "")
	cli := client.NewStreamClient(
		client.WithAppCredential(client.NewAppCredentialConfig(b.clientID, b.clientSecret)),
		client.WithAutoReconnect(true),
	)
	cli.RegisterChatBotCallbackRouter(b.onMessage)
	if err := cli.Start(ctx); err != nil {
		b.setState(StateError, err.Error())
		return fmt.Errorf("dingtalk %q connect: %w", b.name, err)
	}
	b.mu.Lock()
	b.cli = cli
	b.state, b.detail = StateOnline, ""
	b.mu.Unlock()
	b.logf("钉钉机器人 %q 已连接（Stream 模式）", b.name)
	return nil
}

// Stop disconnects. The SDK read loop only exits on Close (ctx cancellation
// alone won't stop it), so the manager must call this for a clean shutdown.
func (b *dingTalkBot) Stop() {
	b.mu.Lock()
	cli := b.cli
	b.cli = nil
	b.state, b.detail = StateStopped, ""
	b.mu.Unlock()
	if cli != nil {
		cli.Close()
	}
}

// onMessage handles one inbound DingTalk chat: it runs an engine turn keyed by
// the conversation and replies with Markdown. Errors are surfaced back into the
// chat so the user isn't left waiting on silence.
func (b *dingTalkBot) onMessage(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	const ack = "" // empty body: the SDK only needs a non-error return
	text := strings.TrimSpace(data.Text.Content)
	if text == "" {
		return []byte(ack), nil
	}
	replier := chatbot.NewChatbotReplier()

	answer, err := b.reply(ctx, data.ConversationId, text)
	if err != nil {
		b.logf("钉钉机器人 %q 处理消息失败: %v", b.name, err)
		_ = replier.SimpleReplyText(ctx, data.SessionWebhook, []byte("处理消息出错："+err.Error()))
		return []byte(ack), nil
	}
	if strings.TrimSpace(answer) == "" {
		answer = "（无回复）"
	}
	if err := replier.SimpleReplyMarkdown(ctx, data.SessionWebhook, []byte("jelly-agent"), []byte(answer)); err != nil {
		b.logf("钉钉机器人 %q 回复失败: %v", b.name, err)
	}
	return []byte(ack), nil
}
