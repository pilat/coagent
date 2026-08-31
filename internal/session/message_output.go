package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
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
	ms.mu.Lock()
	defer ms.mu.Unlock()

	msg := llmwire.Message{
		Role:       llmwire.RoleTool,
		Content:    content,
		ToolCallID: callID,
		ToolName:   toolName,
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
