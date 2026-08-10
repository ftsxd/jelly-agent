package model

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
)

func TestRetryableClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"rate limited", &openai.APIError{HTTPStatusCode: 429}, true},
		{"server error", &openai.APIError{HTTPStatusCode: 500}, true},
		{"bad gateway", &openai.APIError{HTTPStatusCode: 502}, true},
		{"unauthorized", &openai.APIError{HTTPStatusCode: 401}, false},
		{"bad request", &openai.APIError{HTTPStatusCode: 400}, false},
		{"model not found", &openai.APIError{HTTPStatusCode: 404}, false},
		{"request error 503", &openai.RequestError{HTTPStatusCode: 503}, true},
		{"wrapped 429", fmt.Errorf("call: %w", &openai.APIError{HTTPStatusCode: 429}), true},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"net timeout", &net.DNSError{IsTimeout: true}, true},
		{"plain error", errors.New("boom"), false},
	} {
		if got := retryable(tc.err); got != tc.want {
			t.Errorf("%s: retryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A cancelled context must win over a retryable status, so a user who hangs up
// mid-request is not kept waiting through the backoff schedule.
func TestRetryableCancelledBeatsStatus(t *testing.T) {
	err := fmt.Errorf("rate limited: %w", context.Canceled)
	if retryable(err) {
		t.Error("cancelled context reported retryable")
	}
}

func TestBackoffDelayProgression(t *testing.T) {
	want := []time.Duration{
		500 * time.Millisecond,
		1 * time.Second,
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		8 * time.Second, // capped
	}
	for i, w := range want {
		if got := backoffDelay(i); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", i, got, w)
		}
	}
	// A large attempt count must saturate at the cap, not overflow negative.
	if got := backoffDelay(62); got != retryMaxDelay {
		t.Errorf("backoffDelay(62) = %v, want %v", got, retryMaxDelay)
	}
}

func TestWaitBackoffHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := waitBackoff(ctx, 3); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitBackoff = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %v despite cancellation", elapsed)
	}
}
