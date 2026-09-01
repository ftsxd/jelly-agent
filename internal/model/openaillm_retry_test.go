package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/model"
	"google.golang.org/genai"
)

// fastBackoff shrinks the retry schedule so tests don't sleep for seconds.
func fastBackoff(t *testing.T) {
	t.Helper()
	prev := retryBaseDelay
	retryBaseDelay = time.Millisecond
	t.Cleanup(func() { retryBaseDelay = prev })
}

func testRequest() *adkmodel.LLMRequest {
	return &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText("hello", genai.RoleUser)},
	}
}

const okCompletion = `{"id":"1","object":"chat.completion","model":"m",
	"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

// drain consumes the response iterator, returning the last response and error.
func drain(seq func(func(*adkmodel.LLMResponse, error) bool)) (*adkmodel.LLMResponse, error) {
	var last *adkmodel.LLMResponse
	var lastErr error
	seq(func(r *adkmodel.LLMResponse, err error) bool {
		if err != nil {
			lastErr = err
			return false
		}
		last = r
		return true
	})
	return last, lastErr
}

func newTestLLM(t *testing.T, h http.HandlerFunc) *OpenAILLM {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return New(ProviderConfig{Name: "test", BaseURL: srv.URL, APIKey: "k", Model: "m"})
}

func TestGenerateOnceRetriesTransientFailure(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	m := newTestLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down","type":"rate_limit"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okCompletion))
	})

	resp, err := drain(m.GenerateContent(context.Background(), testRequest(), false))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if resp == nil {
		t.Fatal("no response after retry")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2 (one failure + one retry)", got)
	}
}

func TestGenerateOnceDoesNotRetryAuthFailure(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	m := newTestLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key","type":"invalid_request_error"}}`))
	})

	if _, err := drain(m.GenerateContent(context.Background(), testRequest(), false)); err == nil {
		t.Fatal("want error for 401")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d calls, want 1 (401 must not be retried)", got)
	}
}

func TestGenerateOnceGivesUpAfterMaxRetries(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	one := 1
	m := New(ProviderConfig{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: &one})

	if _, err := drain(m.GenerateContent(context.Background(), testRequest(), false)); err == nil {
		t.Fatal("want error after exhausting retries")
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2 (initial + 1 retry)", got)
	}
}

func TestMaxRetriesZeroDisablesRetrying(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	zero := 0
	m := New(ProviderConfig{BaseURL: srv.URL, APIKey: "k", Model: "m", MaxRetries: &zero})

	if _, err := drain(m.GenerateContent(context.Background(), testRequest(), false)); err == nil {
		t.Fatal("want error")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d calls, want 1 (retries disabled)", got)
	}
}

// A stream that fails before emitting anything is safe to replay.
func TestStreamRetriesBeforeFirstDelta(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	m := newTestLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
			"data: [DONE]\n\n"))
	})

	var text strings.Builder
	var gotErr error
	m.GenerateContent(context.Background(), testRequest(), true)(func(r *adkmodel.LLMResponse, err error) bool {
		if err != nil {
			gotErr = err
			return false
		}
		if r != nil && r.Partial && r.Content != nil {
			for _, p := range r.Content.Parts {
				text.WriteString(p.Text)
			}
		}
		return true
	})
	if gotErr != nil {
		t.Fatalf("stream error: %v", gotErr)
	}
	if got := calls.Load(); got != 2 {
		t.Errorf("made %d calls, want 2", got)
	}
	if text.String() != "hi" {
		t.Errorf("streamed text = %q, want %q", text.String(), "hi")
	}
}

// Once deltas have reached the consumer a replay would duplicate text on
// screen, so the failure must surface instead of being retried.
func TestStreamDoesNotRetryAfterEmitting(t *testing.T) {
	fastBackoff(t)
	var calls atomic.Int32
	m := newTestLLM(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n"))
		w.(http.Flusher).Flush()
		// Drop the connection mid-stream without sending [DONE].
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
		}
	})

	var text strings.Builder
	var gotErr error
	m.GenerateContent(context.Background(), testRequest(), true)(func(r *adkmodel.LLMResponse, err error) bool {
		if err != nil {
			gotErr = err
			return false
		}
		if r != nil && r.Content != nil {
			for _, p := range r.Content.Parts {
				text.WriteString(p.Text)
			}
		}
		return true
	})
	if gotErr == nil {
		t.Fatal("want mid-stream error to surface")
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("made %d calls, want 1 (no replay after emitting)", got)
	}
	if !strings.Contains(text.String(), "partial") {
		t.Errorf("consumer lost the already-emitted delta: %q", text.String())
	}
}

func TestGenerationParamsAreSent(t *testing.T) {
	var body string
	temp := 0.3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okCompletion))
	}))
	defer srv.Close()
	m := New(ProviderConfig{BaseURL: srv.URL, APIKey: "k", Model: "m", Temperature: &temp, MaxTokens: 256})

	if _, err := drain(m.GenerateContent(context.Background(), testRequest(), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	for _, want := range []string{`"temperature":0.3`, `"max_tokens":256`} {
		if !strings.Contains(body, want) {
			t.Errorf("request body missing %s:\n%s", want, body)
		}
	}
}

// Unset generation params must not appear at all, leaving the endpoint's own
// defaults in charge.
func TestGenerationParamsOmittedWhenUnset(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(okCompletion))
	}))
	defer srv.Close()
	m := New(ProviderConfig{BaseURL: srv.URL, APIKey: "k", Model: "m"})

	if _, err := drain(m.GenerateContent(context.Background(), testRequest(), false)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	for _, bad := range []string{"temperature", "max_tokens"} {
		if strings.Contains(body, bad) {
			t.Errorf("request body should omit %s:\n%s", bad, body)
		}
	}
}
