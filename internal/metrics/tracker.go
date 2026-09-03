package metrics

import (
	"encoding/json"
	"sync"
	"time"
)

// maxPending bounds the in-flight table. A call that starts and never finishes
// (the invocation was cancelled, the process is shutting down) would otherwise
// leak an entry forever.
const maxPending = 512

// pendingTTL is how long an unfinished call is kept before it is treated as
// abandoned. It is generous on purpose: a slow log query legitimately takes
// minutes, and dropping its start time would report the call as instant.
const pendingTTL = 10 * time.Minute

// CallMeta identifies one tool call. It is read from the ADK tool context at
// the moment the call finishes.
type CallMeta struct {
	SessionID    string
	InvocationID string
	Agent        string
	CallID       string
	Tool         string
}

type inflight struct {
	started time.Time
	tool    string
	args    map[string]any
}

// Tracker pairs the before-call and after-call hooks into one row.
//
// It deliberately knows nothing about ADK: the agent runtime adapts its
// callback signatures to Start and Finish. That keeps the measurement rules
// testable without standing up an agent, and keeps a framework upgrade from
// reaching in here.
//
// Every method is safe on a nil Tracker, so wiring can stay unconditional even
// when the database could not be opened.
type Tracker struct {
	rec *Recorder

	mu      sync.Mutex
	pending map[string]inflight
}

// NewTracker returns a tracker writing to rec. A nil rec yields a tracker that
// times calls and discards the rows, which is what a degraded start should do —
// never a reason to skip the tool call itself.
func NewTracker(rec *Recorder) *Tracker {
	return &Tracker{rec: rec, pending: make(map[string]inflight)}
}

// Start notes that a call is in flight. callID must be the ADK
// FunctionCallID — it is the only identifier that distinguishes two concurrent
// calls to the same tool with the same arguments.
func (t *Tracker) Start(callID, tool string, args map[string]any) {
	if t == nil || callID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.pending) >= maxPending {
		t.evictLocked()
	}
	t.pending[callID] = inflight{started: time.Now(), tool: tool, args: args}
}

// Finish records the completed call. It is called whether the tool returned a
// result or an error, because a success rate computed over successes alone is
// not a success rate.
//
// The failure judgement reads both signals: the transport-level err, and the
// error key ADK puts inside an otherwise-successful response (see ToolError).
// A tool that reports its failure in the payload is still a failure.
func (t *Tracker) Finish(m CallMeta, result map[string]any, err error) ToolCall {
	now := time.Now()
	row := ToolCall{
		At:           now,
		SessionID:    m.SessionID,
		InvocationID: m.InvocationID,
		Agent:        m.Agent,
		CallID:       m.CallID,
		Tool:         m.Tool,
		ResultBytes:  resultBytes(result),
	}

	if t != nil && m.CallID != "" {
		t.mu.Lock()
		if in, ok := t.pending[m.CallID]; ok {
			delete(t.pending, m.CallID)
			row.At = in.started
			row.Duration = now.Sub(in.started)
			row.Args = in.args
			if row.Tool == "" {
				row.Tool = in.tool
			}
		}
		t.mu.Unlock()
	}

	msg := ToolError(result)
	if err != nil {
		row.Err = err.Error()
	} else {
		row.Err = msg
	}
	row.OK = err == nil && msg == ""
	if !row.OK {
		row.ErrKind = classify(err, msg)
	}

	if t != nil {
		// A failed insert must not fail the tool call: the row is
		// observability, the call is the product.
		_ = t.rec.Record(row)
	}
	return row
}

// Pending reports how many calls are in flight. Exposed for tests and for a
// health endpoint to notice a leak.
func (t *Tracker) Pending() int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pending)
}

// Recorder exposes the underlying store, for a caller that has its own row to
// write — the gateway, which knows more about a call than this tracker can.
func (t *Tracker) Recorder() *Recorder {
	if t == nil {
		return nil
	}
	return t.rec
}

// Close releases the underlying recorder.
func (t *Tracker) Close() error {
	if t == nil {
		return nil
	}
	return t.rec.Close()
}

// evictLocked drops abandoned entries, then — if the table is still full —
// the oldest one, so Start can never block or grow without bound.
func (t *Tracker) evictLocked() {
	cutoff := time.Now().Add(-pendingTTL)
	for id, in := range t.pending {
		if in.started.Before(cutoff) {
			delete(t.pending, id)
		}
	}
	if len(t.pending) < maxPending {
		return
	}
	var oldestID string
	var oldest time.Time
	for id, in := range t.pending {
		if oldestID == "" || in.started.Before(oldest) {
			oldestID, oldest = id, in.started
		}
	}
	delete(t.pending, oldestID)
}

func resultBytes(result map[string]any) int {
	if len(result) == 0 {
		return 0
	}
	b, err := json.Marshal(result)
	if err != nil {
		return 0
	}
	return len(b)
}
