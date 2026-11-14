package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestOperatorProtocolModel_UserInputRacesPark(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	armFired := func(t *testing.T) (*Store, int64, *budget.Record) {
		t.Helper()

		store, _, projectID := newTestStore(t) //nolint:contextcheck // test helper owns its own bootstrap context
		root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "telegram-test"})
		require.NoError(t, err)
		limit := 1.0
		_, err = store.Arm(ctx, budget.Mutation{
			RootSessionID: root.ID, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
		})
		require.NoError(t, err)
		record, _, err := store.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
		require.NoError(t, err)
		require.Equal(t, budget.Fired, record.State)

		return store, root.ID, record
	}

	t.Run("input wins release while the park has not begun", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)
		before, err := store.GetSession(ctx, rootID)
		require.NoError(t, err)

		_, err = enqueueUser(ctx, store, rootID, "continue")
		require.NoError(t, err)

		record, err := store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Released, record.State)
		assert.Equal(t, "resumed", record.ReleasedReason)
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		assert.False(t, stopped)
		after, err := store.GetSession(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, before.Status, after.Status)
	})

	t.Run("the park's root stop rejects later input", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		require.True(t, stopped)
		root, err := store.GetSession(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, SessionStatusStopping, root.Status)

		_, err = enqueueUser(ctx, store, rootID, "continue")
		require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
		_, pendingErr := store.PeekPending(ctx, rootID)
		require.ErrorIs(t, pendingErr, ErrNoPendingInput)

		record, err := store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Fired, record.State, "a rejected race must not release the budget")
	})

	t.Run("parked budget releases on the next user turn", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		require.True(t, stopped)
		require.NoError(t, store.UpdateSessionStatus(ctx, rootID, SessionStatusStopped))

		_, err = enqueueUser(ctx, store, rootID, "continue")
		require.NoError(t, err)

		record, err := store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Released, record.State)
		assert.Equal(t, "resumed", record.ReleasedReason)
	})

	t.Run("a re-armed budget is not stopped, its next fire is", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		armed, err := store.Arm(ctx, targetedBudgetMutation(t, store, rootID, rootID, "rearm"))
		require.NoError(t, err)
		require.Equal(t, record.Generation+1, armed.Generation)
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		assert.False(t, stopped)
		_, _, err = store.FireBudget(ctx, rootID, armed.Generation, "cost", 2, "next checkpoint")
		require.NoError(t, err)
		stopped, err = store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		assert.True(t, stopped)
	})

	t.Run("a completed root is stopped", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)
		require.NoError(t, store.UpdateSessionStatus(ctx, rootID, SessionStatusCompleted))
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		assert.True(t, stopped)
		root, err := store.GetSession(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, SessionStatusStopping, root.Status)
	})

	t.Run("a root under /stop is left alone", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)
		require.NoError(t, store.UpdateSessionStatus(ctx, rootID, SessionStatusStopping))
		stopped, err := store.StopFiredBudgetRoot(ctx, rootID)
		require.NoError(t, err)
		assert.False(t, stopped)
		root, err := store.GetSession(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, SessionStatusStopping, root.Status)
	})
}
