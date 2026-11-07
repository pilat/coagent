package telegram

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestPreflight_UsesOnlyIdentityAndChat(t *testing.T) {
	t.Run("private", func(t *testing.T) {
		var methods []string
		m := &Manager{
			cfg: config.ManagerEntry{BotToken: "test-token", AllowedUserIDs: []int64{7}},
			httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				method := filepath.Base(req.URL.Path)
				methods = append(methods, method)
				if method == "getMe" {
					return telegramResponse(req, `{"ok":true,"result":{"id":9}}`), nil
				}
				var body struct {
					ChatID int64 `json:"chat_id"`
				}
				require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
				assert.Equal(t, int64(7), body.ChatID)
				return telegramResponse(req, `{"ok":true,"result":{"id":7,"type":"private"}}`), nil
			})},
		}
		require.NoError(t, m.preflightWithRetry(t.Context(), defaultStartupRetryPolicy()))
		assert.Equal(t, []string{"getMe", "getChat"}, methods)
	})
}

func TestNew_RequiresOnePositiveAllowedUser(t *testing.T) {
	for _, tt := range []struct {
		name string
		ids  []int64
	}{
		{name: "missing"},
		{name: "empty", ids: []int64{}},
		{name: "multiple", ids: []int64{7, 8}},
		{name: "duplicate", ids: []int64{7, 7}},
		{name: "zero", ids: []int64{0}},
		{name: "negative", ids: []int64{-7}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			manager, err := New(config.ManagerEntry{
				BotToken: "test-token", AllowedUserIDs: tt.ids,
			}, nil, &fakeController{})
			require.EqualError(t, err, "private chat requires exactly one positive allowed user id")
			assert.Nil(t, manager)
		})
	}
}

func TestPreflight_RejectsMissingBotID(t *testing.T) {
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "test-token", AllowedUserIDs: []int64{7}},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return telegramResponse(req, `{"ok":true,"result":{}}`), nil
		})},
	}
	require.ErrorContains(t, m.preflightWithRetry(t.Context(), defaultStartupRetryPolicy()), "invalid bot user id")
}

func TestPreflight_PrivateChatNotFoundExplainsStart(t *testing.T) {
	m := &Manager{
		cfg: config.ManagerEntry{BotToken: "test-token", AllowedUserIDs: []int64{7}},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if filepath.Base(req.URL.Path) == "getMe" {
				return telegramResponse(req, `{"ok":true,"result":{"id":9}}`), nil
			}
			return telegramResponse(
				req,
				`{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			), nil
		})},
	}
	require.EqualError(
		t,
		m.preflightWithRetry(t.Context(), defaultStartupRetryPolicy()),
		"private chat not found; open the bot and send /start first",
	)
}
