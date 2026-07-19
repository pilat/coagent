package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

// The brief is a paraphrase; the excerpt keeps the exact text of the last turns,
// which is what a summary reliably loses (the closing diff, the stack trace).
func TestCompactionSummaryCarriesATruncatedVerbatimTail(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	longAnswer := "panic: " + strings.Repeat("z", 4000)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage("first thing"),
		compactionAssistantCall("c1", "second thing"),
		compactionToolResult("c1", "TOOL-BODY-NOT-QUOTED"),
		{Role: llmwire.RoleAssistant, Content: longAnswer},
	})

	_, err := s.compact(t.Context(), 1)
	require.NoError(t, err)

	summary := s.ms.getMessages()[2].Content

	assert.Contains(t, summary, "Verbatim tail")
	assert.Contains(t, summary, "first thing")
	assert.Contains(t, summary, "second thing")
	assert.Contains(t, summary, "panic:")
	assert.NotContains(t, summary, "TOOL-BODY-NOT-QUOTED", "tool results are not quoted")
	assert.NotContains(t, summary, longAnswer, "each quoted turn is truncated")
}

// Only the model's brief feeds the next merge — quoting stale excerpts back into
// the summarizer would have it retell yesterday's tail forever.
func TestCompactionBriefExcludesTheExcerptAndBackgroundSection(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)
	s.activeSubagentsProvider = func(context.Context) []ActiveSubagentInfo {
		return []ActiveSubagentInfo{{ChildID: 42, Blocking: false, State: "running"}}
	}

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage("QUOTED-TURN"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	_, err := s.compact(t.Context(), 1)
	require.NoError(t, err)

	summary := s.ms.getMessages()[2].Content
	assert.Contains(t, summary, "QUOTED-TURN")
	assert.Contains(t, summary, "#42 (background): running")

	assert.Equal(t, validSummary, s.compactionBrief, "the brief is the model's text and nothing else")
	assert.NotContains(t, s.compactionBrief, "QUOTED-TURN")
	assert.NotContains(t, s.compactionBrief, "#42")
}

// Without the section, a "subagent #42 completed" event arriving after the
// compaction lands where nobody remembers why #42 was started.
func TestCompactionSummaryOmitsBackgroundSectionWithoutAProvider(t *testing.T) {
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

	assert.NotContains(t, s.ms.getMessages()[2].Content, "# Active subagents")
}

// The next merge must start from a clean brief: it gets the previous brief plus
// only what happened since, never the quotes the last summary row carried.
func TestIncrementalMergeReceivesTheBriefWithoutQuotes(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage("FIRST-ROUND-TEXT"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	_, err := s.compact(t.Context(), 1)
	require.NoError(t, err)

	s.ms.mu.Lock()
	s.ms.messages = append(s.ms.messages,
		compactionUserMessage("SECOND-ROUND-TEXT"),
		compactionAssistantCall("c2", "more work"),
		compactionToolResult("c2", "result"),
	)
	s.ms.mu.Unlock()

	_, err = s.compact(t.Context(), 1)
	require.NoError(t, err)

	require.Len(t, llm.prompts, 2)
	merge := llm.prompts[1]
	assert.Contains(t, merge, "SECOND-ROUND-TEXT")
	assert.NotContains(t, merge, "Verbatim tail", "the previous summary row is not fed back in")
	assert.NotContains(t, merge, "FIRST-ROUND-TEXT")
}

func TestBuildVerbatimTailSkipsSyntheticRows(t *testing.T) {
	messages := []llmwire.Message{
		compactionUserMessage(agentsMDMessagePrefix + "project rules"),
		compactionUserMessage("real user turn"),
		{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck},
		compactionUserMessage("[AUTOMATED WARNING: empty responses]"),
		compactionUserMessage("[LOOP WARNING: low diversity]"),
		skillMessage(t, "review", "Review carefully."),
		{Role: llmwire.RoleTool, Content: "tool body", ToolCallID: "c1", ToolName: "read"},
		{Role: llmwire.RoleAssistant, Content: "real assistant turn"},
	}

	tail := buildVerbatimTail(messages)

	assert.Contains(t, tail, "real user turn")
	assert.Contains(t, tail, "real assistant turn")
	assert.NotContains(t, tail, "project rules")
	assert.NotContains(t, tail, "AUTOMATED WARNING")
	assert.NotContains(t, tail, "LOOP WARNING")
	assert.NotContains(t, tail, "Review carefully")
	assert.NotContains(t, tail, "tool body")
	assert.NotContains(t, tail, registry.PostCompactionAssistantAck)
}

func TestBuildVerbatimTailIsEmptyWithNothingQuotable(t *testing.T) {
	assert.Empty(t, buildVerbatimTail([]llmwire.Message{
		{Role: llmwire.RoleTool, Content: "body", ToolCallID: "c1", ToolName: "read"},
	}))
}
