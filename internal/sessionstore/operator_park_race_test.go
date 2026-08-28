package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOperatorProtocolModel_UserInputRacesPark pins the plan's park arbitration:
// either the input atomically wins release+acceptance, or the drain owns the
// tree and the input observes that committed state without being recorded.
func TestOperatorProtocolModel_UserInputRacesPark(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	armFired := func(t *testing.T) (Store, int64, *BudgetRecord) {
		t.Helper()

		store, _, projectID := newTestStore(t) //nolint:contextcheck // test helper owns its own bootstrap context
		root, err := store.CreateSession(ctx, projectID, "priced", "", map[string]any{"manager_id": "cli"})
		require.NoError(t, err)
		input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget")
		require.NoError(t, err)
		_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
			ActivationDraft{ToolID: "set_budget", Command: "/budget"})
		require.NoError(t, err)
		limit := 1.0
		_, _, err = store.ArmBudget(ctx, BudgetMutation{
			RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
			ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
		})
		require.NoError(t, err)

		record, _, err := store.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
		require.NoError(t, err)
		require.Equal(t, BudgetFired, record.State)

		return store, root.ID, record
	}

	t.Run("input wins release while drain has not begun", func(t *testing.T) {
		t.Parallel()

		store, rootID, _ := armFired(t)

		_, err := store.EnqueueModelInput(ctx, rootID, "continue")
		require.NoError(t, err)

		budget, err := store.GetBudget(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, BudgetReleased, budget.State)
	})

	t.Run("draining owns the tree and rejects the input", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		_, err = store.EnqueueModelInput(ctx, rootID, "continue")
		require.ErrorIs(t, err, ErrBudgetConflict)
		_, pendingErr := store.PeekPending(ctx, rootID)
		require.ErrorIs(t, pendingErr, ErrNoPendingInput)

		budget, err := store.GetBudget(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, BudgetFired, budget.State, "a rejected race must not release the budget")
	})

	t.Run("parked budget releases on the next user turn", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)
		_, err = store.MarkBudgetParked(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		_, err = store.EnqueueModelInput(ctx, rootID, "continue")
		require.NoError(t, err)

		budget, err := store.GetBudget(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, BudgetReleased, budget.State)
	})

	// A clear racing the drain must not release a budget the park coordinator
	// still owns: the drain window refuses every external release, /budget included.
	t.Run("draining budget refuses clear", func(t *testing.T) {
		t.Parallel()

		store, rootID, record := armFired(t)
		_, err := store.BeginBudgetDrain(ctx, rootID, record.Generation, record.ParkOwner)
		require.NoError(t, err)

		input, err := store.EnqueueInput(ctx, rootID, InputSourceUser, "/budget clear")
		require.NoError(t, err)
		_, _, err = store.PromoteInputWithActivation(ctx, input.ID, "/budget clear\n\nactivate",
			ActivationDraft{ToolID: "set_budget", Command: "/budget"})
		require.NoError(t, err)

		_, _, err = store.ClearBudget(ctx, BudgetMutation{
			RootSessionID: rootID, InputID: input.ID, ToolID: "set_budget",
			Command: "/budget", ToolCallID: "clear-call", Receipt: "Budget cleared",
		})
		require.ErrorIs(t, err, ErrBudgetConflict)

		budget, err := store.GetBudget(ctx, rootID)
		require.NoError(t, err)
		assert.Equal(t, BudgetFired, budget.State, "a refused clear must not touch the budget")
		assert.Equal(t, "draining", budget.ParkPhase)
	})
}
