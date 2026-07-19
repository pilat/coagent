package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/registry"
)

const testPrimer = "2020-01-01T00:00:00Z"

func TestCompactLeavesTranscriptsWithNothingToSummarize(t *testing.T) {
	summary := llmwire.Message{Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed]\n\nold"}
	ack := llmwire.Message{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck}

	tests := []struct {
		name     string
		messages []llmwire.Message
	}{
		{
			name:     "header only",
			messages: []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}, compactionUserMessage("task")},
		},
		{
			name: "previous summary is the last message",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
			},
		},
		{
			name: "previous ack is the last message",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
				ack,
			},
		},
		{
			// A second /compact straight after one: summary, ack, primer and the
			// skill envelopes it reattached are all its own output.
			name: "everything present is the previous compaction's own output",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
				ack,
				compactionUserMessage(fmt.Sprintf(registry.PostCompactionPrimer, testPrimer)),
				skillMessage(t, "review", "Review carefully."),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
			s := newCompactionTestSvc(llm)
			s.ms.setMessages(tc.messages)

			compacted, err := s.compact(t.Context(), 1)

			require.NoError(t, err)
			assert.False(t, compacted)
			assert.Zero(t, llm.callCount)
			assert.Equal(t, tc.messages, s.ms.getMessages())
		})
	}
}

func TestCompactSummarizesOnlyPastThePreviousCompactionHeader(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.compactionBrief = "old brief"
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage("[CONTEXT SUMMARY - previous work condensed]\n\nold brief"),
		{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck},
		compactionUserMessage(fmt.Sprintf(registry.PostCompactionPrimer, testPrimer)),
		compactionAssistantCall("c1", "MIDDLE-WORK"),
		compactionToolResult("c1", "middle result"),
		compactionAssistantCall("c2", "recent work"),
		compactionToolResult("c2", "recent result"),
	})

	compacted, err := s.compact(t.Context(), 1)

	require.NoError(t, err)
	assert.True(t, compacted)

	require.Len(t, llm.prompts, 1)
	prompt := llm.prompts[0]
	assert.Contains(t, prompt, "EXISTING BRIEF:")
	assert.Contains(t, prompt, "MIDDLE-WORK")
	assert.NotContains(t, prompt, "[CONTEXT SUMMARY")
	assert.NotContains(t, prompt, registry.PostCompactionAssistantAck)
	assert.NotContains(t, prompt, "[Post-compaction")
}

func TestCompactSkipsOnlyTheHeaderMessagesThatArePresent(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.compactionBrief = "old brief"
	// A summary without the ack and primer that normally follow it: the skips are
	// conditional, so nothing after the summary may be swallowed.
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage("[CONTEXT SUMMARY - previous work condensed]\n\nold brief"),
		compactionAssistantCall("c1", "MIDDLE-WORK"),
		compactionToolResult("c1", "middle result"),
		compactionAssistantCall("c2", "recent work"),
		compactionToolResult("c2", "recent result"),
	})

	compacted, err := s.compact(t.Context(), 1)

	require.NoError(t, err)
	assert.True(t, compacted)

	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "MIDDLE-WORK")
	assert.NotContains(t, llm.prompts[0], "[CONTEXT SUMMARY")
}

func TestCompactReportsSummarizedCountAndBriefPersistFailure(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	ctx := logger.ToContext(t.Context(), zap.New(core))

	store := &compactionRecordingStore{nextID: 1, updateBriefErr: errors.New("store unavailable")}
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)

	for _, message := range []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "one"),
		compactionToolResult("c1", "one result"),
		compactionAssistantCall("c2", "two"),
		compactionToolResult("c2", "two result"),
		compactionAssistantCall("c3", "three"),
		compactionToolResult("c3", "three result"),
	} {
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}

	compacted, err := s.compact(ctx, 1)

	require.NoError(t, err)
	assert.True(t, compacted)
	assert.Len(t, logs.FilterMessage("persist_compaction_brief_failed").All(), 1)

	completed := logs.FilterMessage("compaction_completed").All()
	require.Len(t, completed, 1)
	// Only the header stays out — no tail is retained any more.
	assert.Equal(t, int64(6), completed[0].ContextMap()["summarized"])
}

func compactionUserMessage(content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleUser, Content: content}
}

func compactionAssistantCall(id, content string) llmwire.Message {
	return llmwire.Message{
		Role:      llmwire.RoleAssistant,
		Content:   content,
		ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
	}
}

func compactionToolResult(id, content string) llmwire.Message {
	return llmwire.Message{Role: llmwire.RoleTool, Content: content, ToolCallID: id, ToolName: "read"}
}
