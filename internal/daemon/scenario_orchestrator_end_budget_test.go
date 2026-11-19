package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/orchestrator"
)

func TestOrchestratorStopRefusesArmedBudget(t *testing.T) {
	// Delegated stop cannot shed an armed budget; parked budgets survive stop and kill.
	h := orchestratorProjectHarness(t)
	caller := modeRoot(t, h)
	armed := spawnEndTarget(t, h, caller, "queued budgeted work")
	limit := 1.0
	_, err := h.store.Arm(h.ctx, budget.Mutation{
		RootSessionID: armed, ToolCallID: "end-budget", CostLimitUSD: &limit, Receipt: "armed",
	})
	require.NoError(t, err)
	before := endRowCounts(t, h, armed)
	_, err = endToolResult(t, h, caller, armed, orchestrator.IDStopSession)
	require.EqualError(t, err, fmt.Sprintf(
		"session #%d runs under a budget the human set; leave it to the human, or end it for good with kill_session",
		armed,
	))
	assert.Equal(t, before, endRowCounts(t, h, armed))
	record, err := h.store.Get(h.ctx, armed)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	result, err := endToolResult(t, h, caller, armed, orchestrator.IDKillSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Killed session #%d.", armed), result.Output)

	parked := spawnEndTarget(t, h, caller, "queued parked work")
	_, err = endToolResult(t, h, caller, parked, orchestrator.IDStopSession)
	require.NoError(t, err)
	fireOrchestratorBudget(t, h, parked)
	before = endRowCounts(t, h, parked)
	result, err = endToolResult(t, h, caller, parked, orchestrator.IDStopSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Session #%d is parked; nothing to stop.", parked), result.Output)
	assert.Equal(t, before, endRowCounts(t, h, parked))
	record, err = h.store.Get(h.ctx, parked)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	result, err = endToolResult(t, h, caller, parked, orchestrator.IDKillSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Killed session #%d.", parked), result.Output)
	record, err = h.store.Get(h.ctx, parked)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
}
