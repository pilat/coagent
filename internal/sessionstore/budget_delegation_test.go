package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestBudgetStore_DelegatedMutationAndReceiptReplay(t *testing.T) {
	t.Parallel()
	store, db, project := newTestStore(t)
	caller, err := store.EnsureAssistantRoot(t.Context(), project, "owner", "/assistant", "model", "")
	require.NoError(t, err)
	target := chatTestRoot(t, store, project, "owner")
	for _, action := range []string{"arm", "clear"} {
		mutation := delegatedTestBudgetMutation(caller.ID, target.ID, action)
		mutate := store.Arm
		if action == "clear" {
			mutation.DurationSeconds = nil
			mutate = store.Clear
		}
		record, writeErr := mutate(t.Context(), mutation)
		require.NoError(t, writeErr)
		assert.Equal(t, target.ID, record.RootSessionID)
		if action == "arm" {
			assert.Equal(t, budget.Armed, record.State)
		} else {
			assert.Equal(t, budget.Released, record.State)
			assert.Equal(t, "cleared", record.ReleasedReason)
		}
		replayed, replayErr := mutate(t.Context(), mutation)
		require.NoError(t, replayErr)
		assert.Equal(t, record, replayed)
		var receipts, workReceipts, grants int
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, caller.ID, "tool:"+action+":direct:0").Scan(&receipts))
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox
			WHERE session_id = ?`, target.ID).Scan(&workReceipts))
		require.NoError(
			t,
			db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_tool_activations`).Scan(&grants),
		)
		assert.Equal(t, 1, receipts)
		assert.Zero(t, workReceipts)
		assert.Zero(t, grants)
	}
}

func TestBudgetStore_DelegatedMutationRefusesInvalidAuthority(t *testing.T) {
	for _, kind := range []string{
		"work caller", "own assistant", "foreign manager", "killed", "terminating", "child", "assistant target",
		"same root", "ownerless", "missing call", "missing tool", "missing receipt", "missing grant",
	} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store, db, project := newTestStore(t)
			caller, err := store.EnsureAssistantRoot(t.Context(), project, "owner", "/assistant", "model", "")
			require.NoError(t, err)
			target := chatTestRoot(t, store, project, "owner")
			mutation := delegatedTestBudgetMutation(caller.ID, target.ID, "denied")
			switch kind {
			case "work caller":
				mutation.GrantSessionID = chatTestRoot(t, store, project, "owner").ID
			case "own assistant":
				mutation.RootSessionID = caller.ID
			case "foreign manager":
				mutation.RootSessionID = chatTestRoot(t, store, project, "foreign").ID
			case "killed":
				_, err = store.MarkSessionKilledWithOutput(t.Context(), target.ID, 0)
			case "terminating":
				err = store.UpdateSessionStatus(t.Context(), target.ID, SessionStatusTerminating)
			case "child":
				mutation.RootSessionID, err = store.CreateSubagentSession(
					t.Context(),
					project,
					target.ID,
					target.ID,
					"general",
					"model",
					"",
				)
			case "assistant target":
				other, createErr := store.EnsureAssistantRoot(t.Context(), project, "other", "/assistant", "model", "")
				require.NoError(t, createErr)
				mutation.RootSessionID = other.ID
			case "same root":
				mutation.GrantSessionID = target.ID
			case "ownerless":
				_, err = db.ExecContext(t.Context(), `UPDATE sessions SET attributes = '{}' WHERE id = ?`, caller.ID)
			case "missing call":
				mutation.ToolCallID = ""
			case "missing tool":
				mutation.ToolID = ""
			case "missing receipt":
				mutation.Receipt = ""
			case "missing grant":
				mutation.GrantSessionID = 0
			}
			require.NoError(t, err)
			_, err = store.Arm(t.Context(), mutation)
			require.ErrorIs(t, err, budget.ErrConflict)
			mutation.DurationSeconds = nil
			_, err = store.Clear(t.Context(), mutation)
			require.ErrorIs(t, err, budget.ErrConflict)
			var budgets, receipts int
			require.NoError(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_budgets`).Scan(&budgets))
			require.NoError(
				t,
				db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE source_key = 'tool:denied:direct:0'`).
					Scan(&receipts),
			)
			assert.Zero(t, budgets)
			assert.Zero(t, receipts)
		})
	}
}

func TestBudgetStore_DelegatedArmProtectsParking(t *testing.T) {
	for _, phase := range []string{"requested", "draining", "parked"} {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()
			store, _, project := newTestStore(t)
			caller, err := store.EnsureAssistantRoot(t.Context(), project, "owner", "/assistant", "model", "")
			require.NoError(t, err)
			target := chatTestRoot(t, store, project, "owner")
			mutation := delegatedTestBudgetMutation(caller.ID, target.ID, "arm")
			armed, err := store.Arm(t.Context(), mutation)
			require.NoError(t, err)
			fired, _, err := store.FireBudget(t.Context(), target.ID, armed.Generation, "duration", 0, "checkpoint")
			require.NoError(t, err)
			if phase != "requested" {
				_, err = store.BeginBudgetDrain(t.Context(), target.ID, fired.Generation, fired.ParkOwner)
				require.NoError(t, err)
			}
			if phase == "parked" {
				_, err = store.MarkBudgetParked(t.Context(), target.ID, fired.Generation, fired.ParkOwner)
				require.NoError(t, err)
			}
			before, err := store.Get(t.Context(), target.ID)
			require.NoError(t, err)
			mutation.ToolCallID = "rearm"
			rearmed, armErr := store.Arm(t.Context(), mutation)
			if phase == "parked" {
				require.NoError(t, armErr)
				assert.Equal(t, before.Generation+1, rearmed.Generation)
				assert.Equal(t, budget.Armed, rearmed.State)
				assert.Empty(t, rearmed.ParkPhase)
				return
			}
			require.ErrorContains(t, armErr, "is parking after a budget checkpoint")
			after, err := store.Get(t.Context(), target.ID)
			require.NoError(t, err)
			assert.Equal(t, before, after)
			if phase == "draining" {
				mutation.DurationSeconds = nil
				_, err = store.Clear(t.Context(), mutation)
				require.ErrorIs(t, err, budget.ErrConflict)
			}
		})
	}
}

func delegatedTestBudgetMutation(caller, target int64, call string) budget.Mutation {
	return budget.Mutation{
		RootSessionID: target, GrantSessionID: caller, ToolID: budget.ToolID,
		ToolCallID: call, DurationSeconds: new(int64(60)), Receipt: "budget " + call,
	}
}
