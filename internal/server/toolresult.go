package server

import "fmt"

// ADK reports a failed tool through the ordinary FunctionResponse channel: the
// call completes at the protocol level and the payload carries an "error" key.
// A response existing is therefore not the same as the tool having succeeded —
// a distinction the transcript, the SSE stream and the usage stats all need, so
// the judgement lives here once rather than being re-derived (or forgotten) at
// each of those three call sites.
const toolErrorKey = "error"

// toolError returns the error a tool response carries, or "" when the call
// succeeded. A present-but-empty error counts as success, so a tool that always
// sets the key is not reported as permanently failing.
func toolError(resp map[string]any) string {
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

// toolFailed reports whether a tool response represents a failure.
func toolFailed(resp map[string]any) bool { return toolError(resp) != "" }
