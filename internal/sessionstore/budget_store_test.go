package sessionstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

func TestBudgetStore_ArmFireAndReplayAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, root.ID, InputSourceUser, "/budget two dollars")
	require.NoError(t, err)
	_, _, err = acceptActivation(ctx, store, input.ID, "/budget two dollars\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 2.0
	mutation := budget.Mutation{
		RootSessionID: root.ID, InputID: input.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "call-budget",
		CostLimitUSD: &limit, Receipt: "Budget armed: $2.000000 additional persisted cost",
	}

	armed, err := store.Arm(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, armed.State)

	replayed, err := store.Arm(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, armed.Generation, replayed.Generation)
	var receipts int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = 'tool:call-budget:direct:0'`, root.ID).Scan(&receipts))
	assert.Equal(t, 1, receipts)

	fired, checkpoint, err := store.FireBudget(ctx, root.ID, armed.Generation, "cost", 2.25,
		"Budget checkpoint reached (cost).")
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, fired.State)
	assert.Positive(t, checkpoint.OutputID)

	duplicate, duplicateCheckpoint, err := store.FireBudget(ctx, root.ID, armed.Generation, "cost", 2.25,
		"Budget checkpoint reached (cost).")
	require.NoError(t, err)
	assert.Equal(t, fired.FiredAt, duplicate.FiredAt)
	assert.True(t, duplicateCheckpoint.Existing)

	var releases bool
	require.NoError(t, db.QueryRowContext(ctx, `SELECT releases_input FROM session_outbox WHERE id = ?`,
		checkpoint.OutputID).Scan(&releases))
	assert.True(t, releases)
}

func TestBudgetStore_CrossingResponseCommitsUsageNonExecutionAndCheckpointTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli:main"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, root.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = acceptActivation(ctx, store, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 0.5
	_, err = store.Arm(ctx, budget.Mutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)

	result, err := store.Commit(
		ctx,
		Commit{SessionID: root.ID, RootID: root.ID, ObserveBudget: true, Messages: []*transcript.Message{{
			Role: "assistant", Content: "checkpoint summary", CostUSD: 0.75,
			ToolCalls: json.RawMessage(`[{"ID":"danger","Name":"bash","Arguments":{"command":"false"}}]`),
		}}},
	)
	require.NoError(t, err)
	require.True(t, result.BudgetFired)
	late, err := store.Commit(
		ctx,
		Commit{SessionID: root.ID, RootID: root.ID, ObserveBudget: true, Messages: []*transcript.Message{{
			Role: "assistant", Content: "late parallel response", CostUSD: 0.1,
			ToolCalls: json.RawMessage(`[{"ID":"late","Name":"bash","Arguments":{}}]`),
		}}},
	)
	require.NoError(t, err)
	assert.True(t, late.BudgetFired)

	var assistantCount, resultCount, checkpointCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'assistant' AND cost_usd = 0.75`, root.ID).Scan(&assistantCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'danger'
			AND content = ?`, root.ID, budgetToolNotExecuted).Scan(&resultCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = ? AND releases_input = 1`,
		root.ID, "budget:1:checkpoint").Scan(&checkpointCount))
	assert.Equal(t, 1, assistantCount)
	assert.Equal(t, 1, resultCount)
	assert.Equal(t, 1, checkpointCount)
	var lateResults int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'late'`, root.ID).Scan(&lateResults))
	assert.Equal(t, 1, lateResults)
}

func TestBudgetStore_RejectsCrossSessionGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	first, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "one"})
	require.NoError(t, err)
	second, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "two"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, first.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = acceptActivation(ctx, store, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 1.0
	_, err = store.Arm(ctx, budget.Mutation{
		RootSessionID: second.ID, InputID: input.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "wrong", CostLimitUSD: &limit,
		Receipt: "Budget armed: $1.000000 additional persisted cost",
	})
	require.ErrorIs(t, err, budget.ErrConflict)
}
