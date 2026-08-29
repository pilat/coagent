package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestCompactLeavesTranscriptsWithNothingToSummarize(t *testing.T) {
	summary := llmwire.Message{Role: llmwire.RoleUser, Content: renderMarkedSummary("old checkpoint", "")}

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
			name: "previous summary plus its reattachment are the last messages",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
				skillMessage(t, "review", "Review carefully."),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
			s := newCompactionTestSvc(llm)
			s.ms.setMessages(tc.messages)

			compacted, err := s.compact(t.Context(), nil)

			require.NoError(t, err)
			assert.False(t, compacted, "nothing to compact")
			assert.Zero(t, llm.callCount)
			assert.Equal(t, tc.messages, s.ms.getMessages())
		})
	}
}

// A second compaction immediately after one has no raw delta: the previous
// summary and its reattachment scaffolding are the only rows present.
func TestSecondCompactionRightAfterOneFindsNothing(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		skillMessage(t, "review", "Review carefully."),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 1, llm.callCount)
	require.Len(t, renderedSkills(s.ms.getMessages()), 1, "the skill was reattached")

	compacted, err = s.compact(t.Context(), nil)

	require.NoError(t, err)
	assert.False(t, compacted, "nothing but the previous compaction's own output is present")
	assert.Equal(t, 1, llm.callCount, "no second summarization request")
}

// A repeated checkpoint feeds the previous summary as the anchor and only the
// delta as HISTORY TO SUMMARIZE.
func TestSecondCheckpointAnchorsOnThePreviousSummaryAndSendsOnlyTheDelta(t *testing.T) {
	window := 1 << 20

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "MIDDLE-WORK"),
		compactionToolResult("c1", "middle result"),
		compactionAssistantCall("c2", "recent work"),
		compactionToolResult("c2", "recent result"),
	})

	_, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, llm.callCount)

	s.ms.mu.Lock()
	s.ms.messages = append(s.ms.messages,
		compactionAssistantCall("c3", "NEWLY-AGED"),
		compactionToolResult("c3", "new result"),
	)
	s.ms.mu.Unlock()

	compacted, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 2, llm.callCount)

	prompt := llm.prompts[1]
	assert.Contains(t, prompt, summarizePrevSection, "the previous marked summary is the anchor")
	assert.Contains(t, prompt, validSummary, "the extracted model text anchors, not the wrapper")
	assert.Contains(t, prompt, "NEWLY-AGED", "the delta is the raw group that left the tail")
}

func TestParseCheckpointPrefix(t *testing.T) {
	t.Run("no previous checkpoint", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
		}

		cp := parseCheckpointPrefix(msgs, 2)
		assert.Equal(t, -1, cp.summaryRowIdx)
		assert.Equal(t, 2, cp.rawStart)
	})

	t.Run("marked summary directly after the header", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			compactionUserMessage(renderMarkedSummary("anchor text", "")),
			compactionAssistantCall("c1", "raw work"),
		}

		cp := parseCheckpointPrefix(msgs, 2)
		assert.Equal(t, 2, cp.summaryRowIdx)
		assert.Equal(t, "anchor text", cp.prevSummary)
		assert.Equal(t, 3, cp.rawStart)
		assert.Equal(t, -1, cp.skillRowIdx)
	})

	t.Run("reattachment row is scaffolding", func(t *testing.T) {
		rendered := skillMessage(t, "review", "Review carefully.")
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			compactionUserMessage(renderMarkedSummary("anchor", "")),
			compactionUserMessage(rendered.Content),
			compactionAssistantCall("c1", "raw work"),
		}

		cp := parseCheckpointPrefix(msgs, 2)
		assert.Equal(t, 2, cp.summaryRowIdx)
		assert.Equal(t, 3, cp.skillRowIdx)
		assert.Equal(t, 4, cp.rawStart)
	})

	t.Run("an unclosed wrapper is ordinary history", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			compactionUserMessage(compactionMarkOpen + "\n\nno closing marker"),
			compactionAssistantCall("c1", "work"),
		}

		cp := parseCheckpointPrefix(msgs, 2)
		assert.Equal(t, -1, cp.summaryRowIdx)
		assert.Equal(t, 2, cp.rawStart)
	})

	t.Run("a summary not immediately after the header is raw", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			compactionUserMessage("steering turn"),
			compactionUserMessage(renderMarkedSummary("anchor", "")),
			compactionAssistantCall("c1", "work"),
		}

		cp := parseCheckpointPrefix(msgs, 2)
		assert.Equal(t, -1, cp.summaryRowIdx)
	})
}

func TestMarkedSummaryRoundTrips(t *testing.T) {
	background := "\n\n# Active subagents\n- #42 (background): running\n"

	content := renderMarkedSummary("model text", background)
	assert.True(t, isMarkedSummary(content))

	modelText, bg, ok := parseMarkedSummary(content)
	require.True(t, ok)
	assert.Equal(t, "model text", modelText)
	assert.Equal(t, strings.TrimRight(background, "\n"), bg)
	assert.True(t, isMarkedSummary(content))

	content = renderMarkedSummary("model text", "")
	modelText, bg, ok = parseMarkedSummary(content)
	require.True(t, ok)
	assert.Equal(t, "model text", modelText)
	assert.Empty(t, bg)

	_, _, ok = parseMarkedSummary("not a summary at all")
	assert.False(t, ok)
}
