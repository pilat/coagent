package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

// addScheduledToolNotificationPairOnce commits one scheduled turn and its
// manager announcement under the producer's durable identity.
func (ms *messageStore) addScheduledToolNotificationPairOnce(
	ctx context.Context,
	deliveryID, callID, toolName string,
	content string,
) (bool, error) {
	args := json.RawMessage("{}")

	assistant := llmwire.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: callID, Name: toolName, Arguments: args}},
	}
	result := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
	}

	toolCallsJSON, err := json.Marshal(assistant.ToolCalls)
	if err != nil {
		return false, fmt.Errorf("marshal idempotent notification tool call: %w", err)
	}

	return ms.persistStoredToolNotificationPairOnce(ctx, deliveryID, []*transcript.Message{
		{Role: assistant.Role, ToolCalls: toolCallsJSON},
		{
			Role:       result.Role,
			Content:    result.Content,
			ToolCallID: result.ToolCallID,
			ToolName:   result.ToolName,
		},
	})
}

func (ms *messageStore) persistStoredToolNotificationPairOnce(
	ctx context.Context,
	deliveryID string,
	pair []*transcript.Message,
) (bool, error) {
	if len(pair) != 2 {
		return false, fmt.Errorf("idempotent notification requires a pair, got %d messages", len(pair))
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.store == nil {
		return false, errors.New("idempotent tool notification requires durable store")
	}

	assistant, result := pair[0], pair[1]

	calls, err := decodeStoredToolCalls(assistant.ToolCalls)
	if err != nil {
		return false, err
	}

	if len(calls) != 1 {
		return false, fmt.Errorf("idempotent notification requires one tool call, got %d", len(calls))
	}

	args, err := json.Marshal(calls[0].Arguments)
	if err != nil {
		return false, fmt.Errorf("marshal notification arguments: %w", err)
	}

	fingerprint := deliveryFingerprint(
		"tool_notification", calls[0].Name, string(args), result.Content,
	)

	asstID, resultID, inserted, err := ms.store.InsertScheduledToolNotificationPairOnce(
		ctx, ms.sessID, deliveryID, fingerprint, assistant, result,
	)
	if err != nil {
		return false, fmt.Errorf("persist idempotent tool notification pair: %w", err)
	}

	if !inserted {
		return false, nil
	}

	ms.appendLocked(llmwire.Message{
		Role:      assistant.Role,
		ToolCalls: calls,
	}, asstID)
	ms.appendLocked(llmwire.Message{
		Role:       result.Role,
		Content:    result.Content,
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
	}, resultID)

	return true, nil
}

// decodeStoredToolCalls parses the persisted tool-call JSON back into wire
// calls for the in-memory projection.
func decodeStoredToolCalls(data json.RawMessage) ([]llmwire.ToolCall, error) {
	if len(data) == 0 {
		return nil, nil
	}

	var calls []llmwire.ToolCall
	if err := json.Unmarshal(data, &calls); err != nil {
		return nil, fmt.Errorf("decode stored tool calls: %w", err)
	}

	return calls, nil
}

// resetToOnce reopens the conversation with opening under a durable delivery
// identity, hiding the old rows via compacted_at (append-only — never deleted).
// Hiding is the LAST durable step: an earlier failure leaves the old transcript
// visible instead of emptying the session.
func (ms *messageStore) resetToOnce(
	ctx context.Context,
	deliveryID, fingerprint string,
	opening []llmwire.Message,
) (bool, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if ms.store == nil {
		ms.messages = opening
		ms.rowIDs = make([]int64, len(opening))

		return true, nil
	}

	storedOpening := make([]*transcript.Message, len(opening))
	for i := range opening {
		stored, err := storedMessage(&opening[i])
		if err != nil {
			return false, fmt.Errorf("serialize opening message: %w", err)
		}

		storedOpening[i] = stored
	}

	ids, inserted, err := ms.store.ResetSessionContextOnce(
		ctx,
		ms.sessID,
		deliveryID,
		fingerprint,
		storedOpening,
	)
	if err != nil {
		return false, fmt.Errorf("reset session context: %w", err)
	}

	if !inserted {
		return false, nil
	}

	if len(ids) != len(opening) {
		return false, fmt.Errorf("reset returned %d message ids for %d opening messages", len(ids), len(opening))
	}

	ms.messages = opening

	ms.rowIDs = append([]int64(nil), ids...)

	return true, nil
}

func deliveryFingerprint(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(part))
	}

	return hex.EncodeToString(h.Sum(nil))
}

func storedMessage(msg *llmwire.Message) (*transcript.Message, error) {
	var toolCallsJSON json.RawMessage

	if len(msg.ToolCalls) > 0 {
		data, err := json.Marshal(msg.ToolCalls)
		if err != nil {
			return nil, fmt.Errorf("marshal tool calls: %w", err)
		}

		toolCallsJSON = data
	}

	var usageJSON json.RawMessage

	if msg.Usage != nil {
		data, err := json.Marshal(msg.Usage)
		if err != nil {
			return nil, fmt.Errorf("marshal usage: %w", err)
		}

		usageJSON = data
	}

	var attachmentsJSON json.RawMessage

	// Images are valid on user/tool rows only (D2); any other role drops them
	// so they can neither render nor inflate the token estimate.
	if len(msg.Images) > 0 && (msg.Role == llmwire.RoleUser || msg.Role == llmwire.RoleTool) {
		data, err := json.Marshal(msg.Images)
		if err != nil {
			return nil, fmt.Errorf("marshal attachments: %w", err)
		}

		attachmentsJSON = data
	}

	return &transcript.Message{
		Role:             msg.Role,
		Content:          msg.Content,
		ToolCallID:       msg.ToolCallID,
		ToolName:         msg.ToolName,
		ToolError:        msg.ToolError,
		ToolCalls:        toolCallsJSON,
		ReasoningContent: msg.ReasoningContent,
		ReasoningRaw:     msg.ReasoningRaw,
		Attachments:      attachmentsJSON,
		CostUSD:          msg.CostUSD,
		Usage:            usageJSON,
	}, nil
}
