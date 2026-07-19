package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

const (
	validSummary   = "## Goal\nBuild\n## Progress\n- Done\n## Context for Continuation\nGo on"
	partialSummary = "## Goal\nOnly the goal"
)

func TestCompactWithRetryStopsAfterThreeAttempts(t *testing.T) {
	llm := &compactionMockLLM{
		chat: func(callIndex int, _ string) (*llmwire.Response, error) {
			if callIndex > 3 {
				return nil, errors.New("compactWithRetry looped past its attempt budget")
			}

			return &llmwire.Response{Text: fmt.Sprintf("%s %d", partialSummary, callIndex)}, nil
		},
	}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactWithRetry(t.Context(), &compactionUsage{}, func() string { return "base prompt" })

	require.NoError(t, err)
	assert.Equal(t, 3, llm.callCount)
	assert.Equal(t, partialSummary+" 3", brief)
}

func TestCompactWithRetryAppendsMissingSectionsOnlyAfterTheFirstAttempt(t *testing.T) {
	llm := &compactionMockLLM{
		chat: func(callIndex int, _ string) (*llmwire.Response, error) {
			if callIndex == 1 {
				return &llmwire.Response{Text: partialSummary}, nil
			}

			return &llmwire.Response{Text: validSummary}, nil
		},
	}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactWithRetry(t.Context(), &compactionUsage{}, func() string { return "base prompt" })

	require.NoError(t, err)
	assert.Equal(t, validSummary, brief)
	require.Len(t, llm.prompts, 2)
	assert.Equal(t, "base prompt", llm.prompts[0])
	// The hint is appended, never substituted: the retry must still carry the conversation.
	assert.True(t, strings.HasPrefix(llm.prompts[1], "base prompt"))
	assert.Contains(t, llm.prompts[1],
		"missing these required sections: ## Progress, ## Context for Continuation")
}

func TestCompactWithRetryPropagatesChatErrors(t *testing.T) {
	llm := &compactionMockLLM{err: errors.New("provider down")}
	s := newCompactionTestSvc(llm)

	_, err := s.compactWithRetry(t.Context(), &compactionUsage{}, func() string { return "base prompt" })

	require.ErrorContains(t, err, "compaction chat")
	assert.Equal(t, 1, llm.callCount)
}

// A summary that never arrived must surface as an error: replacing real work
// with a placeholder destroys the transcript it was meant to summarize.
func TestCompactInitialLockedPropagatesSummarizationFailure(t *testing.T) {
	llm := &compactionMockLLM{err: errors.New("provider down")}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactInitialLocked(t.Context(), []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "small"},
		{Role: llmwire.RoleTool, Content: strings.Repeat("H", 40000)},
	}, &compactionUsage{})

	require.ErrorContains(t, err, "summarize conversation")
	assert.Empty(t, brief)
	assert.Equal(t, 1, llm.callCount, "no second attempt at a degraded summary")
}

func TestCompactInitialLockedPropagatesCancellation(t *testing.T) {
	llm := &compactionMockLLM{err: context.Canceled}
	s := newCompactionTestSvc(llm)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	brief, err := s.compactInitialLocked(ctx, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "small"},
	}, &compactionUsage{})

	require.ErrorIs(t, err, context.Canceled)
	assert.Empty(t, brief)
}

func TestCompactMergeLockedReturnsTheMergedBrief(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactMergeLocked(t.Context(), "EXISTING", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "new work"},
	}, &compactionUsage{})

	require.NoError(t, err)
	assert.Equal(t, validSummary, brief)
}

func TestCompactMergeLockedAbortsOnFailure(t *testing.T) {
	llm := &compactionMockLLM{err: errors.New("provider down")}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactMergeLocked(t.Context(), "EXISTING", []llmwire.Message{
		{Role: llmwire.RoleUser, Content: "new work"},
		{Role: llmwire.RoleUser, Content: "more work"},
	}, &compactionUsage{})

	require.ErrorContains(t, err, "merge 2 new messages")
	assert.Empty(t, brief, "keeping the old brief would compact those messages away unrecorded")
}
