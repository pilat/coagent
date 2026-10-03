package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// summarySizedForProjection returns summarizer text whose marked projection
// makes the post-commit estimate land on wantTailTokens summary tokens.
func summarySizedForProjection(t *testing.T, wantTailTokens int) string {
	t.Helper()

	for n := 4 * wantTailTokens; n > 0; n-- {
		if len(renderMarkedSummary(strings.Repeat("a", n), ""))/4 == wantTailTokens {
			return strings.Repeat("a", n)
		}
	}

	t.Fatal("no summary length lands on the target token count")

	return ""
}

// equality at the 85% cutoff is relieving: the trigger is strict
// greater-than, so a projection exactly on the cutoff commits. The retained
// tail's own tokens participate in the projection and are subtracted up front.
func TestCompactionCommitsAtExactCutoff(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{contextWindow: window}
	s := newCompactionTestSvc(llm)
	header := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}
	retainedTail := []llmwire.Message{
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	}
	setTestMessages(s, append(header, append(
		[]llmwire.Message{compactionUserMessage("raw")}, retainedTail...)...))

	target := compactionCutoff(window) - estimateTokens(header) - s.requestOverhead() - estimateTokens(retainedTail)
	llm.response = &llmwire.Response{
		Text:       summarySizedForProjection(t, target),
		FinishType: llmwire.FinishStop,
	}

	ok, err := s.compact(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, ok, "a projection exactly on the cutoff is relieving")

	size := estimateTokens(s.ms.getMessages()) + s.requestOverhead()
	assert.Equal(t, compactionCutoff(window), size)
}

// The relief check adds the request overhead to the projection: a summary one
// token under the cutoff is still rejected once the overhead pushes it over.
func TestCompactionRejectsWhenOverheadPushesOverCutoff(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{contextWindow: window}
	s := newCompactionTestSvc(llm)
	header := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}
	retainedTail := []llmwire.Message{
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	}
	setTestMessages(s, append(header, append(
		[]llmwire.Message{compactionUserMessage("raw")}, retainedTail...)...))

	target := compactionCutoff(window) - estimateTokens(header) - s.requestOverhead() - estimateTokens(retainedTail) + 1
	llm.response = &llmwire.Response{
		Text:       summarySizedForProjection(t, target),
		FinishType: llmwire.FinishStop,
	}

	ok, err := s.compact(context.Background(), nil)
	require.ErrorIs(t, err, errCompactionNonRelieving)
	assert.False(t, ok)
	assert.Len(t, s.ms.getMessages(), 5, "a non-relieving candidate commits nothing")
}
