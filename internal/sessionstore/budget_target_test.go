package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestBudgetStore_TargetedMutationDeliversReceiptToCaller(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, db, project := newTestStore(t)
	caller := chatTestRoot(t, store, project, "owner")
	target := chatTestRoot(t, store, project, "owner")
	_, err := db.ExecContext(ctx, `INSERT INTO messages (session_id,role,content,cost_usd)
		VALUES (?, 'user', 'caller cost', 50), (?, 'user', 'target cost', 2)`, caller.ID, target.ID)
	require.NoError(t, err)
	for _, action := range []string{"arm", "clear"} {
		mutation := targetedBudgetMutation(t, store, caller.ID, target.ID, action)
		mutate := store.Arm
		if action == "clear" {
			mutation.DurationSeconds = nil
			mutate = store.Clear
		}
		store.TakeWoken()
		record, writeErr := mutate(ctx, mutation)
		require.NoError(t, writeErr)
		assert.Equal(t, target.ID, record.RootSessionID)
		assert.InDelta(t, 2, record.BaselineCostUSD, 1e-9)
		assert.Equal(t, []int64{target.ID}, store.TakeWoken())
		var callerReceipts, targetReceipts int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, caller.ID, "tool:"+action+":direct:0").Scan(&callerReceipts))
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, target.ID, "tool:"+action+":direct:0").Scan(&targetReceipts))
		assert.Equal(t, 1, callerReceipts)
		assert.Zero(t, targetReceipts)
		_, loadErr := store.Get(ctx, caller.ID)
		require.ErrorIs(t, loadErr, budget.ErrNotFound)
	}
}

func TestBudgetStore_ArmReplacesFiredAndReleasedGenerations(t *testing.T) {
	for _, state := range []budget.State{budget.Fired, budget.Released} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			store, _, project := newTestStore(t)
			caller := chatTestRoot(t, store, project, "owner")
			target := chatTestRoot(t, store, project, "owner")
			mutation := targetedBudgetMutation(t, store, caller.ID, target.ID, "first")
			armed, err := store.Arm(ctx, mutation)
			require.NoError(t, err)
			fired, _, err := store.FireBudget(ctx, target.ID, armed.Generation, "duration", 0, "checkpoint")
			require.NoError(t, err)
			if state == budget.Released {
				_, err = store.ReleaseBudget(ctx, target.ID, fired.Generation, "resumed")
				require.NoError(t, err)
			}
			before, err := store.Get(ctx, target.ID)
			require.NoError(t, err)
			mutation = targetedBudgetMutation(t, store, caller.ID, target.ID, "rearm")
			record, armErr := store.Arm(ctx, mutation)
			require.NoError(t, armErr)
			assert.Equal(t, before.Generation+1, record.Generation)
			assert.Equal(t, budget.Armed, record.State)
		})
	}
}

func TestBudgetStore_RejectsNegativeGrantSession(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	mutation := targetedBudgetMutation(t, store, root.ID, root.ID, "invalid")
	mutation.GrantSessionID = -1
	_, err := store.Arm(t.Context(), mutation)
	require.ErrorIs(t, err, budget.ErrConflict)
	mutation.DurationSeconds = nil
	_, err = store.Clear(t.Context(), mutation)
	require.ErrorIs(t, err, budget.ErrConflict)
}

func targetedBudgetMutation(t *testing.T, store *Store, grant, target int64, call string) budget.Mutation {
	t.Helper()
	return budget.Mutation{
		RootSessionID:   target,
		GrantSessionID:  grant,
		ToolCallID:      call,
		DurationSeconds: new(int64(60)),
		Receipt:         "budget " + call,
	}
}
