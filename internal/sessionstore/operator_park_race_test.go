package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

// TestOperatorProtocolModel_UserInputRacesPark pins the plan's park arbitration:
// either the input atomically wins release+acceptance, or the drain owns the
// tree and the input observes that committed state without being recorded.
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

	t.Run("input wins release while drain has not begun", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)

		_, err := enqueueUser(ctx, store, rootID, "continue")
		require.NoError(t, err)

		record, err := store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Released, record.State)
	})

	t.Run("draining owns the tree and rejects the input", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		_, err = enqueueUser(ctx, store, rootID, "continue")
		require.ErrorIs(t, err, budget.ErrConflict)
		_, pendingErr := store.PeekPending(ctx, rootID)
		require.ErrorIs(t, pendingErr, ErrNoPendingInput)

		record, err = store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Fired, record.State, "a rejected race must not release the budget")
	})

	t.Run("parked budget releases on the next user turn", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)
		_, err = store.MarkBudgetParked(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		_, err = enqueueUser(ctx, store, rootID, "continue")
		require.NoError(t, err)

		record, err = store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Released, record.State)
	})

	// A clear racing the drain must not release a budget the park coordinator
	// still owns: the drain window refuses every external release, /budget included.
	t.Run("draining budget refuses clear", func(t *testing.T) {
		t.Parallel()
		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		_, err = store.Clear(ctx, budget.Mutation{
			RootSessionID: rootID, ToolCallID: "clear-call", Receipt: "Budget cleared",
		})
		require.ErrorIs(t, err, budget.ErrConflict)

		record, err = store.Get(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, budget.Fired, record.State, "a refused clear must not touch the budget")
		assert.Equal(t, "draining", record.ParkPhase)
	})
}
