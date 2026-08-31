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

// Every rejected candidate leaves the active transcript and its metadata
// exactly as they were.
func TestCompactLeavesTheTranscriptIntactOnFailure(t *testing.T) {
	tests := []struct {
		name string
		llm  func() *compactionMockLLM
	}{
		{
			name: "provider error",
			llm:  func() *compactionMockLLM { return &compactionMockLLM{err: errors.New("provider down")} },
		},
		{
			name: "cancelled mid-compaction",
			llm:  func() *compactionMockLLM { return &compactionMockLLM{err: context.Canceled} },
		},
		{
			name: "empty text",
			llm: func() *compactionMockLLM {
				return &compactionMockLLM{response: &llmwire.Response{Text: "", FinishType: llmwire.FinishStop}}
			},
		},
		{
			name: "whitespace-only text",
			llm: func() *compactionMockLLM {
				return &compactionMockLLM{response: &llmwire.Response{Text: "  \n  ", FinishType: llmwire.FinishStop}}
			},
		},
		{
			name: "tool-calling response",
			llm: func() *compactionMockLLM {
				return &compactionMockLLM{response: &llmwire.Response{
					Text: "let me call a tool", FinishType: llmwire.FinishToolCalls,
					ToolCalls: []llmwire.ToolCall{{ID: "x", Name: "read"}},
				}}
			},
		},
		{
			name: "length-stopped response",
			llm: func() *compactionMockLLM {
				return &compactionMockLLM{response: &llmwire.Response{
					Text: "partial", FinishType: llmwire.FinishLength,
				}}
			},
		},
		{
			name: "unknown finish",
			llm: func() *compactionMockLLM {
				return &compactionMockLLM{response: &llmwire.Response{
					Text: "looks complete", FinishType: llmwire.FinishUnknown,
				}}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := &compactionRecordingStore{nextID: 1}
			llm := tc.llm()
			s := newCompactionTestSvc(llm)
			s.ms = newMessageStore(store, 1)

			seedCompactableTranscript(ctx, t, s)
			before := s.ms.getMessages()
			beforeRowIDs := s.ms.getRowIDs()

			ok, err := s.compact(ctx, nil)

			require.Error(t, err)
			assert.False(t, ok)

			after := s.ms.getMessages()
			afterRowIDs := s.ms.getRowIDs()
			require.Len(t, after, len(before))
			for i := range before {
				assert.Equal(t, before[i].Role, after[i].Role)
				assert.Equal(t, before[i].Content, after[i].Content)
			}
			assert.Equal(t, beforeRowIDs, afterRowIDs)

			assert.Zero(t, store.markCompacted, "nothing may be hidden without a committed checkpoint")
			assert.False(t, hasSummaryRow(after), "no partial summary survives the failure")

			for _, message := range store.messages {
				assert.NotContains(t, message.Content, compactionMarkOpen)
			}
		})
	}
}

// A durable write failure must not advance the in-memory projection either:
// the summarized messages are still active, and a committed-looking checkpoint
// without the durable swap would summarize the same work twice.
func TestCompactKeepsTheOldTranscriptWhenTheDurableSwapFails(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1, replaceErr: errStoreDown}
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32000,
	}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)

	seedCompactableTranscript(ctx, t, s)

	ok, err := s.compact(ctx, nil)

	require.Error(t, err)
	assert.False(t, ok)

	after := s.ms.getMessages()
	assert.False(t, hasSummaryRow(after))
}

// A candidate that would still sit above the trigger is refused: compaction
// spends no metadata, and the caller records the non-relieving outcome.
func TestCompactHeaderAloneOverThreshold(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	// The header is under the trigger but over half the window, so no legal
	// summarizer request exists at all.
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 100000)},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", strings.Repeat("r", 4000)),
	})

	ok, err := s.compact(t.Context(), nil)

	require.NoError(t, err, "an unfittable head is nothing to compact, not a failure")
	assert.False(t, ok)
	assert.Zero(t, llm.callCount, "no LLM call is worth making")

	after := s.ms.getMessages()
	assert.False(t, hasSummaryRow(after))
}

// A candidate checkpoint that would still sit above the trigger is refused
// whole: no metadata changes, and the provider call is still counted as spent.
func TestCompactRefusesANonRelievingCandidate(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	// Huge indivisible groups: the largest legal head inside the 50% bound is
	// one group, so the retained tail still leaves the projection above the
	// cutoff and the candidate is refused.
	payload := []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}, compactionUserMessage("task")}
	for i := range 5 {
		payload = append(payload, roundTokens(fmt.Sprintf("c%d", i), 100, 8000)...)
	}
	s.ms.setMessages(payload)

	ok, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionNonRelieving)
	assert.False(t, ok)
	assert.Positive(t, llm.callCount, "the summarizer ran before the relief check refused")

	after := s.ms.getMessages()
	assert.False(t, hasSummaryRow(after), "the active transcript keeps byte-identical history")
	assert.Len(t, after, len(payload))
}

// One compaction attempt makes exactly one model call — no compaction-specific
// retry, corrective prompt, or second pass exists.
func TestCompactionMakesExactlyOneModelCall(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	llm := &compactionMockLLM{
		contextWindow: 32000,
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
	}
	s := newCompactionTestSvc(llm)
	s.ms = newMessageStore(store, 1)
	s.ms.setMessages(oversizedTranscript(32000))

	require.NoError(t, s.compactIfNeeded(ctx, 32000))

	assert.Equal(t, 1, llm.callCount)
}

// Only a fully completed non-empty text response is a checkpoint.
func TestAcceptedCheckpointTextRejections(t *testing.T) {
	tests := []struct {
		name string
		resp *llmwire.Response
	}{
		{name: "nil response", resp: nil},
		{
			name: "empty text",
			resp: &llmwire.Response{
				Text: "", FinishType: llmwire.FinishStop,
			},
		},
		{
			name: "whitespace text",
			resp: &llmwire.Response{
				Text: "\n\t \n", FinishType: llmwire.FinishStop,
			},
		},
		{
			name: "tool calls",
			resp: &llmwire.Response{
				Text: "compacting…", FinishType: llmwire.FinishToolCalls,
				ToolCalls: []llmwire.ToolCall{{ID: "c", Name: "read"}},
			},
		},
		{
			name: "length stop",
			resp: &llmwire.Response{
				Text: "partial summary", FinishType: llmwire.FinishLength,
			},
		},
		{
			name: "unknown finish",
			resp: &llmwire.Response{
				Text: "finished somehow", FinishType: llmwire.FinishUnknown,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := acceptedCheckpointText(tc.resp)
			require.Error(t, err)
		})
	}

	t.Run("normal completed text is accepted", func(t *testing.T) {
		brief, err := acceptedCheckpointText(&llmwire.Response{Text: " summary ", FinishType: llmwire.FinishStop})
		require.NoError(t, err)
		assert.Equal(t, "summary", brief)
	})

	t.Run("a short completed answer is accepted", func(t *testing.T) {
		brief, err := acceptedCheckpointText(&llmwire.Response{Text: "brief", FinishType: llmwire.FinishStop})
		require.NoError(t, err)
		assert.Equal(t, "brief", brief, "output is not rejected for being shorter than the reserve")
	})
}
