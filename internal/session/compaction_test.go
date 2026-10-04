package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	llmclient "github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/tool"
)

// An aborted assistant turn is history, not a pairable call: the ordinary
// repair stubs it in the head projection and compaction succeeds around it.
func TestCompactionSucceedsAroundAnAbortedToolCall(t *testing.T) {
	const window = 32000

	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(mockLLM)
	setTestMessages(s, transcriptWithAbortedCall(window))

	err := s.compactIfNeeded(context.Background(), window)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	messages := s.ms.getMessages()
	require.NotEmpty(t, messages)
	assert.True(t, isMarkedSummary(messages[2].Content), "the checkpoint is committed")

	for _, m := range messages {
		for _, tc := range m.ToolCalls {
			assert.NotEmpty(t, tc.ID, "no incomplete call survives in the committed projection")
		}
	}
}

func TestCompactLeavesTranscriptsWithNothingToSummarize(t *testing.T) {
	summary := llmwire.Message{Role: llmwire.RoleUser, Content: renderMarkedSummary("old checkpoint", "")}

	tests := []struct {
		name     string
		messages []llmwire.Message
	}{
		{
			name:     "header only",
			messages: []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}, compactionUserMessage("task")},
		},
		{
			name: "previous summary is the last message",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
			},
		},
		{
			name: "previous summary plus its reattachment are the last messages",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				compactionUserMessage("task"),
				summary,
				skillMessage(t, "review", "Review carefully."),
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
			s := newCompactionTestSvc(llm)
			setTestMessages(s, tc.messages)

			compacted, err := s.compact(t.Context(), nil)

			require.NoError(t, err)
			assert.False(t, compacted, "nothing to compact")
			assert.Zero(t, llm.callCount)
			assert.Equal(t, tc.messages, s.ms.getMessages())
		})
	}
}

// A second compaction immediately after one has no raw delta: the previous
// summary and its reattachment scaffolding are the only rows present.
func TestSecondCompactionRightAfterOneFindsNothing(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		skillMessage(t, "review", "Review carefully."),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 1, llm.callCount)
	require.Len(t, renderedSkills(s.ms.getMessages()), 1, "the skill was reattached")

	compacted, err = s.compact(t.Context(), nil)

	require.NoError(t, err)
	assert.False(t, compacted, "nothing but the previous compaction's own output is present")
	assert.Equal(t, 1, llm.callCount, "no second summarization request")
}

// A repeated checkpoint replays the then-current prefix from the beginning:
// the complete previous marked summary and the former-tail rows that have aged
// above the split ride along; there is no separate anchor or delta request.
func TestSecondCheckpointReplaysTheNativePrefixFromTheStart(t *testing.T) {
	window := 1 << 20

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "MIDDLE-WORK"),
		compactionToolResult("c1", "middle result"),
		compactionAssistantCall("c2", "recent work"),
		compactionToolResult("c2", "recent result"),
	})

	_, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, llm.callCount)

	appendMessages(t, s, compactionAssistantCall("c3", "NEWLY-AGED"), compactionToolResult("c3", "new result"))

	compacted, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, compacted)
	require.Equal(t, 2, llm.callCount)

	// The second call is a fresh projection of the whole current transcript
	// (loaded rows plus the instruction): prior summary included, newest group
	// still in the tail.
	input := llm.lastMessages
	require.NotEmpty(t, input)

	assert.True(t, isMarkedSummary(input[2].Content), "the previous marked summary is replayed as its row")
	assert.Contains(t, input[2].Content, validSummary)
	assert.Contains(t, transcriptText(input), "recent work", "former-tail rows above the split ride along")
	assert.NotContains(t, transcriptText(input), "NEWLY-AGED", "the newest group stays verbatim in the non-empty tail")

	for _, m := range input {
		assert.NotContains(t, m.Content, "HISTORY TO SUMMARIZE", "no canonical JSONL sections remain")
		assert.NotContains(t, m.Content, "PREVIOUS SUMMARY")
	}

	last := input[len(input)-1]
	assert.Equal(t, llmwire.RoleUser, last.Role)
	assert.Contains(t, last.Content, "continuation checkpoint", "the instruction is final")
}

// A compliant summarizer converges on a 32k window: the projection right after
// the compaction is back under the trigger, and the counter resets.
func TestAutoCompactionConvergesOnASmallWindow(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))

	require.True(t, s.shouldCompact(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	require.NoError(t, s.compactionStep(context.Background(), r))

	assert.False(t, s.shouldCompact(window), "the projection is back under the trigger")
	assert.True(t, notesContain(notes, "✅ Context compacted"))
	assert.Zero(t, r.compactionFailures)
	assert.False(t, r.autoCompactionOff)
}

// A completed summary that still leaves the projection over the threshold is
// rejected pre-commit and counts as a failed attempt.
func TestAutoCompactionCountsANonRelievingCandidateAsAFailure(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response: &llmwire.Response{
			Text:       validSummary + "\n" + strings.Repeat("b", window*4),
			FinishType: llmwire.FinishStop,
		},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))
	before := s.ms.getMessages()

	var notes []string
	r := contextEventRunner(s, &notes)
	require.NoError(t, s.compactionStep(context.Background(), r))

	assert.Equal(t, 1, llm.callCount, "the summarizer ran")
	assert.True(t, notesContain(notes, "❌ Compaction failed"))
	assert.Len(t, s.ms.getMessages(), len(before), "a non-relieving candidate commits nothing")
	assert.Equal(t, 1, r.compactionFailures)
}

// Three attempts that free nothing and the automatic path goes quiet for the
// rest of the activation, with one notice — explicit requests still run.
func TestAutoCompactionStopsAfterThreeFruitlessAttempts(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response: &llmwire.Response{
			Text:       validSummary + "\n" + strings.Repeat("b", window*4),
			FinishType: llmwire.FinishStop,
		},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)

	for range compactionAttemptCap {
		require.NoError(t, s.compactionStep(context.Background(), r))
	}

	require.True(t, r.autoCompactionOff)
	assert.Equal(t, 1, countNotes(notes, compactionNotConvergingNotice))

	callsAtCap := llm.callCount
	require.NoError(t, s.compactionStep(context.Background(), r))
	assert.Equal(t, callsAtCap, llm.callCount, "the automatic path is silent for the rest of the activation")
}

// The cap governs the automatic path only: a human asking for compaction gets
// one, and asking does not clear the streak either.
func TestExplicitCompactionIgnoresTheAttemptCap(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap
	r.autoCompactionOff = true

	s.RequestCompaction()
	require.NoError(t, s.compactionStep(context.Background(), r))

	assert.Equal(t, 1, llm.callCount)
	assert.True(t, notesContain(notes, "✅ Context compacted"))
	assert.Equal(t, compactionAttemptCap, r.compactionFailures, "an explicit run neither counts nor clears")
}

// A successful, relieving compaction wipes the streak.
func TestAutoCompactionResetsTheCounterOnRelief(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap - 1

	require.NoError(t, s.compactionStep(context.Background(), r))

	assert.Zero(t, r.compactionFailures)
	assert.False(t, r.autoCompactionOff)
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
			name: "tool-calling response that persists after the nudge",
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
			s.store = store
			s.ms = newMessageStore(store, 1)

			seedCompactableTranscript(ctx, t, s)
			before := s.ms.getMessages()
			beforeRowIDs := slices.Clone(s.ms.rowIDs)

			ok, err := s.compact(ctx, nil)

			require.Error(t, err)
			assert.False(t, ok)

			after := s.ms.getMessages()
			afterRowIDs := slices.Clone(s.ms.rowIDs)
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
	s.store = store
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
	setTestMessages(s, []llmwire.Message{
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
	setTestMessages(s, payload)

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
	s.store = store
	s.ms = newMessageStore(store, 1)
	setTestMessages(s, oversizedTranscript(32000))

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

// A summarizer that answers with a tool call gets one tools-unavailable nudge
// restating the demand; the retry's cost joins the summary row and no tool
// result is persisted — the nudge lives only inside the summarizer request.
func TestSummarizerToolCallGetsOneNudgeAndRetries(t *testing.T) {
	ctx := context.Background()
	llm := &compactionMockLLM{
		contextWindow: 32000,
		chat: func(call int, _ string) (*llmwire.Response, error) {
			if call == 1 {
				return &llmwire.Response{
					FinishType: llmwire.FinishToolCalls,
					ToolCalls:  []llmwire.ToolCall{{ID: "c1", Name: "read"}},
					Usage:      &llmwire.MessageUsage{PromptTokens: 10, CompletionTokens: 5},
				}, nil
			}

			return &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}, nil
		},
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(32000))

	ok, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 2, llm.callCount, "one nudge, one retry, no more")

	// The nudge is an in-request role-tool reply to the attempted call.
	lastCall := llm.lastMessages
	toolReply := lastCall[len(lastCall)-1]
	assert.Equal(t, llmwire.RoleTool, toolReply.Role)
	assert.Equal(t, "c1", toolReply.ToolCallID)
	assert.Contains(t, toolReply.Content, "TOOLS ARE UNAVAILABLE")

	summary := findSummaryRow(t, s.ms.getMessages())
	assert.Contains(t, summary.Content, validSummary)
}

// No tail survives a cut through an open group, so an unanswered tool_use inside
// the tail would be unanswerable. The guard refuses before anything is written.
func TestCompactRefusesWhileAnExternalCallIsPending(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)
	s.stagedCalls = map[string]string{"c9": tool.IDTask}

	before := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		{
			Role:      llmwire.RoleAssistant,
			Content:   "spawning",
			ToolCalls: []llmwire.ToolCall{{ID: "c9", Name: tool.IDTask}},
		},
	}
	setTestMessages(s, before)

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount, "no summarization request may go out")
	assert.Equal(t, before, s.ms.getMessages(), "the transcript is untouched")
}

// The same guard covers ordinary tool calls of the current turn that have not
// been executed yet: compaction would erase the call the loop is about to run.
func TestCompactRefusesWhileOrdinaryToolWorkIsPending(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	before := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		compactionAssistantCall("c2", "unexecuted"),
	}
	setTestMessages(s, before)

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionPendingCall)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount)
	assert.Equal(t, before, s.ms.getMessages())
}

// A tool_use abandoned by a later user turn has no external owner and is
// deliberately not protected — the repair policy stubs it in the head.
func TestCompactProceedsWithAnAbandonedToolCall(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 200000,
	}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "interrupted"),
		compactionUserMessage("stop, do something else"),
		compactionAssistantCall("c2", "settled work"),
		compactionToolResult("c2", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.NoError(t, err)
	assert.True(t, compacted)
}

// A header that clears the trigger on its own makes compaction an endless
// grinder: say so instead of summarizing forever.
func TestCompactRefusesWhenTheHeaderAloneExceedsTheThreshold(t *testing.T) {
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32000,
	}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 120000)},
		compactionUserMessage("the task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.ErrorIs(t, err, errCompactionHeaderTooLarge)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount, "no LLM call is worth making")
}

// The system prompt rides along on every request, so a header that only fits
// without it does not fit at all.
func TestHeaderCheckCountsTheSystemPrompt(t *testing.T) {
	llm := &compactionMockLLM{contextWindow: 32000}
	s := newCompactionTestSvc(llm)

	// 20000 tokens of header: under the 27200 cutoff alone, over it with a
	// 10000-token system prompt.
	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + strings.Repeat("p", 80000)},
		compactionUserMessage("the task"),
	})

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	assert.True(t, s.headerFitsLocked(2))

	s.prompt = sessionprompt.NewBuilder(strings.Repeat("s", 40000), "")
	assert.False(t, s.headerFitsLocked(2))
}

// The summarizer call receives the ordinary full output reserve: the complement
// of the context input fraction, not a fixed summary-length target.
func TestSummarizationRequestCarriesTheFullOutputReserve(t *testing.T) {
	const window = 200000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		compactionAssistantCall("c1", "work"),
		compactionToolResult("c1", "result"),
		compactionUserMessage("recent note"),
	})

	_, err := s.compact(t.Context(), nil)
	require.NoError(t, err)

	assert.Equal(t, int((1-llmwire.ContextInputFraction)*float64(window)), llm.lastOptions.MaxTokens)
}

// A header carrying tool protocol fields can never pair once retained.
func TestCompactRefusesAHeaderWithToolProtocolFields(t *testing.T) {
	llm := &compactionMockLLM{contextWindow: 200000}
	s := newCompactionTestSvc(llm)

	setTestMessages(s, []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleAssistant, ToolCalls: []llmwire.ToolCall{{ID: "c1", Name: "read"}}},
		compactionToolResult("c1", "result"),
	})

	compacted, err := s.compact(t.Context(), nil)

	require.Error(t, err)
	assert.False(t, compacted)
	assert.Zero(t, llm.callCount)
}

// TestCompactionProtocolModel drives every sequence of commands up to a bounded
// length through both the reference model and the real session, asserting the
// invariant the plan states: compaction never runs while a call is pending, and
// a queued /compact is never silently dropped.
func TestCompactionProtocolModel(t *testing.T) {
	const depth = 4

	sequences := compactionSequences(depth)
	require.NotEmpty(t, sequences)

	for _, sequence := range sequences {
		t.Run(sequenceName(sequence), func(t *testing.T) {
			runCompactionSequence(t, sequence)
		})
	}
}

// The selected tail is the existing rows verbatim: same row IDs, same bytes, same
// order — never copies.
func TestCheckpointRetainsTheTailVerbatim(t *testing.T) {
	const window = 32000

	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.store = store
	s.ms = newMessageStore(store, 1)

	original := oversizedTranscript(window)
	for i := range original {
		message := original[i]
		require.NoError(t, appendTestMessage(ctx, s.ms, &message))
	}
	before := s.ms.getMessages()
	beforeRowIDs := slices.Clone(s.ms.rowIDs)

	require.NoError(t, s.compactIfNeeded(ctx, window))

	after := s.ms.getMessages()
	afterRowIDs := slices.Clone(s.ms.rowIDs)

	// The tail is a suffix of the original: find the marked summary row and
	// compare everything after it byte-for-byte, row IDs included.
	splitAt := -1

	for i, m := range after {
		if isMarkedSummary(m.Content) {
			splitAt = i + 1

			break
		}
	}

	require.Positive(t, splitAt, "a checkpoint was committed")
	require.Less(t, splitAt, len(after), "the tail is non-empty at this window size")

	for i := splitAt; i < len(after); i++ {
		originalOffset := len(before) - (len(after) - i)
		require.GreaterOrEqual(t, originalOffset, splitAt)

		assert.Equal(t, before[originalOffset], after[i],
			"tail row %d is the original row byte-for-byte", i)
		assert.Equal(t, beforeRowIDs[originalOffset], afterRowIDs[i],
			"tail row %d keeps its durable identity", i)
	}
}

// Restart after one and two checkpoints derives the exact prior summary from the
// marked message itself — no session metadata is involved.
func TestRestartDerivesTheAnchorFromTheMarkedSummaryRow(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	setTestMessages(s, oversizedTranscript(window))

	require.NoError(t, s.compactIfNeeded(context.Background(), window))

	reloaded := newCompactionTestSvc(llm)
	setTestMessages(reloaded, s.ms.getMessages())

	cp := parseCheckpointPrefix(reloaded.ms.getMessages(), compactionHeaderSize(reloaded.ms.getMessages()))
	require.NotEqual(t, -1, cp.summaryRowIdx, "the marked summary row follows the header after a reload")
	assert.Equal(t, validSummary, cp.prevSummary, "the anchor is the extracted model text")

	// A malformed wrapper in the scaffolding position is ordinary history: it
	// neither becomes an anchor nor hides the raw rows behind it.
	tampered := s.ms.getMessages()
	tampered[2] = compactionUserMessage(compactionMarkOpen + "\n\nbroken checkpoint without a close")

	cp = parseCheckpointPrefix(tampered, compactionHeaderSize(tampered))
	assert.Equal(t, -1, cp.summaryRowIdx, "an incomplete wrapper is ordinary history")
}

// After the winning completion CAS, a producer row committed outside the
// compaction snapshot loads after the selected tail, whichever reload runs
// first — and exactly once.
func TestOutsideSnapshotCompletionLoadsAfterTheTailInBothReloadOrders(t *testing.T) {
	const window = 32000

	ctx := context.Background()

	for _, order := range []string{"completion-reload-then-loop-reload", "loop-reload-then-completion-reload"} {
		t.Run(order, func(t *testing.T) {
			store := &compactionRecordingStore{nextID: 1}
			llm := &compactionMockLLM{
				response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
				contextWindow: window,
			}
			s := newCompactionTestSvc(llm)
			s.store = store
			s.ms = newMessageStore(store, 1)

			// Persist the transcript so the store owns the rows the loop reads.
			for i := range oversizedTranscript(window) {
				message := oversizedTranscript(window)[i]
				require.NoError(t, appendTestMessage(ctx, s.ms, &message))
			}

			require.NoError(t, s.compactIfNeeded(ctx, window))
			require.NoError(t, s.ms.reloadMessages(ctx))
			afterCompaction := s.ms.getMessages()
			afterCompactionRowIDs := slices.Clone(s.ms.rowIDs)
			require.True(t, hasSummaryRow(afterCompaction))

			// The completion is committed by the store while the parent may hold
			// a stale candidate: insertMessageWith leaves its position NULL, so
			// LoadActiveMessages sorts it after the positioned tail.
			callID := fmt.Sprintf("child-%s", order)
			asstMem := mkStoredAssistant(callID)
			resultMem := mkStoredResult(callID)
			asstStored, err := storedMessage(&asstMem)
			require.NoError(t, err)
			resultStored, err := storedMessage(&resultMem)
			require.NoError(t, err)
			asstID, err := store.appendRow(ctx, 1, asstStored)
			require.NoError(t, err)
			resultID, err := store.appendRow(ctx, 1, resultStored)
			require.NoError(t, err)

			if order == "completion-reload-then-loop-reload" {
				require.NoError(t, s.ms.reloadMessages(ctx))
				require.NoError(t, s.ms.reloadMessages(ctx))
			} else {
				require.NoError(t, s.ms.reloadMessages(ctx))
			}

			final := s.ms.getMessages()
			finalRowIDs := slices.Clone(s.ms.rowIDs)

			assert.Equal(t, 1, countRowID(finalRowIDs, asstID), "one in-memory copy of the completion call")
			assert.Equal(t, 1, countRowID(finalRowIDs, resultID), "one in-memory copy of the completion result")

			for _, rowID := range afterCompactionRowIDs {
				assert.Equal(t, 1, countRowID(finalRowIDs, rowID),
					"checkpoint row %d appears exactly once after either reload order", rowID)
			}

			// The outside-snapshot rows sort after the selected tail.
			summaryAt := -1
			for i, m := range final {
				if isMarkedSummary(m.Content) {
					summaryAt = i
				}
			}

			require.Positive(t, summaryAt)
			assert.Greater(t, indexWithRowID(finalRowIDs, asstID), summaryAt,
				"the completion lands after the marked summary and the tail")

			// Both reload orders reach the same projection.
			if order == "loop-reload-then-completion-reload" {
				require.NoError(t, s.ms.reloadMessages(ctx))
				assert.Equal(t, final, s.ms.getMessages())
				assert.Equal(t, finalRowIDs, slices.Clone(s.ms.rowIDs))
			}
		})
	}
}

// The compaction pin keeps the pending candidate and its nudge verbatim: the
// pinned split stays at or before the candidate, while the unpinned split
// would have summarized it away.
func TestCompactionPinRetainsCandidateAndNudge(t *testing.T) {
	messages, pin := pinFixture(t)
	const window = 80_000

	cp := parseCheckpointPrefix(messages, compactionHeaderSize(messages))

	unpinned, ok := selectCheckpointSplit(messages, cp, 0, window, 0)
	require.True(t, ok, "the fixture must compact without a pin")
	assert.Greater(t, unpinned, pin, "the unpinned split must cross the candidate")

	pinned, ok := selectCheckpointSplit(messages, cp, 0, window, pin)
	require.True(t, ok, "the pinned transcript must still compact")
	assert.LessOrEqual(t, pinned, pin, "the pinned split must not cross the candidate")

	tail := messages[pinned:]
	require.GreaterOrEqual(t, len(tail), 2, "candidate and nudge stay verbatim")
	assert.Contains(t, tail[len(tail)-2].Content, "first answer", "candidate stays verbatim")
	assert.Contains(t, tail[len(tail)-1].Content, "second look", "nudge stays verbatim")

	zero, ok := selectCheckpointSplit(messages, cp, 0, window, 0)
	require.True(t, ok)
	assert.Equal(t, unpinned, zero, "pin=0 must equal unpinned behavior")
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
	setTestMessages(s, msgs)

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
// byte-identically between the summary and the tail; the summarizer input may
// carry the ordinary activation rows, but the committed transcript keeps
// exactly one byte-identical envelope.
func TestCurrentSkillInHeadIsReattachedByteIdentically(t *testing.T) {
	const window = 200000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)

	rendered := skillMessage(t, "review", "Review carefully.")

	setTestMessages(s, []llmwire.Message{
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

	// The replayed input is the ordinary repaired prefix plus the final
	// instruction; the committed transcript, not the summarizer request, owns
	// the single envelope.
	require.Len(t, llm.lastMessages, 7)
}

// A latest skill invoked alongside other calls keeps the committed transcript
// provider-valid and exactly one envelope: the ordinary repaired projection is
// committed as-is, so sibling calls of a mixed response stay valid pairs.
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

	setTestMessages(s, msgs)

	ok, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)

	require.NoError(t, llmclient.ValidateToolPairing(s.ms.getMessages()), "remaining calls stay valid pairs")
	skills := renderedSkills(s.ms.getMessages())
	require.Len(t, skills, 1, "exactly one envelope survives")
}

func TestCompactIfNeeded_BelowThreshold_NoCompaction(t *testing.T) {
	mockLLM := &compactionMockLLM{response: &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop}}
	s := newCompactionTestSvc(mockLLM)

	require.NoError(t, appendTestUser(context.Background(), s.ms, "Hello"))
	require.NoError(t, appendTestAssistant(context.Background(), s.ms, &llmwire.Response{Text: "Hi"}))
	require.NoError(t, appendTestUser(context.Background(), s.ms, "How are you?"))

	err := s.compactIfNeeded(context.Background(), 100000)
	require.NoError(t, err)
	assert.Equal(t, 0, mockLLM.callCount)
	assert.Len(t, s.ms.getMessages(), 3)
}

func TestCompactIfNeeded_AboveThreshold_Compacts(t *testing.T) {
	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 32000,
	}
	s := newCompactionTestSvc(mockLLM)
	setTestMessages(s, oversizedTranscript(32000))

	err := s.compactIfNeeded(context.Background(), 32000)
	require.NoError(t, err)
	assert.Equal(t, 1, mockLLM.callCount)

	// Header -> marked summary -> verbatim tail (at least one round survives).
	messages := s.ms.getMessages()
	require.Greater(t, len(messages), 3, "a verbatim tail survives the checkpoint")

	assert.Equal(t, llmwire.RoleSystem, messages[0].Role)
	assert.Equal(t, llmwire.RoleUser, messages[1].Role)
	assert.Equal(t, llmwire.RoleUser, messages[2].Role)
	assert.True(t, isMarkedSummary(messages[2].Content), "the marked summary follows the header")
	for _, m := range messages[3:] {
		assert.NotEqual(t, llmwire.RoleUser, m.Role, "only the summary row sits between header and tail")
	}
}

// A summarization that never produced an accepted summary must leave the
// conversation exactly as it was.
func TestCompactIfNeeded_SummaryFailure_KeepsTheConversation(t *testing.T) {
	mockLLM := &compactionMockLLM{err: errors.New("summary generation failed")}
	s := newCompactionTestSvc(mockLLM)

	setTestMessages(s, oversizedTranscript(32000))

	before := s.ms.getMessages()

	err := s.compactIfNeeded(context.Background(), 32000)
	require.Error(t, err)

	after := s.ms.getMessages()
	require.Len(t, after, len(before), "no summarizing rewrite without a summary")
	for i := range before {
		assert.Equal(t, before[i].Role, after[i].Role)
		assert.Equal(t, before[i].Content, after[i].Content)
	}
}

func TestCompactionAttributesOwnCostToSummaryRow(t *testing.T) {
	ctx := context.Background()
	store := &compactionRecordingStore{nextID: 1}
	mockLLM := &compactionMockLLM{
		chat: func(_ int, _ string) (*llmwire.Response, error) {
			return &llmwire.Response{
				Text:       validSummary,
				FinishType: llmwire.FinishStop,
				CostUSD:    0.01,
				Usage:      &llmwire.MessageUsage{PromptTokens: 100, CompletionTokens: 20},
			}, nil
		},
	}
	s := newCompactionTestSvc(mockLLM)
	s.store = store
	s.ms = newMessageStore(store, 1)

	// Costed rounds big enough to cross the trigger.
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
	}
	for estimateTokens(msgs) < compactionCutoff(32000)+10000 {
		id := fmt.Sprintf("cost-%d", len(msgs))
		msgs = append(msgs, llmwire.Message{
			Role: llmwire.RoleAssistant, Content: "step", CostUSD: 0.5,
			ToolCalls: []llmwire.ToolCall{{ID: id, Name: "read"}},
		})
		msgs = append(msgs, llmwire.Message{
			Role: llmwire.RoleTool, Content: strings.Repeat("t", 3600), ToolCallID: id, ToolName: "read",
		})
	}

	for i := range msgs {
		message := msgs[i]
		require.NoError(t, appendTestMessage(ctx, s.ms, &message))
	}

	require.NoError(t, s.compactIfNeeded(ctx, 32000))
	require.Equal(t, 1, mockLLM.callCount, "one summarization call")

	summary := findStoredSummary(t, store)
	assert.InDelta(t, 0.01, summary.CostUSD, 1e-9,
		"summary row carries the summed compaction cost")

	var usage llmwire.MessageUsage
	require.NoError(t, json.Unmarshal(summary.Usage, &usage))
	assert.Equal(t, 100, usage.PromptTokens)
	assert.Equal(t, 20, usage.CompletionTokens)

	var originalCost float64

	for _, m := range store.messages {
		if m.Role == llmwire.RoleAssistant && m.CompactedAt != nil {
			originalCost += m.CostUSD
		}
	}

	assert.Positive(t, originalCost, "compacted originals keep their own cost")
	var compactedAssistants int

	for _, m := range store.messages {
		if m.Role == llmwire.RoleAssistant && m.CompactedAt != nil {
			compactedAssistants++
		}
	}

	assert.InDelta(t, 0.5*float64(compactedAssistants), originalCost, 1e-9,
		"each compacted original keeps its own cost, counted exactly once")
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

// The whole automatic path: byte pressure triggers, the checkpoint relieves
// both axes, and a verbatim tail survives under the ceilings.
func TestImagePressureCompactionRelievesTheByteAxis(t *testing.T) {
	ctx := context.Background()

	mockLLM := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: 1_048_576,
	}
	s := newCompactionTestSvc(mockLLM)

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "scan the archive"},
	}
	for i := range 12 {
		messages = append(messages,
			llmwire.Message{
				Role:      llmwire.RoleAssistant,
				Content:   "reading",
				ToolCalls: []llmwire.ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "read"}},
			},
			llmwire.Message{
				Role: llmwire.RoleTool, Content: "page", ToolCallID: fmt.Sprintf("c%d", i), ToolName: "read",
				Images: []llmwire.ImageRef{{
					Path: fmt.Sprintf("/tmp/p%d.png", i), Mime: llmwire.MimeImagePng, Size: 2_290_000,
					Width: 1615, Height: 2193,
				}},
			},
		)
	}
	setTestMessages(s, messages)

	require.True(t, s.shouldCompact(1_048_576), "byte pressure fires the trigger")

	ok, err := s.compact(ctx, nil)
	require.NoError(t, err)
	require.True(t, ok)

	after := s.ms.getMessages()
	afterBytes, afterCount := imagePressure(after)
	assert.LessOrEqual(t, afterBytes, int64(imageBytesHighWater), "the checkpoint relieves the byte axis")
	assert.LessOrEqual(t, afterCount, imageCountHighWater)
	assert.False(t, s.shouldCompact(1_048_576), "the session is out of pressure")

	summaryIdx := -1
	for i, m := range after {
		if isMarkedSummary(m.Content) {
			summaryIdx = i
			break
		}
	}
	require.Positive(t, summaryIdx, "a summary row was written")

	// The tail is verbatim, non-empty, and holds the remaining pixels.
	tail := after[summaryIdx+1:]
	assert.NotEmpty(t, tail)
	tailBytes, _ := imagePressure(tail)
	assert.LessOrEqual(t, tailBytes, int64(imageBytesLowWater))
}
