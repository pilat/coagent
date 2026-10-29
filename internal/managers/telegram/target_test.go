package telegram

import (
	"fmt"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestPreflight_UsesOnlyIdentityAndChat(t *testing.T) {
	for _, chatType := range []string{"private", "group", "supergroup"} {
		t.Run(chatType, func(t *testing.T) {
			var methods []string
			m := &Manager{
				cfg: config.ManagerEntry{BotToken: "test-token", AllowedUserIDs: []int64{7}},
				httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					method := filepath.Base(req.URL.Path)
					methods = append(methods, method)
					if method == "getMe" {
						return telegramResponse(req, `{"ok":true,"result":{"id":9}}`), nil
					}
					return telegramResponse(req, fmt.Sprintf(`{"ok":true,"result":{"id":7,"type":%q}}`, chatType)), nil
				})},
			}
			require.NoError(t, m.preflight(t.Context()))
			assert.Equal(t, []string{"getMe", "getChat"}, methods)
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
	require.ErrorContains(t, m.preflight(t.Context()), "invalid bot user id")
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
	require.EqualError(t, m.preflight(t.Context()), "private chat not found; open the bot and send /start first")
}
