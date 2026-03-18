package llm

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pilat/coagent/internal/llmwire"
)

const testSuccessText = "success"

// mockClient is a mock implementation of Client for testing
type mockClient struct {
	chatCalls     int
	chatResponses []*llmwire.Response
	chatErrors    []error
	model         string
	apiKey        string
}

func (m *mockClient) Chat(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	defer func() { m.chatCalls++ }()
	if m.chatCalls < len(m.chatResponses) {
		return m.chatResponses[m.chatCalls], m.chatErrors[m.chatCalls]
	}
	// Return last error/response if exhausted
	if len(m.chatErrors) > 0 {
		return nil, m.chatErrors[len(m.chatErrors)-1]
	}
	return &llmwire.Response{Text: "default"}, nil
}

func (m *mockClient) Model() string                  { return m.model }
func (m *mockClient) APIKey() string                 { return m.apiKey }
func (m *mockClient) Close() error                   { return nil }
func (m *mockClient) Provider() string               { return "mock" }
func (m *mockClient) ContextWindow() int             { return 0 }
func (m *mockClient) SetReasoningLevel(level string) {}
func (m *mockClient) GetReasoningLevel() string      { return "medium" }
func (m *mockClient) SetSessionID(id string)         {}

var _ Client = (*mockClient)(nil)

func TestRetryableClient_SuccessOnFirstCall(t *testing.T) {
	inner := &mockClient{
		chatResponses: []*llmwire.Response{{Text: testSuccessText}},
		chatErrors:    []error{nil},
	}

	client := newRetryableClient(inner, 0)
	ctx := context.Background()

	resp, err := client.Chat(ctx, "", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != testSuccessText {
		t.Errorf("expected 'success', got %q", resp.Text)
	}
	if inner.chatCalls != 1 {
		t.Errorf("expected 1 call, got %d", inner.chatCalls)
	}
}

func TestRetryableClient_SuccessAfterRetries(t *testing.T) {
	inner := &mockClient{
		chatResponses: []*llmwire.Response{
			nil,
			nil,
			{Text: testSuccessText},
		},
		chatErrors: []error{
			errors.New("timeout"),
			errors.New("temporary failure"),
			nil,
		},
	}

	client := newRetryableClient(inner, 0).(*retryableClient)
	client.baseDelay = 1 * time.Millisecond
	client.maxDelay = 1 * time.Millisecond
	ctx := context.Background()

	resp, err := client.Chat(ctx, "", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != testSuccessText {
		t.Errorf("expected 'success', got %q", resp.Text)
	}
	if inner.chatCalls != 3 {
		t.Errorf("expected 3 calls, got %d", inner.chatCalls)
	}
}

func TestRetryableClient_RetriesUntilSuccess(t *testing.T) {
	const failCount = 4 // < maxRetryAttempts, so the retry succeeds before the cap

	responses := make([]*llmwire.Response, failCount+1)

	errs := make([]error, failCount+1)
	for i := range failCount {
		errs[i] = errors.New("timeout")
	}
	responses[failCount] = &llmwire.Response{Text: testSuccessText}

	inner := &mockClient{
		chatResponses: responses,
		chatErrors:    errs,
	}

	client := newRetryableClient(inner, 0).(*retryableClient)
	client.baseDelay = 1 * time.Millisecond
	client.maxDelay = 1 * time.Millisecond
	ctx := context.Background()

	resp, err := client.Chat(ctx, "", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.Text != testSuccessText {
		t.Errorf("expected %q, got %q", testSuccessText, resp.Text)
	}
	if inner.chatCalls != failCount+1 {
		t.Errorf("expected %d calls, got %d", failCount+1, inner.chatCalls)
	}
}

func TestRetryableClient_StopsOnContextCancel(t *testing.T) {
	inner := &mockClient{
		chatResponses: []*llmwire.Response{nil},
		chatErrors:    []error{errors.New("timeout")},
	}

	client := newRetryableClient(inner, 0).(*retryableClient)
	// Backoff longer than the ctx deadline so cancellation lands mid-wait (not after
	// the attempt cap), proving the loop honors ctx.
	client.baseDelay = 1 * time.Second
	client.maxDelay = 1 * time.Second

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.Chat(ctx, "", nil, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context.DeadlineExceeded, got: %v", err)
	}
	if inner.chatCalls < 1 {
		t.Errorf("expected at least 1 call, got %d", inner.chatCalls)
	}
}

func TestRetryableClient_GivesUpAfterMaxAttempts(t *testing.T) {
	// A retryable error that never recovers must be abandoned after the attempt cap,
	// not retried forever (the incident: a stuck session holding a slot).
	inner := &mockClient{
		chatResponses: []*llmwire.Response{nil},
		chatErrors:    []error{errors.New("503 service unavailable")},
	}

	client := newRetryableClient(inner, 0).(*retryableClient)
	client.baseDelay = 1 * time.Millisecond
	client.maxDelay = 1 * time.Millisecond

	_, err := client.Chat(context.Background(), "", nil, nil)
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}

	// The underlying cause is surfaced so the user sees why, not a bare deadline.
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("expected the underlying error to surface, got: %v", err)
	}

	if inner.chatCalls != maxRetryAttempts {
		t.Errorf("expected exactly %d attempts, got %d", maxRetryAttempts, inner.chatCalls)
	}
}

func TestRetryableClient_NonRetryableError(t *testing.T) {
	tests := []struct {
		name string
		err  string
	}{
		{"401", "HTTP 401: unauthorized"},
		{"403", "HTTP 403: forbidden"},
		{"400", "HTTP 400: bad request"},
		{"invalid api key", "invalid api key provided"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &mockClient{
				chatResponses: []*llmwire.Response{nil},
				chatErrors:    []error{errors.New(tt.err)},
			}

			client := newRetryableClient(inner, 0)
			ctx := context.Background()

			_, err := client.Chat(ctx, "", nil, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			if inner.chatCalls != 1 {
				t.Errorf("expected 1 call for non-retryable error, got %d", inner.chatCalls)
			}
		})
	}
}

func TestRetryableClient_RetryableErrors(t *testing.T) {
	tests := []struct {
		name string
		err  string
	}{
		{"429", "HTTP 429: too many requests"},
		{"500", "HTTP 500: internal server error"},
		{"502", "HTTP 502: bad gateway"},
		{"503", "HTTP 503: service unavailable"},
		{"504", "HTTP 504: gateway timeout"},
		{"timeout", "connection timeout"},
		{"connection refused", "connection refused"},
		{"no such host", "no such host"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inner := &mockClient{
				chatResponses: []*llmwire.Response{nil, {Text: testSuccessText}},
				chatErrors:    []error{errors.New(tt.err), nil},
			}

			client := newRetryableClient(inner, 0).(*retryableClient)
			client.baseDelay = 1 * time.Millisecond
			client.maxDelay = 1 * time.Millisecond
			ctx := context.Background()

			resp, err := client.Chat(ctx, "", nil, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.Text != testSuccessText {
				t.Errorf("expected 'success', got %q", resp.Text)
			}
			if inner.chatCalls != 2 {
				t.Errorf("expected 2 calls (1 retry), got %d", inner.chatCalls)
			}
		})
	}
}

func TestRetryableClient_ContextCancellation(t *testing.T) {
	inner := &mockClient{
		chatResponses: []*llmwire.Response{nil, nil},
		chatErrors:    []error{errors.New("timeout"), nil},
	}

	client := newRetryableClient(inner, 0)
	clientWrapped := client.(*retryableClient)
	// Set a long base delay so we can cancel during retry
	clientWrapped.baseDelay = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())

	// Cancel context after a short delay
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := client.Chat(ctx, "", nil, nil)
	if err == nil {
		t.Fatal("expected error due to context cancellation")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestRetryableClient_PropagatesMethods(t *testing.T) {
	inner := &mockClient{
		model:  "test-model",
		apiKey: "test-key",
	}

	client := newRetryableClient(inner, 0)

	if client.Model() != "test-model" {
		t.Error("Model not propagated")
	}

	if client.APIKey() != "test-key" {
		t.Error("APIKey not propagated")
	}

	if err := client.Close(); err != nil {
		t.Errorf("Close error: %v", err)
	}
}

func TestShouldRetry_Classification(t *testing.T) {
	r := &retryableClient{}

	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		// Non-retryable: billing/credits exhausted
		{"credits exhausted", errors.New("Your credits have been exhausted"), false},
		{"insufficient balance", errors.New("insufficient balance"), false},
		{"402 payment required", errors.New("HTTP 402: payment required"), false},

		// Non-retryable: auth errors
		{"401 unauthorized", errors.New("HTTP 401: unauthorized"), false},
		{"403 forbidden", errors.New("HTTP 403: forbidden"), false},
		{"invalid api key", errors.New("invalid api key provided"), false},

		// Non-retryable: client errors
		{"400 bad request", errors.New("HTTP 400: bad request"), false},
		{"invalid request", errors.New("invalid request format"), false},

		// Retryable: rate limits
		{"429 rate limit", errors.New("HTTP 429: too many requests"), true},
		{"rate limit exceeded", errors.New("rate limit exceeded"), true},

		// Retryable: server errors
		{"500 internal error", errors.New("HTTP 500: internal server error"), true},
		{"502 bad gateway", errors.New("HTTP 502: bad gateway"), true},
		{"503 unavailable", errors.New("HTTP 503: service unavailable"), true},
		{"504 gateway timeout", errors.New("HTTP 504: gateway timeout"), true},

		// Retryable: network errors
		{"timeout", errors.New("connection timeout"), true},
		{"connection refused", errors.New("connection refused"), true},
		{"no such host", errors.New("no such host"), true},
		{"temporary error", errors.New("temporary failure"), true},

		// Default: unknown errors (conservative - retry)
		{"unknown error", errors.New("some random error"), true},
		{"nil error", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.shouldRetry(tt.err)
			if got != tt.expected {
				t.Errorf("shouldRetry(%v) = %v, want %v", tt.err, got, tt.expected)
			}
		})
	}
}

func TestCalculateDelay(t *testing.T) {
	r := &retryableClient{
		baseDelay: 100 * time.Millisecond,
		maxDelay:  1 * time.Second,
	}

	// Test that delay increases with attempt number
	delays := make([]time.Duration, 5)
	for i := 1; i <= 5; i++ {
		delays[i-1] = r.calculateDelay(i)
	}

	// Each delay should be >= previous (accounting for jitter)
	for i := 1; i < len(delays); i++ {
		// With jitter, delays could technically be slightly lower, but
		// base delay should increase
		baseExpected := float64(r.baseDelay) * math.Pow(2, float64(i-1))
		if baseExpected > float64(r.maxDelay) {
			baseExpected = float64(r.maxDelay)
		}
		// Delay should be roughly around baseExpected +- 25%
		minExpected := time.Duration(baseExpected * 0.5) // Allow for jitter
		if delays[i] < minExpected {
			t.Errorf("delay %d (%v) < minimum expected (%v)", i+1, delays[i], minExpected)
		}
	}

	// Test max delay cap
	r.baseDelay = 500 * time.Millisecond
	r.maxDelay = 500 * time.Millisecond
	for i := 1; i <= 10; i++ {
		delay := r.calculateDelay(i)
		if delay > r.maxDelay {
			t.Errorf("delay %d (%v) exceeded max (%v)", i, delay, r.maxDelay)
		}
	}
}
