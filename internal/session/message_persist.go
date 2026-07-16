package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// addToolNotificationPairOnce appends a synthetic assistant tool_call stub plus
// its tool_result under a durable delivery identity: the store commits the
// identity and pair together, and a duplicate identity with the same payload is
// a no-op that is deliberately not appended in memory.
func (ms *messageStore) addToolNotificationPairOnce(
	ctx context.Context,
	deliveryID, callID, toolName string,
	content string,
) (bool, error) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

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

	if ms.store == nil {
		return false, errors.New("idempotent tool notification requires durable store")
	}

	toolCallsJSON, err := json.Marshal(assistant.ToolCalls)
	if err != nil {
		return false, fmt.Errorf("marshal idempotent notification tool call: %w", err)
	}

	asstStored := &sessionstore.StoredMessage{Role: assistant.Role, ToolCalls: toolCallsJSON}
	resultStored := &sessionstore.StoredMessage{
		Role:       result.Role,
		Content:    result.Content,
		ToolCallID: result.ToolCallID,
		ToolName:   result.ToolName,
	}
	fingerprint := deliveryFingerprint("tool_notification", toolName, string(args), content)

	asstID, resultID, inserted, err := ms.store.InsertToolNotificationPairOnce(
		ctx,
		ms.sessID,
		deliveryID,
		fingerprint,
		asstStored,
		resultStored,
	)
	if err != nil {
		return false, fmt.Errorf("persist idempotent tool notification pair: %w", err)
	}

	if !inserted {
		return false, nil
	}

	assistant.DBID = asstID
	result.DBID = resultID
	ms.messages = append(ms.messages, assistant, result)

	return true, nil
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

		return true, nil
	}

	storedOpening := make([]*sessionstore.StoredMessage, len(opening))
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

	for i, dbID := range ids {
		opening[i].DBID = dbID
	}

	ms.messages = opening

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

func storedMessage(msg *llmwire.Message) (*sessionstore.StoredMessage, error) {
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

	return &sessionstore.StoredMessage{
		Role:             msg.Role,
		Content:          msg.Content,
		ToolCallID:       msg.ToolCallID,
		ToolName:         msg.ToolName,
		ToolCalls:        toolCallsJSON,
		ReasoningContent: msg.ReasoningContent,
		ReasoningRaw:     msg.ReasoningRaw,
		CostUSD:          msg.CostUSD,
		Usage:            usageJSON,
	}, nil
}
