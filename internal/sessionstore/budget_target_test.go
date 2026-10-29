package sessionstore

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestBudgetStore_TargetedGrantAndReceiptReplay(t *testing.T) {
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
		activation, loadErr := store.CurrentActivation(ctx, caller.ID)
		require.NoError(t, loadErr)
		assert.Equal(t, ActivationConsumed, activation.State)
		assert.Equal(t, action, activation.ToolCallID)
		replayed, replayErr := mutate(ctx, mutation)
		require.NoError(t, replayErr)
		assert.Equal(t, record, replayed)
		var callerReceipts, targetReceipts int
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, caller.ID, "tool:"+action+":direct:0").Scan(&callerReceipts))
		require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, target.ID, "tool:"+action+":direct:0").Scan(&targetReceipts))
		assert.Equal(t, 1, callerReceipts)
		assert.Zero(t, targetReceipts)
		_, loadErr = store.Get(ctx, caller.ID)
		require.ErrorIs(t, loadErr, budget.ErrNotFound)
	}
}

func TestBudgetStore_ArmProtectsParkingAndAllowsReleasedPhases(t *testing.T) {
	for _, tc := range []struct {
		state, phase string
	}{
		{"fired", "requested"},
		{"fired", "draining"},
		{"released", "requested"},
		{"released", "parked"},
	} {
		t.Run(tc.state+"/"+tc.phase, func(t *testing.T) {
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
			if tc.phase == "draining" || tc.phase == "parked" {
				_, err = store.BeginBudgetDrain(ctx, target.ID, fired.Generation, fired.ParkOwner)
				require.NoError(t, err)
			}
			if tc.phase == "parked" {
				_, err = store.MarkBudgetParked(ctx, target.ID, fired.Generation, fired.ParkOwner)
				require.NoError(t, err)
			}
			if tc.state == "released" {
				_, err = store.ReleaseBudget(ctx, target.ID, fired.Generation, "resumed")
				require.NoError(t, err)
			}
			before, err := store.Get(ctx, target.ID)
			require.NoError(t, err)
			mutation = targetedBudgetMutation(t, store, caller.ID, target.ID, "rearm")
			record, armErr := store.Arm(ctx, mutation)
			if tc.state == "fired" {
				require.EqualError(
					t,
					armErr,
					fmt.Sprintf("session #%d is parking after a budget checkpoint; try again once it stops", target.ID),
				)
				after, loadErr := store.Get(ctx, target.ID)
				require.NoError(t, loadErr)
				assert.Equal(t, before, after)
				_, loadErr = store.PendingActivation(ctx, caller.ID)
				require.NoError(t, loadErr, "a refusal leaves the grant available")
				return
			}
			require.NoError(t, armErr)
			assert.Equal(t, before.Generation+1, record.Generation)
			assert.Equal(t, budget.Armed, record.State)
			assert.Empty(t, record.ParkPhase)
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
	input, err := enqueueInput(t.Context(), store, grant, InputSourceUser, "/budget 1m")
	require.NoError(t, err)
	_, _, err = acceptActivation(t.Context(), store, input.ID, "/budget 1m\n\nactivate",
		ActivationDraft{ToolID: budget.ToolID, Command: "/budget"})
	require.NoError(t, err)
	return budget.Mutation{
		RootSessionID: target, GrantSessionID: grant, InputID: input.ID, ToolID: budget.ToolID,
		Command: "/budget", ToolCallID: call, DurationSeconds: new(int64(60)), Receipt: "budget " + call,
	}
}
