package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (ms *messageStore) addToolResultOutput(
	ctx context.Context,
	callID, toolName, content string,
	images []llmwire.ImageRef,
	directMessages []string,
) error {
	return ms.addToolResultOutputTyped(ctx, callID, toolName, content, images, directMessages, false)
}

// Tool-result identity and invalidation remain durable even without presentation.
func (ms *messageStore) addToolResultOutputTyped(
	ctx context.Context,
	callID, toolName, content string,
	images []llmwire.ImageRef,
	directMessages []string,
	toolError bool,
) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
		ToolError:  toolError,
		Images:     images,
	}

	if ms.store == nil {
		ms.appendLocked(msg, 0)
		return nil
	}

	stored, err := storedMessage(&msg)
	if err != nil {
		return fmt.Errorf("serialize tool result: %w", err)
	}

	if ms.outputs == nil {
		directMessages = nil
	}

	ids, _, err := ms.store.InsertToolResultSetOnce(ctx, ms.sessID, []sessionstore.ToolResultEntry{
		{Message: stored, DirectMessages: directMessages},
	})
	if err != nil {
		return fmt.Errorf("persist tool result: %w", err)
	}

	if !slices.Contains(ms.rowIDs, ids[0]) {
		ms.appendLocked(msg, ids[0])
	}

	return nil
}

// toolResultCommit is one decided tool result row with its direct outputs.
type toolResultCommit struct {
	message llmwire.Message
	direct  []string
}

// commitToolResults persists the complete decided set for one assistant turn —
// result rows plus direct outputs — in a single transaction, and appends the
// set to in-memory history only after that transaction succeeded.
func (ms *messageStore) commitToolResults(ctx context.Context, commits []toolResultCommit) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()

	if len(commits) == 0 {
		return nil
	}

	if ms.store == nil {
		for _, c := range commits {
			ms.appendLocked(c.message, 0)
		}

		return nil
	}

	entries := make([]sessionstore.ToolResultEntry, len(commits))
	for i := range commits {
		m, err := storedMessage(&commits[i].message)
		if err != nil {
			return fmt.Errorf("serialize tool result %d: %w", i, err)
		}

		entries[i].Message = m
		if ms.outputs != nil {
			entries[i].DirectMessages = commits[i].direct
		}
	}

	ids, _, err := ms.store.InsertToolResultSetOnce(ctx, ms.sessID, entries)
	if err != nil {
		return fmt.Errorf("persist tool result set: %w", err)
	}

	for i := range commits {
		if !slices.Contains(ms.rowIDs, ids[i]) {
			ms.appendLocked(commits[i].message, ids[i])
		}
	}

	return nil
}

func (ms *messageStore) enqueueOutput(ctx context.Context, kind sessionstore.OutputType, content string) error {
	if ms.outputs == nil {
		return nil
	}

	_, err := ms.outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
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

	if ms.outputs == nil {
		return nil
	}

	// Only the last assistant message may be promoted; older text would
	// resurrect stale output the user must not see twice.
	for i, message := range slices.Backward(ms.messages) {
		rowID := ms.rowIDs[i]

		if message.Role != llmwire.RoleAssistant || rowID == 0 ||
			strings.TrimSpace(message.Content) == "" {
			continue
		}

		_, err := ms.outputs.EnqueueOutput(ctx, sessionstore.OutputDraft{
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
