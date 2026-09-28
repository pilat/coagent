package session

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

var errCommittedReload = errors.New("injected post-commit transcript read failure")

type committedReloadFailureStore struct {
	sessionstore.Store
	failReload bool
}

type recordingParkBudgetGate struct {
	terminalBudgetGate
	fired []*sessionstore.BudgetRecord
}

func TestBudgetCrossingParksWhenPostCommitTranscriptReloadFails(t *testing.T) {
	s := newMockSvc(t, nil, "")
	store := s.store.(sessionstore.Store)
	armCheckpointBudget(t, store, s.id)

	wrapped := &committedReloadFailureStore{Store: store}
	s.store = wrapped
	s.ms.store = wrapped
	gate := &recordingParkBudgetGate{}
	s.budgetGate = gate
	probe := &countingTool{id: "probe"}
	s.registry.Register(probe)
	response := toolCallResponse("crossing-call", "probe")
	response.CostUSD = 1
	client := &loopScriptLLM{responses: []*llmwire.Response{response}}
	setRunTestModel(s, client)
	prepareDirectRun(t, s, "run probe")

	result, err := s.run(t.Context(), "run probe")
	require.NoError(t, err)
	require.True(t, result.Suspended)
	assert.Empty(t, result.ErrorNotice)
	assert.Zero(t, probe.runs.Load(), "the crossing response cannot execute its returned tool")
	assert.Equal(t, 1, client.calls)
	require.Len(t, gate.fired, 1, "the committed verdict must reach the park scheduler")
	assert.Equal(t, sessionstore.BudgetFired, gate.fired[0].State)
	assert.Equal(t, "requested", gate.fired[0].ParkPhase)

	record, err := store.GetSession(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusSuspended, record.Status)
	budget, err := store.GetBudget(t.Context(), s.id)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetFired, budget.State)
	assert.Equal(t, "requested", budget.ParkPhase)
	checkpoint, err := store.OutputBySourceKey(t.Context(), s.id, "budget:1:checkpoint")
	require.NoError(t, err)
	assert.Contains(t, checkpoint.Content, "Budget checkpoint reached (cost)")
	messages, err := store.LoadActiveMessages(t.Context(), s.id)
	require.NoError(t, err)
	var skipped int
	for _, message := range messages {
		if message.ToolCallID == "crossing-call" && message.Role == llmwire.RoleTool {
			skipped++
			assert.Equal(t, "Not executed because the budget checkpoint fired.", message.Content)
		}
	}
	assert.Equal(t, 1, skipped)
	assert.False(t, wrapped.failReload, "the fault must occur after the response commit")
}

func (s *committedReloadFailureStore) CommitAcceptedResponseDisposition(
	ctx context.Context,
	disposition sessionstore.AcceptedResponseDisposition,
) (*sessionstore.AcceptedResponseResult, error) {
	result, err := s.Store.CommitAcceptedResponseDisposition(ctx, disposition)
	if err == nil {
		s.failReload = true
	}
	return result, err
}

func (s *committedReloadFailureStore) LoadActiveMessages(
	ctx context.Context,
	sessionID int64,
) ([]*transcript.Message, error) {
	if s.failReload {
		s.failReload = false
		return nil, errCommittedReload
	}
	return s.Store.LoadActiveMessages(ctx, sessionID)
}

func (g *recordingParkBudgetGate) BudgetFired(record *sessionstore.BudgetRecord) {
	g.fired = append(g.fired, record)
}
