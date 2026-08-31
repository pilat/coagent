package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

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

// Successive checkpoints never duplicate the current envelope: exactly one
// byte-identical copy survives across head/tail movement, repeated compactions
// and restart-agnostic reloads.
func TestTwentySuccessiveCompactionsKeepTheCurrentEnvelopeExactlyOnce(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	rendered := skillMessage(t, "review", "Review carefully.")
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionUserMessage(rendered.Content),
	}

	for i := range 40 {
		msgs = append(msgs, roundTokens(fmt.Sprintf("r%d", i), 5, 200)...)
	}
	s.ms.setMessages(msgs)

	for range 20 {
		ok, err := s.compact(t.Context(), nil)
		require.NoError(t, err)

		skills := renderedSkills(s.ms.getMessages())

		if !ok {
			// Nothing raw to summarize; the transcript must still hold exactly
			// one envelope (the one the checkpoint carries).
			require.Len(t, skills, 1)
			assert.Equal(t, rendered.Content, skills[0].Content)

			break
		}

		require.Len(t, skills, 1, "exactly one current envelope after compaction")
		assert.Equal(t, rendered.Content, skills[0].Content, "byte-identical envelope, never a summarized body")
	}
}

// A current skill whose activation falls inside the head is reattached
// byte-identically between the summary and the tail, and its activation rows
// are excluded from the summarizer input.
func TestCurrentSkillInHeadIsReattachedByteIdentically(t *testing.T) {
	const window = 200000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	rendered := skillMessage(t, "review", "Review carefully.")

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "s1", Name: "skill"}}},
		{Role: llmwire.RoleTool, Content: "[review]\n" + rendered.Content, ToolCallID: "s1", ToolName: "skill"},
		compactionAssistantCall("c1", "MIDDLE-WORK"),
		compactionToolResult("c1", "middle result"),
		compactionAssistantCall("c2", "recent"),
		compactionToolResult("c2", "recent result"),
	})

	ok, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)

	skills := renderedSkills(s.ms.getMessages())
	require.Len(t, skills, 1)
	assert.Equal(t, llmwire.RoleUser, skills[0].Role)
	assert.Equal(t, rendered.Content, skills[0].Content)

	// The summarized history must not contain the skill body.
	require.Len(t, llm.prompts, 1)
	assert.NotContains(t, llm.prompts[0], "Review carefully.",
		"the current envelope is reattached verbatim, never summarized")
}

// A latest skill invoked alongside other calls keeps those remaining calls as
// valid pairs; only the skill call/result is omitted from the projection.
func TestMixedAssistantResponseKeepsSiblingCallsValid(t *testing.T) {
	window := 200000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	rendered := skillMessage(t, "review", "Review carefully.")

	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		{
			Role: llmwire.RoleAssistant,
			ToolCalls: []llmwire.ToolCall{
				{ID: "skill-call", Name: "skill"},
				{ID: "work-call", Name: "read"},
			},
		},
		{Role: llmwire.RoleTool, ToolCallID: "skill-call", ToolName: "skill", Content: "[review]\n" + rendered.Content},
		compactionToolResult("work-call", "body"),
		compactionAssistantCall("c1", "later"),
		compactionToolResult("c1", "later result"),
	}
	msgs[3] = llmwire.Message{
		Role: llmwire.RoleTool, ToolCallID: "skill-call", ToolName: "skill",
		Content: "[review]\n" + rendered.Content,
	}

	s.ms.setMessages(msgs)

	ok, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, validateRawHead(s.ms.getMessages()), "remaining calls stay valid pairs")
	skills := renderedSkills(s.ms.getMessages())
	require.Len(t, skills, 1, "exactly one envelope survives")
}
