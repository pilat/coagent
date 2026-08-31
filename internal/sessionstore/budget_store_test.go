package sessionstore

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestBudgetStore_ArmFireAndReplayAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget two dollars")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget two dollars\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 2.0
	mutation := BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "call-budget",
		CostLimitUSD: &limit, Receipt: "Budget armed: $2.000000 additional persisted cost",
	}

	armed, receipt, err := store.ArmBudget(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, BudgetArmed, armed.State)
	assert.False(t, receipt.Existing)

	replayed, replayReceipt, err := store.ArmBudget(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, armed.Generation, replayed.Generation)
	assert.True(t, replayReceipt.Existing)

	fired, checkpoint, err := store.FireBudget(ctx, root.ID, armed.Generation, "cost", 2.25,
		"Budget checkpoint reached (cost).")
	require.NoError(t, err)
	assert.Equal(t, BudgetFired, fired.State)
	assert.Greater(t, checkpoint.OutputID, receipt.OutputID)

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
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 0.5
	_, _, err = store.ArmBudget(ctx, BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)

	result, err := store.InsertBudgetedResponse(ctx, BudgetedResponse{
		SessionID: root.ID, RootID: root.ID,
		Message: &transcript.Message{
			Role: "assistant", Content: "checkpoint summary", CostUSD: 0.75,
			ToolCalls: json.RawMessage(`[{"id":"danger","name":"bash","arguments":{"command":"false"}}]`),
		},
	})
	require.NoError(t, err)
	require.True(t, result.Fired)
	late, err := store.InsertBudgetedResponse(ctx, BudgetedResponse{
		SessionID: root.ID, RootID: root.ID,
		Message: &transcript.Message{
			Role: "assistant", Content: "late parallel response", CostUSD: 0.1,
			ToolCalls: json.RawMessage(`[{"id":"late","name":"bash","arguments":{}}]`),
		},
	})
	require.NoError(t, err)
	assert.True(t, late.Fired)

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

func TestBudgetStore_CompactionCrossingCommitsReplacementAndCheckpointTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli:main"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 0.5
	_, _, err = store.ArmBudget(ctx, BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)

	result, err := store.ReplaceCompactedMessagesBudgeted(ctx, BudgetedCompaction{
		SessionID: root.ID, RootID: root.ID,
		Entries: []CompactionEntry{{Message: &transcript.Message{Role: "user", Content: "summary", CostUSD: 0.75}}},
	})
	require.NoError(t, err)
	require.True(t, result.Fired)
	require.Len(t, result.MessageIDs, 1)

	var checkpoints int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoints))
	assert.Equal(t, 1, checkpoints)
}

func TestBudgetStore_RejectsCrossSessionGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	first, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "one"})
	require.NoError(t, err)
	second, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "two"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, first.ID, InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 1.0
	_, _, err = store.ArmBudget(ctx, BudgetMutation{
		RootSessionID: second.ID, InputID: input.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "wrong", CostLimitUSD: &limit,
		Receipt: "Budget armed: $1.000000 additional persisted cost",
	})
	require.ErrorIs(t, err, ErrBudgetConflict)
}
