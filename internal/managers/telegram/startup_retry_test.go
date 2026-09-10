package telegram

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestPreflightWithRetry_RetriesEachTransientRemoteCall(t *testing.T) {
	attempts := make(map[string]int)

	manager := &Manager{
		cfg:    config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		target: forumTarget{chatID: -100123, topology: forumTopologyGroup},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			attempts[method]++
			if attempts[method] == 1 {
				return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
			}

			switch method {
			case "getMe":
				return telegramResponse(req, `{"ok":true,"result":{"id":42}}`), nil
			case "getChat":
				return telegramResponse(
					req,
					`{"ok":true,"result":{"id":-100123,"type":"supergroup","is_forum":true}}`,
				), nil
			case "getChatMember":
				return telegramResponse(
					req,
					`{"ok":true,"result":{"status":"administrator","can_manage_topics":true,"can_delete_messages":true}}`,
				), nil
			default:
				t.Fatalf("unexpected Telegram method %q", method)
				return nil, nil
			}
		})},
	}

	policy := startupRetryPolicy{
		timeout: time.Second,
		base:    time.Millisecond,
		max:     time.Millisecond,
		wait:    func(context.Context, time.Duration) error { return nil },
	}

	require.NoError(t, manager.preflightWithRetry(context.Background(), policy))
	assert.Equal(t, map[string]int{
		"getMe":         2,
		"getChat":       2,
		"getChatMember": 2,
	}, attempts)
}

func TestPreflightWithRetry_RetriesHTTPServerError(t *testing.T) {
	calls := 0
	manager := &Manager{
		cfg:    config.ManagerEntry{BotToken: "token", AllowedUserIDs: []int64{7}},
		target: forumTarget{chatID: 7, topology: forumTopologyBot},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			if method == "getMe" {
				calls++
				if calls == 1 {
					return &http.Response{
						StatusCode: http.StatusBadGateway,
						Status:     "502 Bad Gateway",
						Body:       io.NopCloser(strings.NewReader("bad gateway")),
						Header:     make(http.Header),
						Request:    req,
					}, nil
				}

				return telegramResponse(req, `{"ok":true,"result":{"id":42,"has_topics_enabled":true}}`), nil
			}

			return telegramResponse(req, `{"ok":true,"result":{"id":7,"type":"private"}}`), nil
		})},
	}

	policy := startupRetryPolicy{
		timeout: time.Second,
		base:    time.Millisecond,
		max:     time.Millisecond,
		wait:    func(context.Context, time.Duration) error { return nil },
	}

	require.NoError(t, manager.preflightWithRetry(context.Background(), policy))
	assert.Equal(t, 2, calls)
}

func TestRetryStartupCall_HonorsRetryAfter(t *testing.T) {
	calls := 0
	var delays []time.Duration
	policy := startupRetryPolicy{
		base: time.Second,
		max:  time.Minute,
		wait: func(_ context.Context, delay time.Duration) error {
			delays = append(delays, delay)
			return nil
		},
	}

	err := retryStartupCall(context.Background(), policy, func(context.Context) error {
		calls++
		if calls == 1 {
			return &tgAPIError{Method: "getMe", ErrorCode: http.StatusTooManyRequests, RetryAfter: 7}
		}

		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, []time.Duration{7 * time.Second}, delays)
}

func TestRetryStartupCall_RetriesServerError(t *testing.T) {
	calls := 0
	waits := 0
	err := retryStartupCall(context.Background(), startupRetryPolicy{
		base: time.Second,
		max:  time.Second,
		wait: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	}, func(context.Context) error {
		calls++
		if calls == 1 {
			return &tgAPIError{Method: "getChat", ErrorCode: http.StatusBadGateway}
		}

		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, 2, calls)
	assert.Equal(t, 1, waits)
}

func TestRetryStartupCall_DoesNotRetryPermanentFailures(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "authentication", err: &tgAPIError{Method: "getMe", ErrorCode: http.StatusUnauthorized}},
		{name: "malformed response", err: errors.New("parse telegram response: invalid character")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			waits := 0
			policy := startupRetryPolicy{
				base: time.Millisecond,
				max:  time.Millisecond,
				wait: func(context.Context, time.Duration) error {
					waits++
					return nil
				},
			}

			err := retryStartupCall(context.Background(), policy, func(context.Context) error {
				calls++
				return tt.err
			})

			require.ErrorIs(t, err, tt.err)
			assert.Equal(t, 1, calls)
			assert.Zero(t, waits)
		})
	}
}

func TestRetryStartupCall_DoesNotRetryPermanentNetworkErrors(t *testing.T) {
	calls := 0
	waits := 0
	err := retryStartupCall(context.Background(), startupRetryPolicy{
		base: time.Millisecond,
		max:  time.Millisecond,
		wait: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
	}, func(context.Context) error {
		calls++
		return permanentNetworkError{}
	})

	require.Error(t, err)
	assert.Equal(t, 1, calls)
	assert.Zero(t, waits)
}

func TestStartupRetryDelay_ClampsOverflow(t *testing.T) {
	delay := startupRetryDelay(&tgAPIError{RetryAfter: int(^uint(0) >> 1)}, time.Second)

	assert.Greater(t, delay, time.Duration(0))
}

func TestRetryStartupCall_BoundsPersistentTransportFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	calls := 0
	started := time.Now()
	err := retryStartupCall(ctx, startupRetryPolicy{
		base: time.Millisecond,
		max:  2 * time.Millisecond,
	}, func(context.Context) error {
		calls++
		return &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
	})

	require.Error(t, err)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Greater(t, calls, 1)
	assert.Less(t, time.Since(started), time.Second)
}

func TestManagerStart_CallerCancellationStopsPreflight(t *testing.T) {
	requestStarted := make(chan struct{})
	manager := managerBlockedInPreflight(requestStarted)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan error, 1)
	go func() { startDone <- manager.Start(ctx) }()

	<-requestStarted
	cancel()

	err := <-startDone
	require.ErrorIs(t, err, context.Canceled)
}

func TestManagerStop_CancelsPreflight(t *testing.T) {
	requestStarted := make(chan struct{})
	manager := managerBlockedInPreflight(requestStarted)

	startDone := make(chan error, 1)
	go func() { startDone <- manager.Start(context.Background()) }()

	<-requestStarted
	stopDone := make(chan error, 1)
	go func() { stopDone <- manager.Stop(context.Background()) }()

	require.NoError(t, <-stopDone)
	require.ErrorIs(t, <-startDone, context.Canceled)
}

type permanentNetworkError struct{}

func (permanentNetworkError) Error() string   { return "permanent network failure" }
func (permanentNetworkError) Timeout() bool   { return false }
func (permanentNetworkError) Temporary() bool { return false }

func managerBlockedInPreflight(requestStarted chan<- struct{}) *Manager {
	return &Manager{
		id:         "telegram-test",
		cfg:        config.ManagerEntry{BotToken: "token", TargetChatID: targetID(-100123)},
		target:     forumTarget{chatID: -100123, topology: forumTopologyGroup},
		controller: &fakeController{},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			select {
			case requestStarted <- struct{}{}:
			default:
			}

			<-req.Context().Done()
			return nil, req.Context().Err()
		})},
	}
}
