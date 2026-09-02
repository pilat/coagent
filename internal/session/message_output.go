package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

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

func (ms *messageStore) addToolResultOutput(
	ctx context.Context,
	callID, toolName, content string,
	images []llmwire.ImageRef,
	directMessages []string,
) error {
	return ms.addToolResultOutputTyped(ctx, callID, toolName, content, images, directMessages, false)
}

// addToolResultOutputTyped is the single-row legacy path kept for injections,
// settlements and block stubs; turn scheduling commits through commitToolResults.
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

	if len(directMessages) == 0 || ms.outputs == nil {
		return ms.appendMessageLocked(ctx, &msg)
	}

	stored, err := storedMessage(&msg)
	if err != nil {
		return fmt.Errorf("serialize tool result: %w", err)
	}

	id, _, err := ms.outputs.InsertToolResultWithDirectOutput(ctx, ms.sessID, stored, directMessages)
	if err != nil {
		return fmt.Errorf("persist tool result with direct output: %w", err)
	}

	ms.appendLocked(msg, id)

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

	stored := make([]*transcript.Message, len(commits))
	for i := range commits {
		m, err := storedMessage(&commits[i].message)
		if err != nil {
			return fmt.Errorf("serialize tool result %d: %w", i, err)
		}

		stored[i] = m
	}

	var (
		ids []int64
		err error
	)

	switch {
	case ms.outputs != nil:
		entries := make([]sessionstore.ToolResultEntry, len(commits))
		for i := range commits {
			entries[i] = sessionstore.ToolResultEntry{
				Message:        stored[i],
				DirectMessages: commits[i].direct,
			}
		}

		// Output commits are deliberately dropped: the outbox rows become
		// deliverable the moment this transaction commits.
		ids, _, err = ms.outputs.InsertToolResultSetOnce(ctx, ms.sessID, entries)
	default:
		ids, err = ms.store.InsertMessages(ctx, ms.sessID, stored)
	}

	if err != nil {
		return fmt.Errorf("persist tool result set: %w", err)
	}

	for i := range commits {
		ms.appendLocked(commits[i].message, ids[i])
	}

	return nil
}

func (ms *messageStore) appendAssistantOutputLocked(
	ctx context.Context,
	msg *llmwire.Message,
	outputType sessionstore.OutputType,
	output string,
) error {
	if ms.outputs == nil {
		return ms.appendMessageLocked(ctx, msg)
	}

	stored, err := storedMessage(msg)
	if err != nil {
		return fmt.Errorf("serialize assistant message: %w", err)
	}

	id, _, err := ms.outputs.InsertAssistantMessageWithOutput(ctx, ms.sessID, stored, outputType, output)
	if err != nil {
		return fmt.Errorf("persist assistant output: %w", err)
	}

	ms.appendLocked(*msg, id)

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
