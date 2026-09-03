package platform

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/open-dingtalk/dingtalk-stream-sdk-go/chatbot"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/client"

	"github.com/jelly-agent/jelly-agent/internal/logging"
)

// dingTalkBot is a DingTalk Stream-mode chatbot. It connects out over a
// WebSocket (no public callback URL/IP/domain), so it works from a purely local
// install. The conversation id keys the jelly session, so each DingTalk chat
// keeps its own multi-turn context.
type dingTalkBot struct {
	name           string
	clientID       string
	clientSecret   string
	cardTemplateID string // when set, replies stream into a DingTalk AI card
	stream         StreamReplyFunc
	cards          *dingCardClient // non-nil iff cardTemplateID is set
	log            Logger

	mu     sync.Mutex
	cli    *client.StreamClient
	state  State
	detail string
}

// NewDingTalkBot builds a DingTalk bot. stream runs one engine turn for an
// incoming message; log receives operational notices. When cardTemplateID is
// non-empty, replies stream incrementally into a DingTalk AI card bound to that
// template; otherwise they're sent as a single Markdown message.
func NewDingTalkBot(name, clientID, clientSecret, cardTemplateID string, stream StreamReplyFunc, log Logger) Bot {
	b := &dingTalkBot{
		name: name, clientID: clientID, clientSecret: clientSecret,
		cardTemplateID: cardTemplateID, stream: stream, log: log, state: StateStopped,
	}
	if cardTemplateID != "" {
		b.cards = newDingCardClient(clientID, clientSecret)
	}
	return b
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
	b.log.Info("钉钉机器人已连接（Stream 模式）")
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
// the conversation and replies — streaming into an AI card when a template is
// configured, otherwise as one Markdown message.
func (b *dingTalkBot) onMessage(ctx context.Context, data *chatbot.BotCallbackDataModel) ([]byte, error) {
	const ack = "" // empty body: the SDK only needs a non-error return
	text := strings.TrimSpace(data.Text.Content)
	if text == "" {
		return []byte(ack), nil
	}
	if b.cards != nil {
		b.replyViaCard(ctx, data, text)
	} else {
		b.replyViaText(ctx, data, text)
	}
	return []byte(ack), nil
}

// replyViaText runs the turn and sends the final answer as one Markdown message.
func (b *dingTalkBot) replyViaText(ctx context.Context, data *chatbot.BotCallbackDataModel, text string) {
	replier := chatbot.NewChatbotReplier()
	answer, err := b.stream(ctx, data.ConversationId, text, nil)
	if err != nil {
		b.log.Error("钉钉机器人处理消息失败", logging.Err(err))
		_ = replier.SimpleReplyText(ctx, data.SessionWebhook, []byte("处理消息出错："+err.Error()))
		return
	}
	if strings.TrimSpace(answer) == "" {
		answer = "（无回复）"
	}
	if err := replier.SimpleReplyMarkdown(ctx, data.SessionWebhook, []byte("jelly-agent"), []byte(answer)); err != nil {
		b.log.Error("钉钉机器人回复失败", logging.Err(err))
	}
}

// replyViaCard creates a streaming AI card and feeds the answer into it as it's
// generated; it falls back to a text reply if the card can't be created.
func (b *dingTalkBot) replyViaCard(ctx context.Context, data *chatbot.BotCallbackDataModel, text string) {
	trackID, err := b.cards.createCard(ctx, b.cardTemplateID, data.ConversationType, data.ConversationId, data.SenderStaffId, text)
	if err != nil {
		b.log.Warn("钉钉卡片创建失败，回退为文本", logging.Err(err))
		b.replyViaText(ctx, data, text)
		return
	}
	// Throttle card updates so we don't hammer the streaming API per token.
	var lastPush time.Time
	onUpdate := func(full string) {
		if time.Since(lastPush) < 400*time.Millisecond {
			return
		}
		lastPush = time.Now()
		if err := b.cards.streamCard(ctx, trackID, full, false); err != nil {
			b.log.Warn("钉钉卡片更新失败", logging.Err(err))
		}
	}
	answer, err := b.stream(ctx, data.ConversationId, text, onUpdate)
	if err != nil {
		b.log.Error("钉钉机器人处理消息失败", logging.Err(err))
		answer = "处理消息出错：" + err.Error()
	}
	if strings.TrimSpace(answer) == "" {
		answer = "（无回复）"
	}
	if err := b.cards.streamCard(ctx, trackID, answer, true); err != nil {
		b.log.Warn("钉钉卡片收尾失败", logging.Err(err))
	}
}
