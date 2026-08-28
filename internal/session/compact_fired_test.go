package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// Plan: "/compact on a fired root returns a persistent parked explanation
// without spending a model call." A generic compaction failure is a false
// claim — nothing failed, the tree is parked.
func TestSlashCompactOnFiredBudgetReturnsParkedExplanation(t *testing.T) {
	llm := &compactionMockLLM{response: &llmwire.Response{Text: validSummary}, contextWindow: 200000}
	s := newCompactionTestSvc(llm)
	s.ms.setMessages(loopRounds(10, 4000))
	s.budgetGate = &terminalBudgetGate{admitErr: ErrBudgetCheckpoint}

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	_, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	r.applyContextEvents(t.Context())

	assert.Zero(t, llm.callCount, "a parked root must not spend a summarization call")
	assert.False(t, notesContain(notes, "❌ Compaction failed"), "parking is not a failure")

	parked := notesContain(notes, "Budget checkpoint reached") && notesContain(notes, "parked")
	assert.True(t, parked, "the reply must explain the parked budget state, got %v", notes)
}
