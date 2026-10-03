package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

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

func TestRawCutLegality(t *testing.T) {
	tests := []struct {
		name  string
		tail  []llmwire.Message
		legal bool
	}{
		{name: "empty tail", tail: nil, legal: true},
		{
			name:  "complete group",
			tail:  []llmwire.Message{compactionAssistantCall("c1", "work"), compactionToolResult("c1", "result")},
			legal: true,
		},
		{
			name: "a result stored later than its call never cuts between them",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "call"),
				compactionUserMessage("interruption"),
				compactionToolResult("c1", "late result"),
			},
			legal: false,
		},
		{
			name: "duplicate call ids across assistant rows are illegal raw",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "one"),
				compactionToolResult("c1", "one result"),
				compactionAssistantCall("c1", "two"),
				compactionToolResult("c1", "two result"),
			},
			legal: false,
		},
		{
			name: "abandoned call before a newer user row must stay in the head",
			tail: []llmwire.Message{
				compactionAssistantCall("c1", "abandoned"),
				compactionUserMessage("new instruction"),
			},
			legal: false,
		},
		{
			name:  "legacy tool rows with no id are singleton groups",
			tail:  []llmwire.Message{{Role: llmwire.RoleTool, Content: "legacy", ToolName: "read"}},
			legal: true,
		},
		{
			name:  "orphaned result needs repair",
			tail:  []llmwire.Message{compactionToolResult("c1", "orphan")},
			legal: false,
		},
		{
			name:  "missing result needs repair",
			tail:  []llmwire.Message{compactionAssistantCall("c1", "unresolved")},
			legal: false,
		},
		{
			name: "one assistant row plus all parallel results is indivisible",
			tail: []llmwire.Message{
				{
					Role:      llmwire.RoleAssistant,
					ToolCalls: []llmwire.ToolCall{{ID: "p1", Name: "read"}, {ID: "p2", Name: "read"}},
				},
				compactionToolResult("p1", "one"),
			},
			legal: false,
		},
		{
			name: "a boundary between two complete parallel groups is legal",
			tail: []llmwire.Message{
				compactionAssistantCall("p1", "one"),
				compactionToolResult("p1", "one"),
				compactionAssistantCall("p2", "two"),
				compactionToolResult("p2", "two"),
			},
			legal: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.legal, rawCutLegal(tc.tail))
		})
	}
}

// The latest successful skill invocation is the current skill: failed calls and
// batch-shaped output never qualify, and the latest candidate wins.
func TestSelectCurrentSkill(t *testing.T) {
	rendered := skillMessage(t, "review", "Review carefully.")

	t.Run("stamped user row", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			{Role: llmwire.RoleUser, Content: "[stamp] " + rendered.Content},
		}

		idx, env := selectCurrentSkill(msgs, 2, -1)
		require.Equal(t, 2, idx)
		assert.Equal(t, rendered.Content, env)
	})

	t.Run("successful skill tool result", func(t *testing.T) {
		msgs := []llmwire.Message{
			{Role: llmwire.RoleSystem, Content: "sys"},
			compactionUserMessage("task"),
			{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "s1", Name: "skill"}}},
			{Role: llmwire.RoleTool, ToolCallID: "s1", ToolName: "skill", Content: "[review]\n" + rendered.Content},
		}

		idx, env := selectCurrentSkill(msgs, 2, -1)
		assert.Equal(t, 3, idx)
		assert.Equal(t, rendered.Content, env)
	})

	t.Run("failed skill call carries no envelope", func(t *testing.T) {
		msgs := []llmwire.Message{
			compactionUserMessage("task"),
			{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "sf", Name: "skill"}}},
			{Role: llmwire.RoleTool, ToolCallID: "sf", ToolName: "skill", Content: "Error: skill unavailable: nope"},
		}

		idx, env := selectCurrentSkill(msgs, 1, -1)
		assert.Equal(t, -1, idx)
		assert.Empty(t, env)
	})

	t.Run("batch-shaped output is historical, never current", func(t *testing.T) {
		msgs := []llmwire.Message{
			compactionUserMessage("task"),
			{
				Role:     llmwire.RoleTool,
				ToolName: "batch",
				Content:  "=== skill (call 1) ===\n" + rendered.Content,
			},
		}

		idx, _ := selectCurrentSkill(msgs, 2, -1)
		assert.Equal(t, -1, idx)
	})

	t.Run("reinvocation with new arguments wins", func(t *testing.T) {
		reinvoked := skillMessage(t, "review", "new body")

		msgs := []llmwire.Message{
			compactionUserMessage("task"),
			compactionUserMessage(rendered.Content),
			compactionAssistantCall("c1", "work"),
			compactionToolResult("c1", "result"),
			compactionUserMessage(reinvoked.Content),
		}

		idx, env := selectCurrentSkill(msgs, 2, -1)
		assert.Equal(t, 4, idx, "the latest candidate wins")
		assert.Equal(t, reinvoked.Content, env)
	})
}

func TestSelectCheckpointSplitNeverSummarizesWholeShortHistory(t *testing.T) {
	// A short raw history (under window/10) used to be summarized whole: the
	// empty tail was legal. One legal group must stay verbatim (D3).
	messages := []llmwire.Message{
		{Role: llmwire.RoleUser, Content: strings.Repeat("a", 80_000)}, // ~20k tokens
		{Role: llmwire.RoleAssistant, Content: "a", ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		{Role: llmwire.RoleTool, Content: "t", ToolCallID: "c1", ToolName: "read"},
	}

	const window = 80_000

	split, ok := selectCheckpointSplit(messages, checkpointPrefix{rawStart: 0}, 0, window, 0)
	require.True(t, ok)
	assert.Less(t, split, len(messages), "the tail is never empty")
	assert.Positive(t, split)
}

func TestSelectCheckpointSplitImageTailCeiling(t *testing.T) {
	const window = 1_048_576

	// Six page scans: the token floor wants half the history verbatim, but two
	// scans already push the tail over the 6 MB ceiling — the ceiling wins (D2).
	messages := []llmwire.Message{{Role: llmwire.RoleUser, Content: "task"}}
	for i := range 6 {
		messages = append(messages,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				Content:   "a",
				ToolCalls: []llmwire.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read"}},
			},
			llmwire.Message{
				Role: llmwire.RoleTool, Content: "t", ToolCallID: fmt.Sprintf("c%d", i), ToolName: "read",
				Images: []llmwire.ImageRef{{
					Path: fmt.Sprintf("/tmp/p%d.png", i), Mime: llmwire.MimeImagePng, Size: 2_290_000,
					Width: 1615, Height: 2193,
				}},
			},
		)
	}

	split, ok := selectCheckpointSplit(messages, checkpointPrefix{rawStart: 0}, 0, window, 0)
	require.True(t, ok)

	tail := messages[split:]
	tailBytes, tailCount := imagePressure(tail)
	assert.LessOrEqual(t, tailBytes, int64(imageBytesLowWater))
	assert.LessOrEqual(t, tailCount, imageCountLowWater)
	assert.NotEmpty(t, tail, "the tail is never empty")
	assert.Less(t, split, len(messages))
}
