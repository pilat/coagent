package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

type failingTransport struct {
	err error
}

func (ft *failingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, ft.err
}

func TestTGTransportErrorOmitsToken(t *testing.T) {
	const token = "8288787998:AAGsecrettoken" //nolint:gosec // fake token

	m := &Manager{
		cfg:        config.ManagerEntry{BotToken: token},
		httpClient: &http.Client{Transport: &failingTransport{err: context.Canceled}},
	}

	err := m.tg(context.Background(), "getUpdates", map[string]any{}, nil)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), token)
	assert.NotContains(t, err.Error(), "api.telegram.org")
	assert.Contains(t, err.Error(), "call telegram getUpdates")
	assert.ErrorIs(t, err, context.Canceled)
}

func TestTGUsesConfiguredAPIURL(t *testing.T) {
	var gotURL string
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "fake", APIURL: "http://127.0.0.1:8081"},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			gotURL = req.URL.String()
			return telegramJSONResponse(`{"ok":true,"result":{}}`), nil
		})},
	}

	require.NoError(t, m.tg(context.Background(), "getMe", nil, nil))
	assert.Equal(t, "http://127.0.0.1:8081/botfake/getMe", gotURL)
}

func TestSanitizeTransportError(t *testing.T) {
	cause := errors.New("connection refused")
	wrapped := &url.Error{Op: "Post", URL: "https://api.telegram.org/botSECRET/getUpdates", Err: cause}

	assert.Equal(t, cause, sanitizeTransportError(wrapped))

	plain := errors.New("no url inside")
	assert.Equal(t, plain, sanitizeTransportError(plain))
}

func TestSendMessage_PlainFallbackOnlyForExplicitEntityParseRejection(t *testing.T) {
	calls := 0
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "fake", TargetChatID: targetID(1)},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			var body map[string]any
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))

			if calls == 1 {
				assert.Equal(t, tgParseModeHTML, body[tgKeyParseMode])
				return telegramJSONResponse(`{
					"ok":false,"error_code":400,
					"description":"Bad Request: can't parse entities: unsupported tag"
				}`), nil
			}

			assert.NotContains(t, body, tgKeyParseMode)
			return telegramJSONResponse(`{"ok":true,"result":{"message_id":42}}`), nil
		})},
	}

	id, err := m.sendMessage(context.Background(), "<broken>", nil, 7)
	require.NoError(t, err)
	assert.Equal(t, int64(42), id)
	assert.Equal(t, 2, calls)
}

func TestSendMessage_DoesNotDuplicateOnRateLimitOrAmbiguousTransportFailure(t *testing.T) {
	tests := []struct {
		name      string
		response  *http.Response
		transport error
	}{
		{
			name: "rate limit",
			response: telegramJSONResponse(`{
				"ok":false,"error_code":429,"description":"Too Many Requests",
				"parameters":{"retry_after":1}
			}`),
		},
		{name: "ambiguous transport", transport: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			m := &Manager{
				cfg: config.ManagerEntry{BotToken: "fake", TargetChatID: targetID(1)},
				httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return tt.response, tt.transport
				})},
			}

			_, err := m.sendMessage(context.Background(), "one logical message", nil, 7)
			require.Error(t, err)
			assert.Equal(t, 1, calls, "an uncertain first send must never trigger a second message")
		})
	}
}

func TestCreateForumTopicRejectsMissingThreadID(t *testing.T) {
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "fake", TargetChatID: targetID(1)},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return telegramJSONResponse(`{"ok":true,"result":{}}`), nil
		})},
	}

	_, err := m.createForumTopic(context.Background(), "Coagent", "")
	require.ErrorContains(t, err, "invalid message_thread_id")
}

func telegramJSONResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
