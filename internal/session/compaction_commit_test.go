package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
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
	s.ms.setMessages(append(header, append(
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
	s.ms.setMessages(append(header, append(
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

type recordingBudgetGate struct {
	got   sessionstore.BudgetedCompaction
	ids   []int64
	fired bool
}

func (g *recordingBudgetGate) Admit(context.Context, time.Time) error { return nil }
func (g *recordingBudgetGate) Observe(context.Context) (bool, error)  { return false, nil }
func (g *recordingBudgetGate) PersistResponse(
	context.Context,
	*transcript.Message,
	string,
) (int64, bool, bool, error) {
	return 0, false, false, nil
}

func (g *recordingBudgetGate) PersistCompaction(
	_ context.Context,
	compaction sessionstore.BudgetedCompaction,
) ([]int64, bool, error) {
	g.got = compaction

	return g.ids, g.fired, nil
}

func budgetedSvc(t *testing.T, gate *recordingBudgetGate, llm *compactionMockLLM) *svc {
	t.Helper()

	s := newCompactionTestSvc(llm)
	s.budgetGate = gate
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleUser, Content: "raw one"},
		{Role: llmwire.RoleUser, Content: "raw two"},
	})
	s.ms.rowIDs = []int64{1, 2, 10, 11}

	return s
}

// A budgeted commit persists through the gate, stamps the returned row ids
// onto the projection in entry order and adopts the fired verdict. The
// verbatim tail keeps "raw two", so only the head row is marked compacted.
func TestBudgetedCompactionCommitStampsGateRowIDs(t *testing.T) {
	const window = 32000

	gate := &recordingBudgetGate{ids: []int64{50, 51, 52, 53}, fired: true}
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := budgetedSvc(t, gate, llm)

	ok, err := s.compact(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, ok)

	assert.Equal(t, []int64{10}, gate.got.CompactedIDs, "only head rows are marked compacted")
	assert.Zero(t, gate.got.InputID, "no command input behind an automatic compaction")
	assert.True(t, s.budgetFired)

	messages := s.ms.getMessages()
	rowIDs := s.ms.getRowIDs()
	require.Len(t, messages, 4, "header, summary and the verbatim tail")
	assert.Equal(t, int64(50), rowIDs[0])
	assert.Equal(t, int64(51), rowIDs[1])
	assert.Equal(t, int64(52), rowIDs[2], "the summary row carries the gate's id")
	assert.True(t, isMarkedSummary(messages[2].Content))
	assert.Equal(t, int64(53), rowIDs[3], "the gate stamps every projected row in entry order")
}

// A gate that returns the wrong number of row ids fails the whole commit.
func TestBudgetedCompactionCommitRejectsMismatchedRowIDs(t *testing.T) {
	const window = 32000

	gate := &recordingBudgetGate{ids: []int64{50, 51}}
	llm := &compactionMockLLM{
		response:      &llmwire.Response{Text: validSummary, FinishType: llmwire.FinishStop},
		contextWindow: window,
	}
	s := budgetedSvc(t, gate, llm)

	ok, err := s.compact(context.Background(), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "budgeted compaction returned 2 ids for 4 messages")
	assert.False(t, ok)
	assert.NotErrorIs(t, err, errCompactionNonRelieving)
}
