package session

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// messageStore manages the agent's conversation message history with optional persistence.
type messageStore struct {
	mu       sync.Mutex
	messages []llmwire.Message
	rowIDs   []int64
	store    sessionstore.RuntimeStore // nil = in-memory only (tests without persistence)
	sessID   int64                     // session ID for persistence
}

func newMessageStore(store sessionstore.RuntimeStore, sessID int64) *messageStore {
	return &messageStore{
		messages: make([]llmwire.Message, 0),
		rowIDs:   make([]int64, 0),
		store:    store,
		sessID:   sessID,
	}
}

func (ms *messageStore) addUserMessage(ctx context.Context, content string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	msg := llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: content,
	}

	return ms.appendMessageLocked(ctx, &msg)
}

func (ms *messageStore) addAssistantMessage(ctx context.Context, resp *llmwire.Response) error {
	return ms.addAssistantMessageOutput(ctx, resp, "", "")
}

func (ms *messageStore) addAssistantMessageOutput(
	ctx context.Context,
	resp *llmwire.Response,
	outputType sessionstore.OutputType,
	output string,
) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	msg := llmwire.Message{
		Role:             llmwire.RoleAssistant,
		Content:          resp.Text,
		ToolCalls:        resp.ToolCalls,
		ReasoningContent: resp.ReasoningContent,
		ReasoningRaw:     resp.ReasoningRaw,
		CostUSD:          resp.CostUSD,
		Usage:            resp.Usage,
	}

	if outputType == "" {
		return ms.appendMessageLocked(ctx, &msg)
	}

	return ms.appendAssistantOutputLocked(ctx, &msg, outputType, output)
}

func (ms *messageStore) addToolResult(ctx context.Context, callID, toolName, content string) error {
	return ms.addToolResultOutput(ctx, callID, toolName, content, nil, nil)
}

func (ms *messageStore) addToolResultOutput(
	ctx context.Context,
	callID, toolName, content string,
	images []llmwire.ImageRef,
	directMessages []string,
) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
		Images:     images,
	}

	if len(directMessages) == 0 {
		return ms.appendMessageLocked(ctx, &msg)
	}

	directStore, ok := ms.store.(sessionstore.DirectOutputStore)
	if !ok {
		return ms.appendMessageLocked(ctx, &msg)
	}

	stored, err := storedMessage(&msg)
	if err != nil {
		return fmt.Errorf("serialize tool result: %w", err)
	}

	id, _, err := directStore.InsertToolResultWithDirectOutput(ctx, ms.sessID, stored, directMessages)
	if err != nil {
		return fmt.Errorf("persist tool result with direct output: %w", err)
	}

	ms.appendLocked(msg, id)

	return nil
}

// appendMessageLocked persists a message and only then appends it in memory, so
// the agent never reasons over a turn the store rejected. Caller must hold ms.mu.
func (ms *messageStore) appendMessageLocked(ctx context.Context, msg *llmwire.Message) error {
	if ms.store == nil {
		ms.appendLocked(*msg, 0)

		return nil
	}

	stored, err := storedMessage(msg)
	if err != nil {
		return fmt.Errorf("serialize message: %w", err)
	}

	dbID, err := ms.store.InsertMessage(ctx, ms.sessID, stored)
	if err != nil {
		return fmt.Errorf("persist %s message: %w", msg.Role, err)
	}

	ms.appendLocked(*msg, dbID)

	return nil
}

func (ms *messageStore) appendAssistantOutputLocked(
	ctx context.Context,
	msg *llmwire.Message,
	outputType sessionstore.OutputType,
	output string,
) error {
	outputStore, ok := ms.store.(sessionstore.AssistantOutputStore)
	if !ok {
		return ms.appendMessageLocked(ctx, msg)
	}

	stored, err := storedMessage(msg)
	if err != nil {
		return fmt.Errorf("serialize assistant message: %w", err)
	}

	id, _, err := outputStore.InsertAssistantMessageWithOutput(ctx, ms.sessID, stored, outputType, output)
	if err != nil {
		return fmt.Errorf("persist assistant output: %w", err)
	}

	ms.appendLocked(*msg, id)

	return nil
}

func (ms *messageStore) enqueueOutput(ctx context.Context, kind sessionstore.OutputType, content string) error {
	outputStore, ok := ms.store.(sessionstore.OutputStore)
	if !ok {
		return nil
	}

	_, err := outputStore.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: ms.sessID,
		Type:      kind,
		Content:   content,
	})
	if err != nil {
		return fmt.Errorf("enqueue session output: %w", err)
	}

	return nil
}

func (ms *messageStore) enqueueFinalAssistantOutput(ctx context.Context, content string) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	outputStore, ok := ms.store.(sessionstore.OutputStore)
	if !ok {
		return nil
	}

	// Only the last assistant message may be promoted: an already-terminal
	// answer replays as a no-op under its own key, while an older turn's text
	// would resurrect stale output the user must not see twice.
	for i, message := range slices.Backward(ms.messages) {
		rowID := ms.rowIDs[i]

		if message.Role != llmwire.RoleAssistant || rowID == 0 ||
			strings.TrimSpace(message.Content) == "" {
			continue
		}

		_, err := outputStore.EnqueueOutput(ctx, sessionstore.OutputDraft{
			SessionID: ms.sessID, Type: sessionstore.OutputMessagePersistent, Content: content,
			SourceKey: fmt.Sprintf("message:%d:final", rowID),
			Fingerprint: sessionstore.OutputFingerprintWithRelease(
				sessionstore.OutputMessagePersistent, content, ms.sessID, nil, true,
			),
			ReleasesInput: true,
		})
		if err != nil {
			return fmt.Errorf("enqueue final assistant output: %w", err)
		}

		return nil
	}

	return nil
}

func (ms *messageStore) appendLocked(message llmwire.Message, rowID int64) {
	ms.messages = append(ms.messages, message)
	ms.rowIDs = append(ms.rowIDs, rowID)
}

func (ms *messageStore) replaceCompactedMessagesLocked(
	ctx context.Context,
	compactedIDs []int64,
	messages []llmwire.Message,
	rowIDs []int64,
) error {
	if ms.store == nil {
		return nil
	}

	entries, err := compactionEntries(messages, rowIDs)
	if err != nil {
		return err
	}

	ids, err := ms.store.ReplaceCompactedMessages(ctx, ms.sessID, compactedIDs, entries)
	if err != nil {
		return fmt.Errorf("replace compacted messages: %w", err)
	}

	if len(ids) != len(messages) {
		return fmt.Errorf("replacement returned %d IDs for %d messages", len(ids), len(messages))
	}

	copy(rowIDs, ids)

	return nil
}

// completeCompactionCommandLocked commits the compaction inside the durable
// /compact command transaction so its inbox settlement cannot outlive it.
func (ms *messageStore) completeCompactionCommandLocked(
	ctx context.Context,
	input PendingInput,
	compactedIDs []int64,
	messages []llmwire.Message,
	rowIDs []int64,
) error {
	commandStore, ok := ms.store.(sessionstore.CompactionCommandStore)
	if !ok {
		return ms.replaceCompactedMessagesLocked(ctx, compactedIDs, messages, rowIDs)
	}

	entries, err := compactionEntries(messages, rowIDs)
	if err != nil {
		return err
	}

	ids, _, err := commandStore.CompleteCompactionInput(
		ctx, input.ID, ms.sessID, compactedIDs, entries, "✅ Context compacted",
	)
	if err != nil {
		return fmt.Errorf("commit compact command: %w", err)
	}

	if len(ids) != len(messages) {
		return fmt.Errorf("compaction command returned %d ids for %d messages", len(ids), len(messages))
	}

	copy(rowIDs, ids)

	return nil
}

func compactionEntries(messages []llmwire.Message, rowIDs []int64) ([]sessionstore.CompactionEntry, error) {
	if len(messages) != len(rowIDs) {
		return nil, fmt.Errorf("serialize %d compaction messages with %d row ids", len(messages), len(rowIDs))
	}

	entries := make([]sessionstore.CompactionEntry, len(messages))
	for i := range messages {
		if rowIDs[i] != 0 {
			entries[i].ExistingID = rowIDs[i]

			continue
		}

		message, err := storedMessage(&messages[i])
		if err != nil {
			return nil, fmt.Errorf("serialize compaction message %d: %w", i, err)
		}

		entries[i].Message = message
	}

	return entries, nil
}

func (ms *messageStore) setMessages(msgs []llmwire.Message) {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	ms.messages = msgs
	ms.rowIDs = make([]int64, len(msgs))
}

func (ms *messageStore) getMessages() []llmwire.Message {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	result := make([]llmwire.Message, len(ms.messages))
	copy(result, ms.messages)

	return result
}

func (ms *messageStore) getRowIDs() []int64 {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	return append([]int64(nil), ms.rowIDs...)
}

// reloadMessages replaces in-memory messages with active messages from the store.
// No-op when store is nil.
func (ms *messageStore) reloadMessages(ctx context.Context) error {
	if ms.store == nil {
		return nil
	}

	ms.mu.Lock()
	defer ms.mu.Unlock()

	stored, err := ms.store.LoadActiveMessages(ctx, ms.sessID)
	if err != nil {
		return fmt.Errorf("reload messages: %w", err)
	}

	messages := make([]llmwire.Message, len(stored))
	rowIDs := make([]int64, len(stored))

	for i, sm := range stored {
		msg := llmwire.Message{
			Role:             sm.Role,
			Content:          sm.Content,
			ToolCallID:       sm.ToolCallID,
			ToolName:         sm.ToolName,
			ReasoningContent: sm.ReasoningContent,
			ReasoningRaw:     sm.ReasoningRaw,
			CostUSD:          sm.CostUSD,
		}

		if len(sm.ToolCalls) > 0 {
			if err := json.Unmarshal(sm.ToolCalls, &msg.ToolCalls); err != nil {
				return fmt.Errorf("unmarshal tool calls for message %d: %w", sm.ID, err)
			}
		}

		if len(sm.Attachments) > 0 {
			if err := json.Unmarshal(sm.Attachments, &msg.Images); err != nil {
				return fmt.Errorf("unmarshal attachments for message %d: %w", sm.ID, err)
			}
		}

		if len(sm.Usage) > 0 {
			var usage llmwire.MessageUsage
			if err := json.Unmarshal(sm.Usage, &usage); err != nil {
				return fmt.Errorf("unmarshal usage for message %d: %w", sm.ID, err)
			}

			msg.Usage = &usage
		}

		messages[i] = msg
		rowIDs[i] = sm.ID
	}

	ms.messages = messages
	ms.rowIDs = rowIDs

	return nil
}
