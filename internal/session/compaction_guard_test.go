package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// No tail survives compaction, so an unanswered tool_use inside it would be
// deleted while its external producer still holds the call id. The guard refuses
// before anything is written.
func TestCompactRefusesWhileAnExternalCallIsPending(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
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

	compacted, err := s.compact(t.Context(), 1)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount, "no summarization request may go out")
	assert.Equal(t, before, s.ms.getMessages(), "the transcript is untouched")
}

// The same guard covers ordinary tool calls of the current turn that have not
// been executed yet: compaction would erase the call the loop is about to run.
func TestCompactRefusesWhileOrdinaryToolWorkIsPending(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	before := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		compactionAssistantCall("c2", "unexecuted"),
	}
	s.ms.setMessages(before)

	compacted, err := s.compact(t.Context(), 1)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount)
	assert.Equal(t, before, s.ms.getMessages())
}

// A tool_use abandoned by a later user turn has no external owner and is
// deliberately not protected — the rebuild destroys it along with everything else.
func TestCompactProceedsWithAnAbandonedToolCall(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "interrupted"),
		compactionUserMessage("stop, do something else"),
	})

	compacted, err := s.compact(t.Context(), 1)

	require.NoError(t, err)
	assert.True(t, compacted)
	assert.Len(t, s.ms.getMessages(), 5)
}

// A header that clears the trigger on its own makes compaction an endless
// grinder: say so instead of summarizing forever.
func TestCompactRefusesWhenTheHeaderAloneExceedsTheThreshold(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 32000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 120000)},
		compactionUserMessage("the task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), 1)

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

// Compacting twice in a row must recognise its own output — summary, ack, primer
// and reattachments — and answer "nothing to compact" without paying for a call.
func TestSecondCompactionRightAfterOneFindsNothing(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		skillMessage(t, "review", "Review carefully."),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), 1)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 1, llm.callCount)
	require.Len(t, renderedSkills(s.ms.getMessages()), 1, "the skill was reattached")

	compacted, err = s.compact(t.Context(), 1)

	require.NoError(t, err)
	assert.False(t, compacted, "nothing but the previous compaction's own output is present")
	assert.Equal(t, 1, llm.callCount, "no second summarization request")
}

func TestSummarizeStartAfterSkipsTheWholePostCompactionPreamble(t *testing.T) {
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage(compactionSummaryPrefix + " - previous work condensed]\n\nbrief"),
		{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck},
		compactionUserMessage(compactionPrimerPrefix + " context refresh]"),
		skillMessage(t, "one", "a"),
		skillMessage(t, "two", "b"),
		compactionAssistantCall("c1", "new work"),
	}

	assert.Equal(t, 7, summarizeStartAfter(messages, 2))
	assert.Equal(t, 2, summarizeStartAfter(messages[:2], 2), "no previous compaction: start at the header")
}

// The summarization request carries its own max_tokens cap, so the brief cannot
// outgrow the room the window reserved for it — first brief and every later
// merge alike.
func TestSummarizationRequestCarriesTheOutputCap(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	_, err := s.compact(t.Context(), 1)
	require.NoError(t, err)

	assert.Equal(t, compactionOutputReserve, llm.lastOptions.MaxTokens)
}
