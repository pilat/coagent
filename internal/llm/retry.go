package llm

import (
	"context"
	"fmt"
	"math"
	"math/rand"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
)

const (
	llmRetryBaseDelay     = 1 * time.Second
	llmRetryMaxDelay      = 60 * time.Second
	llmRetryWarnThreshold = 3
	maxRetryAttempts      = 6
	defaultModelTimeout   = 10 * time.Minute
)

type retryableClient struct {
	inner         Client
	baseDelay     time.Duration
	maxDelay      time.Duration
	warnThreshold int
	timeout       time.Duration // per-request deadline applied to each attempt
}

var _ Client = (*retryableClient)(nil)

func newRetryableClient(inner Client, timeout time.Duration) Client {
	if timeout <= 0 {
		timeout = defaultModelTimeout
	}

	return &retryableClient{
		inner:         inner,
		baseDelay:     llmRetryBaseDelay,
		maxDelay:      llmRetryMaxDelay,
		warnThreshold: llmRetryWarnThreshold,
		timeout:       timeout,
	}
}

func (r *retryableClient) Chat(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	var lastErr error

	for attempt := range maxRetryAttempts {
		if attempt > 0 {
			delay := r.calculateDelay(attempt)

			if attempt < r.warnThreshold {
				logger.Ctx(ctx).Named("llm.client").
					Debug("retrying", zap.Int("attempt", attempt), zap.Duration("delay", delay), zap.Error(lastErr))
			} else {
				logger.Ctx(ctx).Named("llm.client").
					Warn("retrying", zap.Int("attempt", attempt), zap.Duration("delay", delay), zap.Error(lastErr))
			}

			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(delay):
			}
		}

		resp, err := r.callOnce(ctx, systemPrompt, messages, tools, opts...)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		if ctx.Err() != nil {
			return nil, ctx.Err() // caller cancelled — propagate, don't reclassify
		}

		if !r.shouldRetry(err) {
			logger.Ctx(ctx).Named("llm.client").Warn("non_retryable_error", zap.Error(err))
			return nil, err
		}
	}

	logger.Ctx(ctx).Named("llm.client").
		Warn("retries_exhausted", zap.Int("attempts", maxRetryAttempts), zap.Error(lastErr))

	return nil, lastErr
}

func (r *retryableClient) Model() string {
	return r.inner.Model()
}

func (r *retryableClient) APIKey() string {
	return r.inner.APIKey()
}

func (r *retryableClient) Close() error {
	if err := r.inner.Close(); err != nil {
		return fmt.Errorf("close inner client: %w", err)
	}

	return nil
}

func (r *retryableClient) Provider() string {
	return r.inner.Provider()
}

func (r *retryableClient) ContextWindow() int {
	return r.inner.ContextWindow()
}

func (r *retryableClient) SetReasoningLevel(level string) {
	r.inner.SetReasoningLevel(level)
}

func (r *retryableClient) GetReasoningLevel() string {
	return r.inner.GetReasoningLevel()
}

func (r *retryableClient) SetSessionID(id string) {
	r.inner.SetSessionID(id)
}

// callOnce runs a single attempt under the per-request model timeout. The deadline
// is per attempt, not shared across retries.
func (r *retryableClient) callOnce(
	ctx context.Context,
	systemPrompt string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	attemptCtx, cancel := context.WithTimeout(ctx, r.timeout)
	defer cancel()

	resp, err := r.inner.Chat(attemptCtx, systemPrompt, messages, tools, opts...)
	if err != nil {
		return nil, fmt.Errorf("chat completion: %w", err)
	}

	return resp, nil
}

func (r *retryableClient) shouldRetry(err error) bool {
	if err == nil {
		return false
	}

	errStr := strings.ToLower(err.Error())

	// Non-retryable: billing/credits exhausted — no point retrying until user tops up
	if strings.Contains(errStr, "credits") ||
		strings.Contains(errStr, "insufficient") ||
		strings.Contains(errStr, "payment required") ||
		strings.Contains(errStr, "402") {
		return false
	}

	// Retryable: quota/limit errors (even if wrapped in 403)
	// These are transient — key limits can be raised, quotas reset.
	if strings.Contains(errStr, "key limit") ||
		strings.Contains(errStr, "quota") ||
		strings.Contains(errStr, "limit exceeded") {
		return true
	}

	// Non-retryable: auth errors
	if strings.Contains(errStr, "401") ||
		strings.Contains(errStr, "403") ||
		strings.Contains(errStr, "unauthorized") ||
		strings.Contains(errStr, "forbidden") ||
		strings.Contains(errStr, "invalid api key") {
		return false
	}

	// Non-retryable: bad request (likely client error)
	if strings.Contains(errStr, "400") ||
		strings.Contains(errStr, "bad request") ||
		strings.Contains(errStr, "invalid request") {
		return false
	}

	// Retryable: rate limits
	if strings.Contains(errStr, "429") ||
		strings.Contains(errStr, "rate limit") ||
		strings.Contains(errStr, "too many requests") {
		return true
	}

	// Retryable: server errors
	if strings.Contains(errStr, "500") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "504") ||
		strings.Contains(errStr, "internal server error") ||
		strings.Contains(errStr, "bad gateway") ||
		strings.Contains(errStr, "service unavailable") ||
		strings.Contains(errStr, "gateway timeout") {
		return true
	}

	// Retryable: network errors
	if strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "connection refused") ||
		strings.Contains(errStr, "no such host") ||
		strings.Contains(errStr, "temporary") ||
		strings.Contains(errStr, "unavailable") {
		return true
	}

	// Default: retry on unknown errors (conservative)
	return true
}

func (r *retryableClient) calculateDelay(attempt int) time.Duration {
	const (
		backoffMultiplier = 2.0
		jitterPercent     = 0.25
	)

	// Exponential backoff: baseDelay * 2^(attempt-1)
	delay := float64(r.baseDelay) * math.Pow(backoffMultiplier, float64(attempt-1))

	// Add jitter (±25%) to avoid thundering herd
	jitter := delay * jitterPercent * (2*rand.Float64() - 1)
	delay += jitter

	// Cap at maxDelay (after jitter to ensure we never exceed max)
	if delay > float64(r.maxDelay) {
		delay = float64(r.maxDelay)
	}

	return time.Duration(delay)
}
