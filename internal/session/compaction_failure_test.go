package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
)

// Compaction is one call. The middle fits the window it was being sent through,
// so there is nothing to split, merge, or partially summarize.
func TestCompactInitialLockedUsesOneCall(t *testing.T) {
	llm := &compactionMockLLM{
		contextWindow: 200000,
		response:      &llmwire.Response{Text: validSummary},
	}
	s := newCompactionTestSvc(llm)

	brief, err := s.compactInitialLocked(
		t.Context(),
		[]llmwire.Message{userTokens(20000), userTokens(20000), userTokens(20000)},
		&compactionUsage{},
	)

	require.NoError(t, err)
	assert.Equal(t, validSummary, brief)
	assert.Equal(t, 1, llm.callCount, "60k tokens of conversation into a 200k window is one call")
}

// A conversation the compaction model cannot hold is reported as an error — the
// session keeps the uncompacted dialogue rather than a summary of part of it.
func TestCompactInitialLockedRefusesAConversationThatDoesNotFit(t *testing.T) {
	llm := &compactionMockLLM{
		contextWindow: 10000,
		response:      &llmwire.Response{Text: validSummary},
	}
	s := newCompactionTestSvc(llm)

	_, err := s.compactInitialLocked(
		t.Context(),
		[]llmwire.Message{userTokens(4000), userTokens(4000), userTokens(4000)},
		&compactionUsage{},
	)

	require.ErrorIs(t, err, errCompactionTooLarge)
	assert.Zero(t, llm.callCount, "nothing is sent when it cannot fit")
}

func TestCompactMergeRefusesAConversationThatDoesNotFit(t *testing.T) {
	llm := &compactionMockLLM{
		contextWindow: 10000,
		response:      &llmwire.Response{Text: validSummary},
	}
	s := newCompactionTestSvc(llm)

	_, err := s.compactMergeLocked(
		t.Context(),
		"EXISTING",
		[]llmwire.Message{userTokens(4000), userTokens(4000), userTokens(4000)},
		&compactionUsage{},
	)

	require.ErrorIs(t, err, errCompactionTooLarge)
	assert.Zero(t, llm.callCount)
}

// The summarizer must read the whole conversation: a brief written without the
// opening task and the current tail can say neither where the work started nor
// where it stands — the two things a /compact focus most often asks for.
func TestCompactSendsTheWholeConversationToTheSummarizer(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	llm := &compactionMockLLM{contextWindow: 200000, response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "ORIGINAL TASK"},
	}
	for i := range 3 {
		messages = append(messages, roundTokens(fmt.Sprintf("c%d", i), 10, 10)...)
	}
	messages = append(messages, llmwire.Message{Role: llmwire.RoleAssistant, Content: "LATEST STATE"})

	for i := range messages {
		message := messages[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}

	ok, err := s.compact(ctx, 1)

	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "ORIGINAL TASK", "the opening task frames the whole brief")
	assert.Contains(t, llm.prompts[0], "LATEST STATE", "the retained tail is where the work stands now")
}

// The incremental path omits only the rows the existing brief already condenses.
func TestCompactMergeSendsEverythingAfterThePreviousBrief(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	llm := &compactionMockLLM{contextWindow: 200000, response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)
	s.compactionBrief = "EXISTING BRIEF"

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "ORIGINAL TASK"},
		{Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed]\n\nEXISTING BRIEF"},
		{Role: llmwire.RoleAssistant, Content: registry.PostCompactionAssistantAck},
	}
	for i := range 3 {
		messages = append(messages, roundTokens(fmt.Sprintf("c%d", i), 10, 10)...)
	}
	messages = append(messages, llmwire.Message{Role: llmwire.RoleAssistant, Content: "LATEST STATE"})

	for i := range messages {
		message := messages[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}

	ok, err := s.compact(ctx, 1)

	require.NoError(t, err)
	require.True(t, ok)
	require.Len(t, llm.prompts, 1)
	assert.Contains(t, llm.prompts[0], "EXISTING BRIEF", "carried in as the merge base")
	assert.Contains(t, llm.prompts[0], "LATEST STATE")
	assert.NotContains(t, llm.prompts[0], "[CONTEXT SUMMARY", "the brief is passed in, not replayed as a message")
}

func TestCompactLeavesTheTranscriptIntactWhenSummarizationFails(t *testing.T) {
	for _, testCase := range []struct {
		name string
		err  error
	}{
		{name: "provider down", err: errors.New("provider down")},
		{name: "cancelled mid-compaction", err: context.Canceled},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store := &compactionRecordingStore{nextID: 1}
			llm := &compactionMockLLM{err: testCase.err}
			s := newCompactionTestSvc(llm)
			s.ms = newMessageStore(store, 1)

			seedCompactableTranscript(ctx, t, s)
			before := s.ms.getMessages()

			ok, err := s.compact(ctx, 1)

			require.Error(t, err)
			assert.False(t, ok)
			// Trimming tool bodies is compaction's own first step and survives on its
			// own terms (the call stays visible, re-run to recover). What must not
			// happen is the summarizing rewrite: same messages, same order, nothing
			// hidden, no summary row.
			after := s.ms.getMessages()
			require.Len(t, after, len(before))
			for i := range before {
				assert.Equal(t, before[i].Role, after[i].Role)
				assert.Equal(t, before[i].DBID, after[i].DBID)
			}
			assert.Zero(t, store.markCompacted, "nothing may be hidden without a summary that replaces it")
			assert.Empty(t, s.compactionBrief, "no placeholder brief survives the failure")

			for _, message := range store.messages {
				assert.NotContains(t, message.Content, "[CONTEXT SUMMARY")
			}
		})
	}
}

// A durable write failure must not advance the in-memory brief either: the
// summarized messages are still in the transcript, and a brief describing them
// would make the next compaction summarize the same work twice.
func TestCompactKeepsTheOldBriefWhenTheDurableSwapFails(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1, markCompactedErr: errStoreDown}
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)

	seedCompactableTranscript(ctx, t, s)

	ok, err := s.compact(ctx, 1)

	require.Error(t, err)
	assert.False(t, ok)
	assert.Empty(t, s.compactionBrief)
}

func seedCompactableTranscript(ctx context.Context, t *testing.T, s *svc) {
	t.Helper()

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}
	for i := range 3 {
		messages = append(messages, roundTokens(fmt.Sprintf("c%d", i), 10, 10)...)
	}

	for i := range messages {
		message := messages[i]
		s.ms.mu.Lock()
		require.NoError(t, s.ms.appendMessageLocked(ctx, &message))
		s.ms.mu.Unlock()
	}
}

// The budget is window − output reserve, never the compactionFraction trigger:
// a payload the auto path hands over is above that fraction by definition, so
// budgeting by it would refuse every compaction it asks for.
func TestCompactionBudgetIsTheWindowMinusTheReserveNotTheTrigger(t *testing.T) {
	const window = 200000

	llm := &compactionMockLLM{contextWindow: window, response: &llmwire.Response{Text: validSummary}}
	s := newCompactionTestSvc(llm)

	// 180k of conversation: over the 170k trigger, under the 192k real budget.
	payload := make([]llmwire.Message, 0, 18)
	for range 18 {
		payload = append(payload, userTokens(10000))
	}

	require.Greater(t, estimateTokens(payload), compactionCutoff(window), "the trigger would have fired")
	require.Less(t, estimateTokens(payload), s.compactionInputBudget(), "but it still fits the real budget")

	_, err := s.compactInitialLocked(t.Context(), payload, &compactionUsage{})

	require.NoError(t, err)
	assert.Equal(t, 1, llm.callCount)
}
