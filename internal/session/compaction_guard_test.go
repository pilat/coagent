package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// No tail survives a cut through an open group, so an unanswered tool_use inside
// the tail would be unanswerable. The guard refuses before anything is written.
func TestCompactRefusesWhileAnExternalCallIsPending(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{"c9": tool.IDTask}

	before := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		{
			Role:      llmwire.RoleAssistant,
			Content:   "spawning",
			ToolCalls: []llmwire.ToolCall{{ID: "c9", Name: tool.IDTask}},
		},
	}
	s.ms.setMessages(before)

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount, "no summarization request may go out")
	assert.Equal(t, before, s.ms.getMessages(), "the transcript is untouched")
}

// The same guard covers ordinary tool calls of the current turn that have not
// been executed yet: compaction would erase the call the loop is about to run.
func TestCompactRefusesWhileOrdinaryToolWorkIsPending(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	before := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		compactionAssistantCall("c2", "unexecuted"),
	}
	s.ms.setMessages(before)

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount)
	assert.Equal(t, before, s.ms.getMessages())
}

// A tool_use abandoned by a later user turn has no external owner and is
// deliberately not protected — the repair policy stubs it in the head.
func TestCompactProceedsWithAnAbandonedToolCall(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "interrupted"),
		compactionUserMessage("stop, do something else"),
		compactionAssistantCall("c2", "settled work"),
		compactionToolResult("c2", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.NoError(t, err)
	assert.True(t, compacted)
}

// A header that clears the trigger on its own makes compaction an endless
// grinder: say so instead of summarizing forever.
func TestCompactRefusesWhenTheHeaderAloneExceedsTheThreshold(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32000,
	}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 120000)},
		compactionUserMessage("the task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionHeaderTooLarge)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount, "no LLM call is worth making")
}

// The system prompt rides along on every request, so a header that only fits
// without it does not fit at all.
func TestHeaderCheckCountsTheSystemPrompt(t *testing.T) {
	llm := &compactionMockLLM{contextWindow: 32000}
	s := newCompactionTestSvc(llm)

	// 20000 tokens of header: under the 27200 cutoff alone, over it with a
	// 10000-token system prompt.
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 80000)},
		compactionUserMessage("the task"),
	})

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	assert.True(t, s.headerFitsLocked(2))

	s.prompt = newPromptBuilder(strings.Repeat("s", 40000), "", "")
	assert.False(t, s.headerFitsLocked(2))
}

// The summarizer call receives the ordinary full output reserve: the complement
// of the context input fraction, not a fixed summary-length target.
func TestSummarizationRequestCarriesTheFullOutputReserve(t *testing.T) {
	const window = 200000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	_, err := s.compact(t.Context(), nil)
	require.NoError(t, err)

	assert.Equal(t, int((1-llmwire.ContextInputFraction)*float64(window)), llm.lastOptions.MaxTokens)
}

// A header carrying tool protocol fields can never pair once retained.
func TestCompactRefusesAHeaderWithToolProtocolFields(t *testing.T) {
	llm := &compactionMockLLM{contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.Error(t, err)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount)
}
