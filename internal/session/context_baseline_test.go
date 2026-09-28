package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// A fresh session, a resume from SQLite and a subagent all start with nothing
// measured, so the trigger runs on the whole-transcript estimate.
func TestProjectContextSize_UnmeasuredSessionsEstimate(t *testing.T) {
	agent := newTestAgent()
	agent.ms.setMessages(buildMessagesWithTokens(1000))

	size, estimated := agent.contexts.projectContextSize()

	assert.True(t, estimated)
	assert.Equal(t, 1000+agent.contexts.(*checkpointOwner).requestOverhead(), size)
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
	s.models.recordBaseline(ctx, 150000, 2, s.models.snapshot().generation)

	ok, err := s.contexts.(*checkpointOwner).compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Nil(t, s.models.snapshot().baseline, "the summarization request is not a measurement of the new transcript")
}

// A failed attempt changes no transcript metadata, so the baseline it described
// still describes the active transcript and must be kept.
func TestFailedCompactionKeepsItsBaseline(t *testing.T) {
	ctx := context.Background()
	mockLLM := &compactionMockLLM{err: errStoreDown, contextWindow: 200000}
	s := newCompactionTestSvc(mockLLM)

	seedCompactableTranscript(ctx, t, s)
	s.models.recordBaseline(ctx, 150000, 2, s.models.snapshot().generation)

	_, err := s.contexts.(*checkpointOwner).compact(ctx, nil)
	require.Error(t, err)

	assert.NotNil(t, s.models.snapshot().baseline, "the transcript was not rewritten, so its measurement stands")
}

// Another window and another tokenizer: the measurement describes neither.
func TestModelSwitchDropsTheBaseline(t *testing.T) {
	ctx := context.Background()

	s := &sessionModel{
		cfg:       &config.Config{UnifiedConfig: unifiedCfgWithModels("m1", "m2")},
		client:    &mockLLMClientTracked{model: "m1"},
		model:     "m1",
		reasoning: "medium",
		prompt:    newPromptBuilder("", ""),
		registry:  tool.NewRegistry(),

		newClient: func(_ *config.Config, id string) (llm.Client, error) {
			return &mockLLMClientTracked{model: id}, nil
		},
	}
	s.recordBaseline(ctx, 50000, 4, s.snapshot().generation)

	require.NoError(t, s.SetModel("m2", "medium"))

	assert.Nil(t, s.snapshot().baseline)
}

func TestContextResetDropsTheBaseline(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	s := newResetTestSvc(store)

	seedResetTranscript(ctx, t, s)
	s.models.recordBaseline(ctx, 50000, 3, s.models.snapshot().generation)

	inserted, err := s.ResetContextAndInjectOnce(ctx, "reset:fresh:1", "do the fresh job")
	require.NoError(t, err)
	require.True(t, inserted)

	assert.Nil(t, s.models.snapshot().baseline)
}

// A switch that lands while a request is in flight must not be overwritten by
// that request's measurement: it describes a window and tokenizer the session
// no longer uses.
func TestBaselineFromAnInFlightRequestIsDroppedAfterAModelSwitch(t *testing.T) {
	ctx := context.Background()

	s := &sessionModel{
		cfg:       &config.Config{UnifiedConfig: unifiedCfgWithModels("m1", "m2")},
		client:    &mockLLMClientTracked{model: "m1"},
		model:     "m1",
		reasoning: "medium",
		prompt:    newPromptBuilder("", ""),
		registry:  tool.NewRegistry(),

		newClient: func(_ *config.Config, id string) (llm.Client, error) {
			return &mockLLMClientTracked{model: id}, nil
		},
	}

	// Sampled before the request goes out, as callLLM does.
	generation := s.snapshot().generation

	require.NoError(t, s.SetModel("m2", "medium"))

	s.recordBaseline(ctx, 150000, 4, generation)

	assert.Nil(t, s.snapshot().baseline, "the in-flight measurement belongs to the old model")

	// A measurement taken after the switch is kept.
	s.recordBaseline(ctx, 1000, 1, s.snapshot().generation)
	assert.NotNil(t, s.snapshot().baseline)
}

// A compaction that summarizes nothing changes nothing, so it must not throw
// away a valid measurement and downgrade the next check to a pure estimate.
func TestNoOpCompactionKeepsTheBaseline(t *testing.T) {
	ctx := context.Background()
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	})
	s.models.recordBaseline(ctx, 1234, 2, s.models.snapshot().generation)

	compacted, err := s.contexts.(*checkpointOwner).compact(ctx, nil)
	require.NoError(t, err)
	require.False(t, compacted)

	base := s.models.snapshot().baseline
	require.NotNil(t, base)
	assert.Equal(t, 1234, base.promptTokens)
}
