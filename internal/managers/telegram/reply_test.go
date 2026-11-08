package telegram

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
)

type replyLoggingController struct {
	*fakeController
}

func TestReplyLogsFailure(t *testing.T) {
	for _, test := range []struct {
		name     string
		command  string
		response string
		html     bool
		wantLogs int
	}{
		{
			name:     "api error",
			command:  "/sessions",
			response: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			wantLogs: 1,
		},
		{
			name:     "html api error",
			command:  "/model",
			response: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			html:     true,
			wantLogs: 1,
		},
		{
			name:     "api error without command",
			response: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			wantLogs: 1,
		},
		{
			name:     "html api error without command",
			response: `{"ok":false,"error_code":400,"description":"Bad Request: chat not found"}`,
			html:     true,
			wantLogs: 1,
		},
		{name: "success", response: `{"ok":true,"result":{"message_id":42}}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.DebugLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			calls := 0
			m := &Manager{
				cfg: config.ManagerEntry{BotToken: "fake", AllowedUserIDs: []int64{1}},
				httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
					calls++
					return telegramJSONResponse(test.response), nil
				})},
			}

			if test.html {
				m.replyHTML(ctx, test.command, "<b>reply</b>")
			} else {
				m.reply(ctx, test.command, "reply")
			}

			assert.Equal(t, 1, calls)
			require.Equal(t, test.wantLogs, logs.Len())
			if test.wantLogs == 0 {
				return
			}

			entry := logs.All()[0]
			assert.Equal(t, "reply_failed", entry.Message)
			assert.Equal(t, zap.WarnLevel, entry.Level)
			assert.Equal(t, "telegram.reply", entry.LoggerName)
			if test.command == "" {
				assert.NotContains(t, entry.ContextMap(), "command")
			} else {
				assert.Equal(t, test.command, entry.ContextMap()["command"])
			}

			assert.Equal(t, int64(400), entry.ContextMap()["code"])
			assert.Equal(t, "Bad Request: chat not found", entry.ContextMap()["description"])
			assert.Contains(t, entry.ContextMap()["error"], "telegram sendMessage failed (code 400)")
		})
	}
}

func TestCommandReplyLogsCommand(t *testing.T) {
	for _, command := range []string{"/sessions", "/model", "/new", "/spawn", "/gwt", "/switch"} {
		t.Run(command, func(t *testing.T) {
			t.Parallel()
			core, logs := observer.New(zap.InfoLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			ctrl := &replyLoggingController{fakeController: &fakeController{focus: 42, listModels: pickerModels}}
			m := &Manager{
				cfg:        config.ManagerEntry{BotToken: "fake", AllowedUserIDs: []int64{1}},
				controller: ctrl,
				chat:       ctrl,
				httpClient: &http.Client{Transport: &failingTransport{err: context.Canceled}},
			}

			m.handleChatText(ctx, command)

			failures := logs.FilterMessage("reply_failed")
			require.Equal(t, 1, failures.Len())
			entry := failures.All()[0]
			assert.Equal(t, "reply_failed", entry.Message)
			assert.Equal(t, zap.WarnLevel, entry.Level)
			assert.Equal(t, command, entry.ContextMap()["command"])
			assert.Contains(t, entry.ContextMap()["error"], "context canceled")
			assert.NotContains(t, entry.ContextMap(), "code")
			assert.NotContains(t, entry.ContextMap(), "description")
			if command == "/model" {
				downstream := logs.FilterMessage("list_models")
				require.Equal(t, 1, downstream.Len())
				assert.NotContains(t, downstream.All()[0].ContextMap(), "command")
			}
		})
	}
}

func (c *replyLoggingController) ListModels(ctx context.Context) (*controllerapi.ConfigModelsResultData, error) {
	logger.Ctx(ctx).Named("controller").Info("list_models")
	return c.fakeController.ListModels(ctx)
}
