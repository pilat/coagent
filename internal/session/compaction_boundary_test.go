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
			setTestMessages(s, tc.messages)

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

	setTestMessages(s, []llmwire.Message{
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

// A repeated checkpoint replays the then-current prefix from the beginning:
// the complete previous marked summary and the former-tail rows that have aged
// above the split ride along; there is no separate anchor or delta request.
func TestSecondCheckpointReplaysTheNativePrefixFromTheStart(t *testing.T) {
	window := 1 << 20

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, []llmwire.Message{
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

	appendMessages(t, s, compactionAssistantCall("c3", "NEWLY-AGED"), compactionToolResult("c3", "new result"))

	compacted, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 2, llm.callCount)

	// The second call is a fresh projection of the whole current transcript
	// (loaded rows plus the instruction): prior summary included, newest group
	// still in the tail.
	input := llm.lastMessages
	require.NotEmpty(t, input)

	assert.True(t, isMarkedSummary(input[2].Content), "the previous marked summary is replayed as its row")
	assert.Contains(t, input[2].Content, validSummary)
	assert.Contains(t, transcriptText(input), "recent work", "former-tail rows above the split ride along")
	assert.NotContains(t, transcriptText(input), "NEWLY-AGED", "the newest group stays verbatim in the non-empty tail")

	for _, m := range input {
		assert.NotContains(t, m.Content, "HISTORY TO SUMMARIZE", "no canonical JSONL sections remain")
		assert.NotContains(t, m.Content, "PREVIOUS SUMMARY")
	}

	last := input[len(input)-1]
	assert.Equal(t, llmwire.RoleUser, last.Role)
	assert.Contains(t, last.Content, "continuation checkpoint", "the instruction is final")
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
	background := "\n\n# Active background work\n- #42 (background): running\n"

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

func TestMarkedSummaryReadsLegacyActiveSubagentsSection(t *testing.T) {
	background := "\n\n# Active subagents\n- #42 (background): running\n"
	modelText, got, ok := parseMarkedSummary(renderMarkedSummary("model text", background))
	require.True(t, ok)
	assert.Equal(t, "model text", modelText)
	assert.Equal(t, strings.TrimRight(background, "\n"), got)
}
