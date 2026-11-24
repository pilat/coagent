package sessionstore

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
	"github.com/pilat/coagent/internal/transcript"
)

func TestEnqueueCompactWithFocusKeepsParkedBudget(t *testing.T) {
	for _, content := range []string{"/compact", "/compact keep API notes", "  /compact keep API notes  ", "keep going"} {
		t.Run(content, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, db, projectID := newTestStore(t)
			root := sessionstoretest.BareRoot(t, store.db, projectID, "priced", "", map[string]any{})
			_, err := enqueueInput(ctx, store, root, InputSourceUser, "/budget")
			require.NoError(t, err)
			limit := 1.0
			_, err = store.Arm(ctx, budget.Mutation{
				RootSessionID: root, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
			})
			require.NoError(t, err)
			_, _, err = store.FireBudget(ctx, root, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
			require.NoError(t, err)
			require.NoError(t, store.UpdateSessionStatus(ctx, root, SessionStatusStopped))
			var before time.Time
			require.NoError(t, db.QueryRowContext(ctx, `SELECT episode_started_at FROM sessions WHERE id = ?`,
				root).Scan(&before))

			_, err = enqueueInput(ctx, store, root, InputSourceUser, content)
			require.NoError(t, err)
			record, err := store.Get(ctx, root)
			require.NoError(t, err)
			if content == "keep going" {
				assert.Equal(t, budget.Released, record.State)
				assert.Equal(t, "resumed", record.ReleasedReason)
				rearm := targetedBudgetMutation(t, store, root, root, "rearm")
				rearmed, armErr := store.Arm(ctx, rearm)
				require.NoError(t, armErr)
				assert.Equal(t, budget.Armed, rearmed.State)
				assert.Equal(t, record.Generation+1, rearmed.Generation)
				return
			}

			assert.Equal(t, budget.Fired, record.State)
			var after time.Time
			require.NoError(t, db.QueryRowContext(ctx, `SELECT episode_started_at FROM sessions WHERE id = ?`,
				root).Scan(&after))
			assert.Equal(t, before, after)
		})
	}
}

func TestBudgetStore_ArmFireAndReplayAreAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root := sessionstoretest.BareRoot(t, store.db, projectID, "priced", "", map[string]any{})
	limit := 2.0
	mutation := budget.Mutation{
		RootSessionID: root, ToolCallID: "call-budget",
		CostLimitUSD: &limit, Receipt: "Budget armed: $2.000000 additional persisted cost",
	}

	armed, err := store.Arm(ctx, mutation)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, armed.State)

	var receipts int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = 'tool:call-budget:direct:0'`, root).Scan(&receipts))
	assert.Equal(t, 1, receipts)

	fired, checkpoint, err := store.FireBudget(ctx, root, armed.Generation, "cost", 2.25,
		"Budget checkpoint reached (cost).")
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, fired.State)
	assert.Positive(t, checkpoint.OutputID)

	duplicate, duplicateCheckpoint, err := store.FireBudget(ctx, root, armed.Generation, "cost", 2.25,
		"Budget checkpoint reached (cost).")
	require.NoError(t, err)
	assert.Equal(t, fired.FiredAt, duplicate.FiredAt)
	assert.True(t, duplicateCheckpoint.Existing)
}

func TestBudgetStore_CrossingResponseCommitsUsageNonExecutionAndCheckpointTogether(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root := sessionstoretest.BareRoot(t, store.db, projectID, "priced", "", map[string]any{})
	limit := 0.5
	_, err := store.Arm(ctx, budget.Mutation{
		RootSessionID: root, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)

	result, err := store.Commit(
		ctx,
		Commit{SessionID: root, RootID: root, ObserveBudget: true, Messages: []*transcript.Message{{
			Role: "assistant", Content: "checkpoint summary", CostUSD: 0.75,
			ToolCalls: json.RawMessage(`[{"ID":"danger","Name":"bash","Arguments":{"command":"false"}}]`),
		}}},
	)
	require.NoError(t, err)
	require.True(t, result.BudgetFired)
	late, err := store.Commit(
		ctx,
		Commit{SessionID: root, RootID: root, ObserveBudget: true, Messages: []*transcript.Message{{
			Role: "assistant", Content: "late parallel response", CostUSD: 0.1,
			ToolCalls: json.RawMessage(`[{"ID":"late","Name":"bash","Arguments":{}}]`),
		}}},
	)
	require.NoError(t, err)
	assert.True(t, late.BudgetFired)

	var assistantCount, resultCount, checkpointCount int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'assistant' AND cost_usd = 0.75`, root).Scan(&assistantCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'danger'
			AND content = ?`, root, budgetToolNotExecuted).Scan(&resultCount))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = ?`,
		root, "budget:1:checkpoint").Scan(&checkpointCount))
	assert.Equal(t, 1, assistantCount)
	assert.Equal(t, 1, resultCount)
	assert.Equal(t, 1, checkpointCount)
	var lateResults int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = 'late'`, root).Scan(&lateResults))
	assert.Equal(t, 1, lateResults)
}
