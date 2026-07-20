package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// compact_context only raises a flag the loop reads later. A suspend in the same
// turn rebuilds the session and the flag dies with it, so the combination is
// refused at execution rather than lost in silence.
func TestExecuteToolCallsRejectsCompactionAlongsideASuspendingCall(t *testing.T) {
	for _, tc := range []struct {
		name     string
		conflict string
	}{
		{"blocking task", tool.IDTask},
		{"sleep", tool.IDSleep},
		{"config apply", tool.IDSetProvider},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent(
				&stubTool{id: tool.IDCompactContext, result: "should not run"},
				&stubTool{id: tc.conflict, result: "started"},
			)

			require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
				{ID: "c1", Name: tool.IDCompactContext, Arguments: []byte(`{}`)},
				{ID: "c2", Name: tc.conflict, Arguments: []byte(`{}`)},
			}))

			result := toolResultFor(agent.ms.getMessages(), "c1")
			assert.Contains(t, result, "cannot compact in the same turn as "+tc.conflict)
			assert.False(t, agent.compactionRequested(), "the flag must not be raised")
			assert.Equal(t, "started", toolResultFor(agent.ms.getMessages(), "c2"),
				"the sibling call is untouched by the rejection")
		})
	}
}

// The rejection costs the model nothing but a turn: once the suspending call has
// settled, asking again works.
func TestCompactionWorksOnTheTurnAfterARejectedCombination(t *testing.T) {
	agent := newTestAgent(
		newCompactContextTool(agentCompactorStub{}),
		&stubTool{id: tool.IDTask, result: "spawned"},
	)

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "c1", Name: tool.IDCompactContext, Arguments: []byte(`{}`)},
		{ID: "c2", Name: tool.IDTask, Arguments: []byte(`{}`)},
	}))
	require.Contains(t, toolResultFor(agent.ms.getMessages(), "c1"), "cannot compact")

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "c3", Name: tool.IDCompactContext, Arguments: []byte(`{}`)},
	}))

	assert.Contains(t, toolResultFor(agent.ms.getMessages(), "c3"), "compaction requested")
}

// Alone, compact_context still works.
func TestExecuteToolCallsAllowsCompactionOnItsOwn(t *testing.T) {
	agent := newTestAgent(newCompactContextTool(agentCompactorStub{}))

	require.NoError(t, executeToolCalls(t.Context(), agent, []llmwire.ToolCall{
		{ID: "c1", Name: tool.IDCompactContext, Arguments: []byte(`{}`)},
	}))

	assert.Contains(t, toolResultFor(agent.ms.getMessages(), "c1"), "compaction requested")
}

type agentCompactorStub struct{}

func (agentCompactorStub) RequestCompaction(int) {}

func toolResultFor(msgs []llmwire.Message, callID string) string {
	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID == callID {
			return m.Content
		}
	}

	return ""
}
