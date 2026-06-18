// Package platform connects external messaging platforms (currently DingTalk)
// as message entry points into jelly-agent. Each Bot receives chats from its
// platform and answers them through a ReplyFunc the caller supplies — that
// callback runs one engine turn — so all agent logic stays in internal/engine
// and the platform code only does protocol plumbing.
package platform

import "context"

// ReplyFunc answers one inbound message. sessionKey identifies the conversation
// (the caller maps it to a persistent session); it returns the assistant's final
// text to send back to the platform.
type ReplyFunc func(ctx context.Context, sessionKey, text string) (string, error)

// StreamReplyFunc is like ReplyFunc but streams the answer: onUpdate is called
// with the accumulated text as it grows (for platforms that can render a live,
// updating reply such as a DingTalk AI card), and the final full text is
// returned. onUpdate may be nil to ignore the stream and just take the result.
type StreamReplyFunc func(ctx context.Context, sessionKey, text string, onUpdate func(full string)) (string, error)

// Logf is the minimal logger the server passes in (its "[jelly]" notices).
type Logf func(format string, args ...any)

// State is a bot's connection lifecycle state.
type State string

const (
	StateConnecting State = "connecting"
	StateOnline     State = "online"
	StateError      State = "error"
	StateStopped    State = "stopped"
)

// Status is a snapshot of a bot's identity and connection state for display.
type Status struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	State  State  `json:"state"`
	Detail string `json:"detail,omitempty"` // error message when State == StateError
	QR     string `json:"qr,omitempty"`     // login QR as a data: image URI, set while awaiting scan
}

// Bot is a running platform connection. Start connects and begins receiving,
// returning once connected (or on connect failure). Stop disconnects. Status is
// safe to call concurrently with the others.
type Bot interface {
	Start(ctx context.Context) error
	Stop()
	Status() Status
}
