package telegram

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

type switchingTelegramClient struct {
	delayedTelegramClient
	work int64
}

func TestHarnessScenario_AssistantSwitchDeliversHeaderThenWorkTail(t *testing.T) {
	client := &switchingTelegramClient{}
	h := newDelayedTelegramHarnessWithClient(t, func(*config.Config) (llm.Client, error) { return client, nil })
	client.work = createChatWork(t, h)
	require.NoError(t, h.service.Start(t.Context()))
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	awaitChatTail(t, h, manager.assistantRootID)
	enqueueChatMessage(t, h, client.work, "work tail after assistant switch")
	manager.processUpdate(t.Context(), assistantTelegramUpdate("connect me to the work session"))
	tail := fmt.Sprintf("#%d project\nwork tail after assistant switch", client.work)
	require.Eventually(t, func() bool {
		return hasTelegramMessage(h.recorder.snapshot(), tail)
	}, 10*time.Second, 10*time.Millisecond)
	header := fmt.Sprintf("→ #%d project", client.work)
	seenHeader, seenTail := false, false
	for _, call := range h.recorder.snapshot() {
		if call.Method != "sendMessage" {
			continue
		}
		if call.Text == header {
			assert.False(t, seenHeader, "one header per switch")
			seenHeader = true
		}
		if call.Text == tail {
			assert.True(t, seenHeader, "the header precedes the work tail")
			assert.False(t, seenTail, "the tail is delivered once")
			seenTail = true
		}
	}
	assert.True(t, seenHeader)
	assert.True(t, seenTail)
}

func (c *switchingTelegramClient) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	options ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	if !strings.HasPrefix(system, registry.AssistantPrompt) {
		return c.delayedTelegramClient.Chat(ctx, system, messages, tools, options...)
	}
	for _, message := range messages {
		if message.Role == "tool" && message.ToolName == "switch" {
			return &llmwire.Response{Text: "Connected.", FinishType: llmwire.FinishStop}, nil
		}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: "switch-work", Name: "switch", Arguments: []byte(fmt.Sprintf(`{"session":%d}`, c.work)),
	}}}, nil
}
