package telegram

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

func TestPreflight_BotForum(t *testing.T) {
	methods := make([]string, 0, 2)
	m := &Manager{
		cfg:    config.ManagerEntry{BotToken: "token", AllowedUserIDs: []int64{7}},
		target: forumTarget{chatID: 7, topology: forumTopologyBot},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			method := filepath.Base(req.URL.Path)
			methods = append(methods, method)
			if method == "getMe" {
				return telegramResponse(req, `{"ok":true,"result":{"id":9,"has_topics_enabled":true}}`), nil
			}
			return telegramResponse(req, `{"ok":true,"result":{"id":7,"type":"private"}}`), nil
		})},
	}

	require.NoError(t, m.preflight(context.Background()))
	assert.Equal(t, []string{"getMe", "getChat"}, methods)
	assert.Equal(t, int64(9), m.botUserID)
}

func TestPreflight_BotForumRefusalsStopBeforeTopicMutation(t *testing.T) {
	tests := []struct {
		name string
		me   string
		chat string
		want string
	}{
		{"threaded mode", `{"id":9}`, `{"id":7,"type":"private"}`, "Threaded Mode"},
		{
			"user topics",
			`{"id":9,"has_topics_enabled":true,"allows_users_to_create_topics":true}`,
			`{"id":7,"type":"private"}`,
			"Disallow users",
		},
		{"wrong chat", `{"id":9,"has_topics_enabled":true}`, `{"id":7,"type":"supergroup"}`, "private chat"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			methods := make([]string, 0, 2)
			m := &Manager{
				cfg:    config.ManagerEntry{BotToken: "token"},
				target: forumTarget{chatID: 7, topology: forumTopologyBot},
				httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					method := filepath.Base(req.URL.Path)
					methods = append(methods, method)
					if method == "getMe" {
						return telegramResponse(req, `{"ok":true,"result":`+tt.me+`}`), nil
					}
					return telegramResponse(req, `{"ok":true,"result":`+tt.chat+`}`), nil
				})},
			}

			err := m.preflight(context.Background())
			require.ErrorContains(t, err, tt.want)
			assert.NotContains(t, methods, "createForumTopic")
		})
	}
}

func TestPreflight_RejectsMissingBotID(t *testing.T) {
	m := &Manager{
		cfg:    config.ManagerEntry{BotToken: "token"},
		target: forumTarget{chatID: 7, topology: forumTopologyBot},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return telegramResponse(req, `{"ok":true,"result":{"has_topics_enabled":true}}`), nil
		})},
	}

	require.ErrorContains(t, m.preflight(context.Background()), "invalid bot user id")
}
