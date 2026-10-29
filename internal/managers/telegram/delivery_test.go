package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func TestOutputTransport_AppendsEveryMessageAndPreservesCloseContent(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	transport := &outputTransport{manager: manager}
	for _, text := range []string{"first", "second", "Session killed. Cancelled background processes: 3"} {
		assert.Equal(t, managerdelivery.SendOK, transport.Send(t.Context(), text).Class)
	}
	require.Len(t, calls, 3)
	for _, call := range calls {
		assert.Equal(t, "sendMessage", call.Method)
	}
	assert.Equal(t, "first", calls[0].Text)
	assert.Equal(t, "second", calls[1].Text)
	assert.Equal(t, "Session killed. Cancelled background processes: 3", calls[2].Text)
}

func TestClassifyDeliveryError_ClassesAndRetryAfter(t *testing.T) {
	for _, test := range []struct {
		name  string
		err   error
		class managerdelivery.SendClass
		retry time.Duration
	}{
		{name: "network", err: context.DeadlineExceeded, class: managerdelivery.SendTransient},
		{name: "rate limit", err: &tgAPIError{ErrorCode: 429, RetryAfter: 12}, class: managerdelivery.SendTransient, retry: 12 * time.Second},
		{name: "server", err: &tgAPIError{ErrorCode: 503}, class: managerdelivery.SendTransient},
		{name: "blocked", err: &tgAPIError{ErrorCode: 403}, class: managerdelivery.SendChatFailure},
		{name: "missing chat", err: &tgAPIError{ErrorCode: 400, Description: "Bad Request: chat not found"}, class: managerdelivery.SendChatFailure},
		{name: "bad row", err: &tgAPIError{ErrorCode: 400, Description: "message is too long"}, class: managerdelivery.SendRowPermanent},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := classifyDeliveryError(test.err)
			assert.Equal(t, test.class, result.Class)
			assert.Equal(t, test.retry, result.RetryAfter)
			assert.NotEmpty(t, result.Reason)
		})
	}
}

func TestClassifyDeliveryError_TruncatesUTF8Safely(t *testing.T) {
	result := classifyDeliveryError(errors.New("x" + strings.Repeat("😀", 128)))
	assert.LessOrEqual(t, len(result.Reason), 512)
	assert.True(t, utf8.ValidString(result.Reason))
}

func TestOutputTransport_ParseFallbackRetriesOnlyRejectedChunk(t *testing.T) {
	var texts []string
	var modes []string
	manager := &Manager{
		cfg: config.ManagerEntry{BotToken: "test-token", TargetChatID: new(int64(1))},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var body map[string]any
			require.NoError(t, json.NewDecoder(req.Body).Decode(&body))
			texts = append(texts, body["text"].(string))
			mode, _ := body["parse_mode"].(string)
			modes = append(modes, mode)
			if len(texts) == 2 {
				return telegramResponse(req, `{"ok":false,"error_code":400,"description":"can't parse entities"}`), nil
			}
			return telegramResponse(req, `{"ok":true,"result":{"message_id":1}}`), nil
		})},
	}
	result := (&outputTransport{manager: manager}).Send(t.Context(), strings.Repeat("a", maxMessageChunk)+"\nremaining")
	assert.Equal(t, managerdelivery.SendOK, result.Class)
	require.Len(t, texts, 3)
	assert.NotEqual(t, texts[0], texts[1])
	assert.Equal(t, texts[1], texts[2])
	assert.Equal(t, []string{tgParseModeHTML, tgParseModeHTML, ""}, modes)
}

func TestOutputTransport_ParseFallbackReportsItsFinalChatFailure(t *testing.T) {
	calls := 0
	manager := &Manager{
		cfg: config.ManagerEntry{BotToken: "test-token", TargetChatID: new(int64(1))},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return telegramResponse(req, `{"ok":false,"error_code":400,"description":"can't parse entities"}`), nil
			}
			return telegramResponse(req, `{"ok":false,"error_code":403,"description":"bot blocked"}`), nil
		})},
	}
	result := (&outputTransport{manager: manager}).Send(t.Context(), "answer")
	assert.Equal(t, managerdelivery.SendChatFailure, result.Class)
	assert.Contains(t, result.Reason, "bot blocked")
	assert.Equal(t, 2, calls)
}

func TestTelegramCommandAndPollingContract(t *testing.T) {
	var commands []string
	var updates []string
	manager := &Manager{
		cfg: config.ManagerEntry{BotToken: "test-token"},
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var payload struct {
				Commands []map[string]string `json:"commands"`
				Updates  []string            `json:"allowed_updates"`
			}
			require.NoError(t, json.NewDecoder(req.Body).Decode(&payload))
			if filepath.Base(req.URL.Path) == "setMyCommands" {
				for _, command := range payload.Commands {
					commands = append(commands, command["command"])
				}
				return telegramResponse(req, `{"ok":true,"result":true}`), nil
			}
			updates = payload.Updates
			return telegramResponse(req, `{"ok":true,"result":[]}`), nil
		})},
	}
	require.NoError(t, manager.setCommands(t.Context()))
	_, err := manager.getUpdates(t.Context(), 0)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"new", "spawn", "sessions", "switch", "assistant", "kill", "stop", "help", "start",
		"gwt", "model", "clear", "compact", "status", "budget", "schedules", "config", "skill",
	}, commands)
	assert.Equal(t, []string{"message"}, updates)
}
