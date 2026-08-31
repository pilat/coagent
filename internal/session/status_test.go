package session

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// statusStubStore returns fixed lifetime totals for buildSessionStatus tests.
type statusStubStore struct {
	mockSessionStore
	in, out   int
	cost      float64
	subagents int
}

func (m *statusStubStore) GetSessionTreeUsage(context.Context, int64) (int, int, float64, error) {
	return m.in, m.out, m.cost, nil
}

func (m *statusStubStore) GetChildSessionStats(context.Context, int64) (int, int, error) {
	return m.subagents, 0, nil
}

// TestBuildSessionStatus_LifetimeFromTreeOccupancyFromProjection verifies lifetime
// comes from the DB tree-sum (survives compaction, keeps climbing) while occupancy
// is the compaction trigger's own projection, denominated by the same window source.
func TestBuildSessionStatus_LifetimeFromTreeOccupancyFromProjection(t *testing.T) {
	mockLLM := &compactionMockLLM{contextWindow: 200000}
	store := &statusStubStore{in: 500000, out: 50000, cost: 12.34, subagents: 2}

	s := newCompactionTestSvc(mockLLM)
	s.store = store
	s.rootID = 1
	s.model = "test-model"
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: "big turn"},
	})
	s.recordContextBaseline(context.Background(), 150000, 2, s.modelGeneration())

	st := s.buildSessionStatus(context.Background())

	assert.Equal(t, 500000, st.LifetimeIn)
	assert.Equal(t, 50000, st.LifetimeOut)
	assert.InDelta(t, 12.34, st.LifetimeCost, 1e-9)
	assert.Equal(t, 150000, st.ContextUsed, "occupancy is the measured baseline plus its (empty) tail")
	assert.False(t, st.ContextIsEst, "a provider measurement backs it")
	assert.Equal(t, 200000, st.ContextMax, "denominator is s.contextWindow(), not a literal")
	assert.Equal(t, 2, st.SubagentCount)

	// A new message after the measurement is counted as a len/4 delta on top.
	require.NoError(t, s.ms.addUserMessage(context.Background(), strings.Repeat("x", 4000)))

	grown := s.buildSessionStatus(context.Background())
	assert.Equal(t, 151000, grown.ContextUsed, "baseline plus the tail estimate")
	assert.False(t, grown.ContextIsEst)
}

// After a compaction the baseline is gone, so /status falls back to a whole-
// transcript estimate — marked as one, and never 0%.
func TestBuildSessionStatus_AfterCompactionEstimatesAndIsNotZero(t *testing.T) {
	mockLLM := &compactionMockLLM{contextWindow: 200000}

	s := newCompactionTestSvc(mockLLM)
	s.store = &statusStubStore{in: 600000, out: 50000, cost: 15.0}
	s.rootID = 1
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleUser, Content: "[CONTEXT SUMMARY - previous work condensed] " + strings.Repeat("b", 4000)},
	})
	s.resetContextBaseline()

	st := s.buildSessionStatus(context.Background())

	assert.True(t, st.ContextIsEst, "no measurement survives a compaction")
	assert.Positive(t, st.ContextUsed, "0% right after a compaction would lie in the dangerous direction")
	assert.Contains(t, renderStatus(st), "~", "an estimate is visibly marked")
}

func TestRenderStatus_BandsAndBar(t *testing.T) {
	// Fresh session (no assistant turn) → 0%, green, empty bar.
	fresh := renderStatus(sessionStatus{Model: "m", ContextMax: 200000})
	assert.Contains(t, fresh, "🟢")
	assert.Contains(t, fresh, "0%")
	assert.Contains(t, fresh, "`░░░░░░░░░░`")

	// 90% → red + compacting soon; round(90/10)=9 filled cells.
	red := renderStatus(sessionStatus{Model: "m", ContextUsed: 180000, ContextMax: 200000})
	assert.Contains(t, red, "🔴")
	assert.Contains(t, red, "compacting soon")
	assert.Contains(t, red, "`█████████░`")
	assert.Contains(t, red, "90%")

	// 75% → yellow, no compacting-soon tail; round(75/10)=8 filled cells.
	yellow := renderStatus(sessionStatus{Model: "m", ContextUsed: 150000, ContextMax: 200000})
	assert.Contains(t, yellow, "🟡")
	assert.NotContains(t, yellow, "compacting soon")
	assert.Contains(t, yellow, "`████████░░`")

	// Exactly 85% is the red cut (compactionFraction).
	edge := renderStatus(sessionStatus{Model: "m", ContextUsed: 170000, ContextMax: 200000})
	assert.Contains(t, edge, "🔴")
}

func TestRenderStatus_CostHeadlineAndTokenSuffixes(t *testing.T) {
	out := renderStatus(sessionStatus{
		Model: "m", LifetimeCost: 12.3456, LifetimeIn: 2_500_000, LifetimeOut: 40000, ContextMax: 200000,
	})

	assert.Contains(t, out, "**$12.35**", "cost is bold, 2 decimals")
	assert.Contains(t, out, "all-in")
	assert.Contains(t, out, "2.5M in")
	assert.Contains(t, out, "40.0k out")
}
