package model

import (
	"context"
	"errors"
	"net"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

const (
	// defaultMaxRetries is deliberately small: a failing key or a bad request is
	// not worth hammering, and the user is usually watching a live stream.
	defaultMaxRetries = 2
	retryMaxDelay     = 8 * time.Second
)

// retryBaseDelay is the first backoff step; a var so tests can shrink it.
var retryBaseDelay = 500 * time.Millisecond

// retryableStatus lists HTTP statuses worth a second attempt. Everything else
// (400 bad request, 401/403 bad key, 404 unknown model, 422) is a configuration
// or prompt problem that a retry would only repeat.
var retryableStatus = map[int]bool{
	408: true, // request timeout
	409: true, // conflict / concurrent update
	425: true, // too early
	429: true, // rate limited
	500: true,
	502: true,
	503: true,
	504: true,
}

// retryable reports whether err is a transient failure worth retrying. A
// cancelled or expired caller context never is — the user hung up.
func retryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	var apiErr *openai.APIError
	if errors.As(err, &apiErr) {
		return retryableStatus[apiErr.HTTPStatusCode]
	}
	var reqErr *openai.RequestError
	if errors.As(err, &reqErr) {
		return retryableStatus[reqErr.HTTPStatusCode]
	}

	// Transport-level failures (connection reset, DNS blip, TLS handshake,
	// response-header timeout) surface as net errors and are worth retrying.
	var netErr net.Error
	if errors.As(err, &netErr) {
		return true
	}
	return errors.Is(err, net.ErrClosed)
}

// backoffDelay returns the wait before the given 0-based retry attempt:
// 500ms, 1s, 2s, 4s, capped at 8s. No jitter — jelly-agent is a single-tenant
// process, so there is no fleet to de-synchronize.
func backoffDelay(attempt int) time.Duration {
	d := retryBaseDelay << attempt
	if d > retryMaxDelay || d <= 0 { // <= 0 guards the shift overflowing
		return retryMaxDelay
	}
	return d
}

// waitBackoff sleeps before the next attempt, returning ctx.Err() if the caller
// cancels while we wait so a hung-up request doesn't linger.
func waitBackoff(ctx context.Context, attempt int) error {
	t := time.NewTimer(backoffDelay(attempt))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
