package session

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

func TestHookFeedbackDeduplicatesInOrder(t *testing.T) {
	feedback := []string{"first guard", "second guard", "first guard", "third guard", "second guard"}
	assert.Equal(t, "original\n\nfirst guard\n\nsecond guard\n\nthird guard", committedHookContent(t, feedback))
}

func TestHookFeedbackCapsJoinedText(t *testing.T) {
	feedback := []string{strings.Repeat("界", 6500), strings.Repeat("é", 6500)}
	content := strings.TrimPrefix(committedHookContent(t, feedback), "original\n\n")
	assert.Equal(t, 10000, utf8.RuneCountInString(content))
	assert.True(t, utf8.ValidString(content))
	assert.True(t, strings.HasPrefix(content, feedback[0]+"\n\n"))
	assert.True(t, strings.HasSuffix(content, "\n[hook feedback truncated]"))
	assert.Less(t, strings.Count(content, "é"), 6500)
}

func TestHookFeedbackEmptyLeavesRowUnchanged(t *testing.T) {
	for _, feedback := range [][]string{nil, {}, {""}} {
		assert.Equal(t, "original", committedHookContent(t, feedback))
	}
}

func committedHookContent(t *testing.T, feedback []string) string {
	t.Helper()
	stub := &imageStubTool{
		id: "fixture",
		result: &tool.Result{
			Output:       "original",
			HookFeedback: feedback,
		},
	}
	agent := newTestAgent(stub)
	calls := []llmwire.ToolCall{{
		ID:        "fixture-call",
		Name:      stub.ID(),
		Arguments: []byte(`{}`),
	}}
	require.NoError(t, appendTestAssistant(t.Context(), agent.ms, &llmwire.Response{ToolCalls: calls}))
	require.NoError(t, executeToolCalls(t.Context(), agent, calls))
	rows := agent.store.(*mockSessionStore).messages
	require.Len(t, rows, 2)
	return rows[1].Content
}
