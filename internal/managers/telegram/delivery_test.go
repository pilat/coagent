package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

func TestOutputTransport_ReplacesOnlyAfterAReplaceableReceipt(t *testing.T) {
	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	transport := &outputTransport{manager: manager}

	first := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: "first",
		Attributes: map[string]any{"name": "project", "work_dir": "/tmp/project"},
	}))
	require.Empty(t, first.Error)
	require.Equal(t, []string{"123"}, first.MessageIDs)
	require.Equal(t, map[string]any{"telegram_topic_id": int64(harnessTopicID)}, first.SessionPatch)

	replaced := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_persistent", Content: "second",
		PreviousMessageType:       "message_replaceable",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"123"}},
	}))
	require.Empty(t, replaced.Error)
	assert.Equal(t, []string{"123"}, replaced.MessageIDs)

	appended := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_persistent", Content: "third",
		PreviousMessageType:       "message_persistent",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"123"}},
	}))
	require.Empty(t, appended.Error)
	assert.Equal(t, []string{"123"}, appended.MessageIDs)
	assert.Equal(t, []string{
		"createForumTopic", "sendMessage", "editForumTopic", "editMessageText", "editForumTopic", "sendMessage",
	}, callMethods(calls))
}

func TestOutputTransport_TreatsMissingEditTargetAsNewMessage(t *testing.T) {
	manager := newTelegramHarnessManager(t, &fakeController{}, new([]telegramHarnessCall))
	manager.registerTopic(42, harnessTopicID)
	manager.httpClient = failingEditClient(t)
	transport := &outputTransport{manager: manager}
	result := transport.Deliver(context.Background(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: "replacement",
		PreviousMessageType:       "message_replaceable",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"123"}},
	}))
	require.Empty(t, result.Error)
	assert.Equal(t, []string{"456"}, result.MessageIDs)
}

// A shorter replaceable edits the common prefix and deletes every surplus
// chunk, so the receipt shrinks with the content.
func TestOutputTransport_ShorterReplacementDeletesSurplusChunks(t *testing.T) {
	var ops []chunkOperation
	manager := newTelegramHarnessManager(t, &fakeController{}, new([]telegramHarnessCall))
	manager.registerTopic(42, harnessTopicID)
	manager.httpClient = chunkClient(t, &ops)
	transport := &outputTransport{manager: manager}

	result := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: "short now",
		PreviousMessageType:       "message_replaceable",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"111", "222", "333"}},
	}))
	require.Empty(t, result.Error)
	assert.Equal(t, []string{"111"}, result.MessageIDs)
	assert.Equal(t, []chunkOperation{
		{Method: "editMessageText", MessageID: 111},
		{Method: "deleteMessage", MessageID: 222},
		{Method: "deleteMessage", MessageID: 333},
	}, ops)
}

// A longer replaceable edits what exists and sends only the missing tail.
func TestOutputTransport_LongerReplacementEditsAndAppendsChunks(t *testing.T) {
	var ops []chunkOperation
	manager := newTelegramHarnessManager(t, &fakeController{}, new([]telegramHarnessCall))
	manager.registerTopic(42, harnessTopicID)
	manager.httpClient = chunkClient(t, &ops)
	transport := &outputTransport{manager: manager}

	long := strings.Repeat("word ", 1000) // > maxMessageChunk after rendering
	result := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: long,
		PreviousMessageType:       "message_replaceable",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"111"}},
	}))
	require.Empty(t, result.Error)
	require.Len(t, result.MessageIDs, 2)
	assert.Equal(t, "111", result.MessageIDs[0])
	assert.Equal(t, []chunkOperation{
		{Method: "editMessageText", MessageID: 111},
		{Method: "sendMessage", MessageID: 500},
	}, ops)
}

func TestOutputTransport_DeliveryFailureTruncatesUTF8Safely(t *testing.T) {
	transport := &outputTransport{}
	result := transport.deliveryFailure(assert.AnError)
	assert.True(t, utf8.ValidString(result.Error))

	result = transport.deliveryFailure(errors.New("x" + strings.Repeat("😀", 128)))
	assert.LessOrEqual(t, len(result.Error), 512)
	assert.True(t, utf8.ValidString(result.Error))
}

func outputItem(claim *controllerapi.OutputClaimData) *managerdelivery.Item {
	return &managerdelivery.Item{Payload: claim}
}

func failingEditClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if filepath.Base(req.URL.Path) == "editMessageText" {
			return harnessResponse(
				req,
				`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`,
			), nil
		}
		return harnessResponse(req, `{"ok":true,"result":{"message_id":456}}`), nil
	})}
}

type chunkOperation struct {
	Method    string
	MessageID int64
}

// chunkClient records every edit/delete target and hands out fresh IDs for
// sends, so chunk reconciliation can be asserted operation by operation.
func chunkClient(t *testing.T, ops *[]chunkOperation) *http.Client {
	t.Helper()

	next := 500

	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := filepath.Base(req.URL.Path)

		var body struct {
			MessageID int64 `json:"message_id"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))

		switch method {
		case "editMessageText", "deleteMessage":
			*ops = append(*ops, chunkOperation{Method: method, MessageID: body.MessageID})
		case "sendMessage":
			*ops = append(*ops, chunkOperation{Method: method, MessageID: int64(next)})
			next++
		}

		return harnessResponse(req, `{"ok":true,"result":{"message_id":500}}`), nil
	})}
}

func callMethods(calls []telegramHarnessCall) []string {
	methods := make([]string, 0, len(calls))
	for _, call := range calls {
		methods = append(methods, call.Method)
	}
	return methods
}

// When the first chunk's edit target is definitively gone, the fallback renders
// a full new set — but the still-alive surplus receipts behind the missing one
// must be deleted too, or they stay visible in the topic forever.
func TestOutputTransport_MissingFirstChunkStillDeletesSurplusReceipts(t *testing.T) {
	var ops []chunkOperation
	manager := newTelegramHarnessManager(t, &fakeController{}, new([]telegramHarnessCall))
	manager.registerTopic(42, harnessTopicID)
	manager.httpClient = missingFirstEditClient(t, &ops)
	transport := &outputTransport{manager: manager}

	result := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: "replacement",
		PreviousMessageType:       "message_replaceable",
		PreviousMessageAttributes: map[string]any{"message_ids": []any{"111", "222"}},
	}))
	require.Empty(t, result.Error)
	assert.NotEmpty(t, result.MessageIDs)
	assert.Equal(t, []chunkOperation{
		{Method: "editMessageText", MessageID: 111},
		{Method: "deleteMessage", MessageID: 222},
		{Method: "sendMessage", MessageID: 500},
	}, ops)
}

// missingFirstEditClient fails the first recorded receipt's edit with Telegram's
// definitive "message to edit not found" and succeeds everywhere else.
func missingFirstEditClient(t *testing.T, ops *[]chunkOperation) *http.Client {
	t.Helper()

	next := 500

	return &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		method := filepath.Base(req.URL.Path)

		var body struct {
			MessageID int64 `json:"message_id"`
		}
		require.NoError(t, json.NewDecoder(req.Body).Decode(&body))

		switch method {
		case "editMessageText", "deleteMessage":
			*ops = append(*ops, chunkOperation{Method: method, MessageID: body.MessageID})
		case "sendMessage":
			*ops = append(*ops, chunkOperation{Method: method, MessageID: int64(next)})
			next++
		}

		if method == "editMessageText" && body.MessageID == 111 {
			return harnessResponse(
				req,
				`{"ok":false,"error_code":400,"description":"Bad Request: message to edit not found"}`,
			), nil
		}

		return harnessResponse(req, `{"ok":true,"result":{"message_id":500}}`), nil
	})}
}

func TestMayReusePreviousReceiptsByAdjacentGenerations(t *testing.T) {
	current := int64(3)
	older := int64(2)

	tests := []struct {
		name     string
		claim    *controllerapi.OutputClaimData
		expected bool
	}{
		{
			name:     "both legacy rows keep the legacy rule",
			claim:    &controllerapi.OutputClaimData{PreviousMessageType: "message_replaceable"},
			expected: true,
		},
		{
			name: "equal generations edit",
			claim: &controllerapi.OutputClaimData{
				PreviousMessageType:          "message_replaceable",
				ModelInputGeneration:         &current,
				PreviousModelInputGeneration: &current,
			},
			expected: true,
		},
		{
			name: "changed generation sends new",
			claim: &controllerapi.OutputClaimData{
				PreviousMessageType:          "message_replaceable",
				ModelInputGeneration:         &current,
				PreviousModelInputGeneration: &older,
			},
			expected: false,
		},
		{
			name: "mixed legacy and current rows send new",
			claim: &controllerapi.OutputClaimData{
				PreviousMessageType:          "message_replaceable",
				ModelInputGeneration:         &current,
				PreviousModelInputGeneration: nil,
			},
			expected: false,
		},
		{
			name: "mixed current predecessor and legacy row sends new",
			claim: &controllerapi.OutputClaimData{
				PreviousMessageType:          "message_replaceable",
				ModelInputGeneration:         nil,
				PreviousModelInputGeneration: &older,
			},
			expected: false,
		},
		{
			name: "persistent predecessor never edits",
			claim: &controllerapi.OutputClaimData{
				PreviousMessageType:          "message_persistent",
				ModelInputGeneration:         &current,
				PreviousModelInputGeneration: &current,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.expected, mayReusePreviousReceipts(tt.claim))
		})
	}
}

func TestOutputTransport_ChangedGenerationSendsNewChunks(t *testing.T) {
	var ops []chunkOperation
	manager := newTelegramHarnessManager(t, &fakeController{}, new([]telegramHarnessCall))
	manager.registerTopic(42, harnessTopicID)
	manager.httpClient = chunkClient(t, &ops)
	transport := &outputTransport{manager: manager}

	current := int64(2)
	previous := int64(1)

	result := transport.Deliver(t.Context(), outputItem(&controllerapi.OutputClaimData{
		SessionID: 42, Type: "message_replaceable", Content: "new chain",
		PreviousMessageType:          "message_replaceable",
		PreviousMessageAttributes:    map[string]any{"message_ids": []any{"111"}},
		ModelInputGeneration:         &current,
		PreviousModelInputGeneration: &previous,
	}))
	require.Empty(t, result.Error)
	assert.Equal(t, []string{"500"}, result.MessageIDs)
	assert.Equal(t, []chunkOperation{{Method: "sendMessage", MessageID: 500}}, ops)
}
