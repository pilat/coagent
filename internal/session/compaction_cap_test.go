package session

import (
	"context"
	"fmt"
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
		response:      &llmwire.Response{Text: validSummary},
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

// The protection may not rest on the model's diligence: a brief far larger than
// the cap still leaves the session over the threshold, and that counts as a
// failed attempt even though compaction "succeeded".
func TestAutoCompactionCountsASuccessThatDidNotRelievePressure(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response: &llmwire.Response{
			Text: validSummary + "\n" + strings.Repeat("b", window*4),
		},
		contextWindow: window,
	}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.applyContextEvents(context.Background())

	assert.True(t, notesContain(notes, "✅ Context compacted"))
	assert.True(t, s.shouldCompact(window), "the oversized brief kept it over the trigger")
	assert.Equal(t, 1, r.compactionFailures)
}

// Three attempts that free nothing and the automatic path goes quiet for the
// rest of the activation, with one notice — explicit requests still run.
func TestAutoCompactionStopsAfterThreeFruitlessAttempts(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{
		response: &llmwire.Response{
			Text: validSummary + "\n" + strings.Repeat("b", window*4),
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

	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: window}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap
	r.autoCompactionOff = true

	s.RequestCompaction(2)
	r.applyContextEvents(context.Background())

	assert.Equal(t, 1, llm.callCount)
	assert.True(t, notesContain(notes, "✅ Context compacted"))
	assert.Equal(t, compactionAttemptCap, r.compactionFailures, "an explicit run neither counts nor clears")
}

// A successful, relieving compaction wipes the streak.
func TestAutoCompactionResetsTheCounterOnRelief(t *testing.T) {
	const window = 32000

	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: window}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(oversizedTranscript(window))

	var notes []string
	r := contextEventRunner(s, &notes)
	r.compactionFailures = compactionAttemptCap - 1

	r.applyContextEvents(context.Background())

	assert.Zero(t, r.compactionFailures)
	assert.False(t, r.autoCompactionOff)
}

// The counter lives on the runner, so a new activation starts clean — that is
// the "conditions changed" boundary, not a leak.
func TestAutoCompactionCounterIsPerActivation(t *testing.T) {
	s := newCompactionTestSvc(&compactionMockLLM{contextWindow: 32000})

	var notes []string
	first := contextEventRunner(s, &notes)
	first.compactionFailures = compactionAttemptCap
	first.autoCompactionOff = true

	second := contextEventRunner(s, &notes)

	assert.Zero(t, second.compactionFailures)
	assert.False(t, second.autoCompactionOff)
}

// oversizedTranscript builds a header plus ten rounds that put the projection
// over the trigger. Clearing drops the four oldest bodies, so what the
// summarizer actually carries still fits window − compactionOutputReserve.
func oversizedTranscript(window int) []llmwire.Message {
	msgs := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}

	body := compactionCutoff(window) / 9

	for i := range 10 {
		msgs = append(msgs, roundTokens(fmt.Sprintf("c%d", i), 10, body)...)
	}

	return msgs
}

func countNotes(notes []string, sub string) int {
	n := 0

	for _, note := range notes {
		if strings.Contains(note, sub) {
			n++
		}
	}

	return n
}
