package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// A fresh session, a resume from SQLite and a subagent all start with nothing
// measured, so the trigger runs on the whole-transcript estimate.
func TestProjectContextSize_UnmeasuredSessionsEstimate(t *testing.T) {
	agent := newTestAgent()
	setTestMessages(agent, buildMessagesWithTokens(1000))

	size, estimated := agent.projectContextSize()

	assert.True(t, estimated)
	assert.Equal(t, 1000+agent.requestOverhead(), size)
}

// The compaction call goes through s.chat, not callLLM: its own usage — which
// carries the entire pre-compaction conversation — must never become the
// baseline, or the next check would compact again immediately.
func TestCompactionLeavesNoBaselineBehind(t *testing.T) {
	ctx := context.Background()
	mockLLM := &compactionMockLLM{
		response: &llmwire.Response{
			Text:       validSummary,
			FinishType: llmwire.FinishStop,
			Usage:      &llmwire.MessageUsage{PromptTokens: 999_999},
		},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(mockLLM)

	seedCompactableTranscript(ctx, t, s)
	s.storeContextBaseline(150000, 2, s.modelGeneration())

	ok, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Nil(t, s.loadContextBaseline(), "the summarization request is not a measurement of the new transcript")
}

// A failed attempt changes no transcript metadata, so the baseline it described
// still describes the active transcript and must be kept.
func TestFailedCompactionKeepsItsBaseline(t *testing.T) {
	ctx := context.Background()
	mockLLM := &compactionMockLLM{err: errStoreDown, contextWindow: 200000}
	s := newCompactionTestSvc(mockLLM)

	seedCompactableTranscript(ctx, t, s)
	s.storeContextBaseline(150000, 2, s.modelGeneration())

	_, err := s.compact(ctx, nil)
	require.Error(t, err)

	assert.NotNil(t, s.loadContextBaseline(), "the transcript was not rewritten, so its measurement stands")
}

// Another window and another tokenizer: the measurement describes neither.
func TestNoOpCompactionKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	})
	s.storeContextBaseline(1234, 2, s.modelGeneration())

	compacted, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.False(t, compacted)

	base := s.loadContextBaseline()
	require.NotNil(t, base)
	assert.Equal(t, 1234, base.promptTokens)
}
