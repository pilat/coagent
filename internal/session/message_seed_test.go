package session

import (
	"context"

	"github.com/pilat/coagent/internal/llmwire"
)

func (ms *messageStore) addAssistantMessage(ctx context.Context, response *llmwire.Response) error {
	ms.mu.Lock()
	defer ms.mu.Unlock()
	message := llmwire.Message{
		Role: llmwire.RoleAssistant, Content: response.Text, ToolCalls: response.ToolCalls,
		ReasoningContent: response.ReasoningContent, ReasoningRaw: response.ReasoningRaw,
		CostUSD: response.CostUSD, Usage: response.Usage, FinishType: response.FinishType,
		ProviderFinishReason: response.ProviderFinishReason,
	}
	return ms.appendMessageLocked(ctx, &message)
}
