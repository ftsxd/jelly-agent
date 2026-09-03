package metrics

import (
	"context"
	"errors"
	"os"
	"strings"
)

// ErrKind is a coarse cause bucket for a failed tool call. The point of
// bucketing is attribution: a run where every failure is BadArgs says the model
// is filling parameters wrong, and a run where every failure is Upstream says a
// dependency is down. Those two lead to completely different fixes, and a bare
// success-rate number tells them apart not at all.
type ErrKind string

const (
	ErrNone     ErrKind = ""
	ErrBadArgs  ErrKind = "bad_args" // rejected before doing any work
	ErrTimeout  ErrKind = "timeout"
	ErrCanceled ErrKind = "canceled" // the caller went away; not the tool's fault
	ErrAuth     ErrKind = "auth"
	ErrNotFound ErrKind = "not_found" // the target did not exist
	ErrUpstream ErrKind = "upstream"  // the external system failed
	ErrUnknown  ErrKind = "unknown"
)

// classify buckets a failure from the error value and message. Order matters:
// the checks run from most specific to least, and Unknown is a real answer
// rather than a fallback to be embarrassed about — a large Unknown share is a
// signal that this classifier needs another case, which is only visible if
// Unknown is not quietly folded into something else.
func classify(err error, msg string) ErrKind {
	if err == nil && msg == "" {
		return ErrNone
	}
	if err != nil {
		switch {
		case errors.Is(err, context.Canceled):
			return ErrCanceled
		case errors.Is(err, context.DeadlineExceeded):
			return ErrTimeout
		case errors.Is(err, os.ErrDeadlineExceeded):
			return ErrTimeout
		}
	}
	text := msg
	if err != nil {
		text = err.Error() + " " + msg
	}
	text = strings.ToLower(text)

	switch {
	case containsAny(text, "context canceled", "operation was canceled"):
		return ErrCanceled
	case containsAny(text, "timeout", "timed out", "deadline exceeded", "context deadline"):
		return ErrTimeout
	case containsAny(text, "unauthorized", "forbidden", "permission denied", "401", "403",
		"invalid api key", "authentication"):
		return ErrAuth
	case containsAny(text, "not found", "no such", "404", "does not exist", "notfound"):
		return ErrNotFound
	case containsAny(text, "invalid argument", "invalid parameter", "missing required",
		"unmarshal", "cannot parse", "invalid json", "validation", "required field",
		"unknown field", "bad request", "400"):
		return ErrBadArgs
	case containsAny(text, "connection refused", "no route to host", "eof", "reset by peer",
		"dial tcp", "dns", "unavailable", "502", "503", "504", "server error", "500"):
		return ErrUpstream
	}
	return ErrUnknown
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
