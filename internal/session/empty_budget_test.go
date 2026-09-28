package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestSixthEmptyResponseBudgetCrossingParksWithoutEmptyTerminalNotice(t *testing.T) {
	agent, db, store, sessionID, _ := newDispositionLoop(t)
	armCheckpointBudget(t, store, sessionID)
	require.NoError(t, agent.ms.reloadMessages(t.Context()))
	responses := make([]*llmwire.Response, emptyResponseBreakThreshold)
	for i := range responses {
		responses[i] = &llmwire.Response{FinishType: llmwire.FinishStop}
	}
	responses[len(responses)-1].CostUSD = 1
	client := &loopScriptLLM{responses: responses}
	model := newTestModelRuntime(client, store, sessionID)
	gate := &checkpointStoreGate{store: store.(sessionstore.BudgetCompactionStore), sessionID: sessionID}
	turns := newToolTurns(agent.registry, model, agent.ms, testProgressBoundary(agent.boundary))
	agent.models = model
	agent.turns = turns
	agent.budgetGate = gate
	agent.contexts = newCheckpointOwner(
		agent.ms, model, agent.prompt, turns, agent.transcript(),
		store, gate, store, agent.boundary,
		nil, nil, nil,
		checkpointOptions{id: sessionID, outputEnabled: true},
	)
	notifier := &loopNotifier{}
	result, err := runTestLoop(t.Context(), t, agent, loopOptions{Notify: notifier.fn}, iterationGuard(7))
	require.NoError(t, err)
	assert.True(t, result.Suspended)
	assert.Empty(t, result.FinalResponse)
	assert.False(t, result.TerminalStateCommitted)
	assert.Equal(t, emptyResponseBreakThreshold, client.calls)
	assert.Zero(t, notifier.countWith(sessionstore.EmptyStopTerminalNotice(emptyResponseBreakThreshold)))
	var emptyNotices, checkpoints int
	require.NoError(t, db.QueryRowContext(t.Context(),
		"SELECT SUM(content = ?), SUM(source_key LIKE 'budget:%:checkpoint') FROM session_outbox",
		sessionstore.EmptyStopTerminalNotice(emptyResponseBreakThreshold)).Scan(&emptyNotices, &checkpoints))
	assert.Zero(t, emptyNotices)
	assert.Equal(t, 1, checkpoints)
}
