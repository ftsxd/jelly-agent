package server

import "github.com/jelly-agent/jelly-agent/internal/metrics"

// The judgement of what counts as a failed tool call lives in
// internal/metrics, because that package computes the success rate and must
// agree with the transcript, the SSE stream and the usage stats byte for byte.
// These wrappers keep the call sites in this package short.
func toolError(resp map[string]any) string { return metrics.ToolError(resp) }
func toolFailed(resp map[string]any) bool  { return metrics.ToolFailed(resp) }
