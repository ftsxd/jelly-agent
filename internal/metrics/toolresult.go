// Package metrics records what the agent's tool calls actually did: how long
// each took, whether it succeeded, and how it failed when it didn't.
//
// This exists because the information is not recoverable after the fact. A
// session's event log keeps the call and its response, so a coarse success rate
// can be scanned out of it — but per-call latency is never written there at
// all, and neither is the distinction between a call the model got wrong and a
// downstream system that was down. Both have to be captured at the call site,
// which is what the ADK tool callbacks are for.
//
// The package owns no policy: it decides nothing about retries, approval or
// tool selection. It writes rows.
package metrics

import "fmt"

// ADK reports a failed tool through the ordinary FunctionResponse channel: the
// call completes at the protocol level and the payload carries an "error" key.
// A response existing is therefore not the same as the tool having succeeded —
// a distinction the transcript, the SSE stream, the usage stats and this
// package's own rows all need, so the judgement lives here once rather than
// being re-derived (or forgotten) at each of those call sites.
const toolErrorKey = "error"

// ToolError returns the error a tool response carries, or "" when the call
// succeeded. A present-but-empty error counts as success, so a tool that always
// sets the key is not reported as permanently failing.
func ToolError(resp map[string]any) string {
	if resp == nil {
		return ""
	}
	v, ok := resp[toolErrorKey]
	if !ok || v == nil {
		return ""
	}
	switch e := v.(type) {
	case string:
		return e
	case error:
		return e.Error()
	default:
		return fmt.Sprintf("%v", e)
	}
}

// ToolFailed reports whether a tool response represents a failure.
func ToolFailed(resp map[string]any) bool { return ToolError(resp) != "" }
