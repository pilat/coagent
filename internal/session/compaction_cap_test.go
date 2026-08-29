package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// A compliant summarizer converges on a 32k window: the projection right after
// the compaction is back under the trigger, and the counter resets.
func TestAutoCompactionConvergesOnASmallWindow(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(oversizedTranscript(window))

	require.True(t, s.shouldCompact(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.applyContextEvents(context.Background())

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
	s.ms.setMessages(oversizedTranscript(window))
	before := s.ms.getMessages()

	var notes []string
	r := contextEventRunner(s, &notes)
	r.applyContextEvents(context.Background())

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
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)

	for range compactionAttemptCap {
		r.applyContextEvents(context.Background())
	}

	require.True(t, r.autoCompactionOff)
	assert.Equal(t, 1, countNotes(notes, compactionNotConvergingNotice))

	callsAtCap := llm.callCount
	r.applyContextEvents(context.Background())
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
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap
	r.autoCompactionOff = true

	s.RequestCompaction()
	r.applyContextEvents(context.Background())

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
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap - 1

	r.applyContextEvents(context.Background())

	assert.Zero(t, r.compactionFailures)
	assert.False(t, r.autoCompactionOff)
}
