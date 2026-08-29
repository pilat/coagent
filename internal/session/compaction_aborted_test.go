package session

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// transcriptWithAbortedCall builds a pressure-crossing transcript whose oldest
// round is followed by an assistant row aborted mid-generation: a tool call
// with no id and nothing else on the row.
func transcriptWithAbortedCall(window int) []llmwire.Message {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}

	msgs = append(msgs, roundTokens("first", 20, 900)...)

	msgs = append(msgs, llmwire.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: []llmwire.ToolCall{{ID: "", Name: "read"}},
	})

	for estimateTokens(msgs) < compactionCutoff(window)+10000 {
		msgs = append(msgs, roundTokens(fmt.Sprintf("big-%d", len(msgs)), 20, 900)...)
	}

	return msgs
}

// An aborted assistant turn is history, not a pairable call: the canonical
// head sanitizes it away and compaction succeeds around it.
func TestCompactionSucceedsAroundAnAbortedToolCall(t *testing.T) {
	const window = 32000

	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(mockLLM)
	s.ms.setMessages(transcriptWithAbortedCall(window))

	err := s.compactIfNeeded(context.Background(), window)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	messages := s.ms.getMessages()
	require.NotEmpty(t, messages)
	assert.True(t, isMarkedSummary(messages[2].Content), "the checkpoint is committed")

	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			assert.NotEmpty(t, tc.ID, "no incomplete call survives in the committed projection")
		}
	}
}
