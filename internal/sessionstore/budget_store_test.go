package sessionstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

func TestEnqueueCompactWithFocusKeepsParkedBudget(t *testing.T) {
	for _, content := range []string{"/compact", "/compact keep API notes", "  /compact keep API notes  ", "keep going"} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, db, projectID := newTestStore(t)
			root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli:main"})
			require.NoError(t, err)
			_, err = enqueueInput(ctx, store, root.ID, InputSourceUser, "/budget")
			require.NoError(t, err)
			limit := 1.0
			_, err = store.Arm(ctx, budget.Mutation{
				RootSessionID: root.ID, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
			})
			require.NoError(t, err)
			fired, _, err := store.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
			require.NoError(t, err)
			_, err = store.BeginBudgetDrain(ctx, root.ID, fired.Generation, fired.ParkOwner)
			require.NoError(t, err)
			_, err = store.MarkBudgetParked(ctx, root.ID, fired.Generation, fired.ParkOwner)
			require.NoError(t, err)
			require.NoError(t, store.UpdateSessionStatus(ctx, root.ID, SessionStatusStopped))
			var before time.Time
			require.NoError(t, db.QueryRowContext(ctx, `SELECT episode_started_at FROM sessions WHERE id = ?`,
				root.ID).Scan(&before))

			_, err = enqueueInput(ctx, store, root.ID, InputSourceUser, content)
			require.NoError(t, err)
			record, err := store.Get(ctx, root.ID)
			require.NoError(t, err)
			if content == "keep going" {
				assert.Equal(t, budget.Released, record.State)
				assert.Equal(t, "resumed", record.ReleasedReason)
				assert.Equal(t, "parked", record.ParkPhase)
				rearm := targetedBudgetMutation(t, store, root.ID, root.ID, "rearm")
				rearmed, armErr := store.Arm(ctx, rearm)
				require.NoError(t, armErr)
				assert.Equal(t, budget.Armed, rearmed.State)
				assert.Equal(t, record.Generation+1, rearmed.Generation)
				assert.Empty(t, rearmed.ParkPhase)
				return
			}

			assert.Equal(t, budget.Fired, record.State)
			assert.Equal(t, "parked", record.ParkPhase)
			var after time.Time
			require.NoError(t, db.QueryRowContext(ctx, `SELECT episode_started_at FROM sessions WHERE id = ?`,
				root.ID).Scan(&after))
			assert.Equal(t, before, after)
		})
	}
}

func TestBudgetStore_ArmFireAndReplayAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	limit := 2.0
	mutation := budget.Mutation{
		RootSessionID: root.ID, ToolCallID: "call-budget",
		CostLimitUSD: &limit, Receipt: "Budget armed: $2.000000 additional persisted cost",
	}

	armed, err := store.Arm(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, armed.State)

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
}

func TestBudgetStore_CrossingResponseCommitsUsageNonExecutionAndCheckpointTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli:main"})
	require.NoError(t, err)
	limit := 0.5
	_, err = store.Arm(ctx, budget.Mutation{
		RootSessionID: root.ID, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
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
		WHERE session_id = ? AND source_key = ?`,
		root.ID, "budget:1:checkpoint").Scan(&checkpointCount))
	assert.Equal(t, 1, assistantCount)
	assert.Equal(t, 1, resultCount)
	assert.Equal(t, 1, checkpointCount)
	var lateResults int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'late'`, root.ID).Scan(&lateResults))
	assert.Equal(t, 1, lateResults)
}
