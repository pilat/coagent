package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

type startupRetryPolicy struct {
	timeout time.Duration
	base    time.Duration
	max     time.Duration
	wait    func(context.Context, time.Duration) error
}

func defaultStartupRetryPolicy() startupRetryPolicy {
	return startupRetryPolicy{
		timeout: startupRetryTimeout,
		base:    reconnectBackoffBase,
		max:     reconnectBackoffMax,
		wait:    waitForStartupRetry,
	}
}

func retryStartupCall(
	ctx context.Context,
	policy startupRetryPolicy,
	call func(context.Context) error,
) error {
	timeout := policy.timeout
	if timeout <= 0 {
		timeout = startupRetryTimeout
	}

	retryCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return retryStartupCallWithContext(retryCtx, policy, call)
}

func retryStartupCallWithContext(
	ctx context.Context,
	policy startupRetryPolicy,
	call func(context.Context) error,
) error {
	backoff := policy.base
	wait := policy.wait

	if wait == nil {
		wait = waitForStartupRetry
	}

	for {
		err := call(ctx)
		if err == nil {
			return nil
		}

		if ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.Canceled) {
				return ctx.Err()
			}

			return startupRetryDeadlineError(ctx.Err(), err)
		}

		if !isRetryableStartupError(err) {
			return err
		}

		delay := startupRetryDelay(err, backoff)
		if err := wait(ctx, delay); err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}

			return startupRetryDeadlineError(err, nil)
		}

		backoff = min(backoff*2, policy.max)
	}
}

func startupRetryDeadlineError(ctxErr, lastErr error) error {
	if lastErr == nil {
		return fmt.Errorf("startup retry deadline exceeded: %w", ctxErr)
	}

	return fmt.Errorf("startup retry deadline exceeded: %w", errors.Join(ctxErr, lastErr))
}

func isRetryableStartupError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}

	if apiErr, ok := errors.AsType[*tgAPIError](err); ok {
		return apiErr.ErrorCode == http.StatusTooManyRequests ||
			(apiErr.ErrorCode >= http.StatusInternalServerError && apiErr.ErrorCode < 600)
	}

	var netErr net.Error

	return (errors.As(err, &netErr) && netErr.Timeout()) ||
		errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func startupRetryDelay(err error, backoff time.Duration) time.Duration {
	var apiErr *tgAPIError
	if errors.As(err, &apiErr) && apiErr.RetryAfter > 0 {
		seconds := int64(apiErr.RetryAfter)
		maxSeconds := int64(time.Duration(1<<63-1) / time.Second)

		if seconds > maxSeconds {
			return time.Duration(1<<63 - 1)
		}

		return time.Duration(seconds) * time.Second
	}

	return backoff
}

func waitForStartupRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
