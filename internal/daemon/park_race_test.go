package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// A user turn racing the park drain must be rejected with an actionable
// explanation, not the raw store conflict — and leave nothing in the inbox.
func TestSendToSessionDuringBudgetDrainExplainsParking(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, filepath.Join(t.TempDir(), "parkrace.db"))
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, filepath.Join(t.TempDir(), "unused.db")))
	t.Cleanup(func() { _ = db.Close() })

	sessions := sessionstore.NewStore(db)
	store := NewStore(db)
	projectID := testProject(t, store, "/tmp/park-race")
	root, err := sessions.CreateSession(ctx, projectID, "priced", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-park",
	})
	require.NoError(t, err)

	input, err := sessions.EnqueueInput(ctx, root.ID, sessionstore.InputSourceUser, "/budget")
	require.NoError(t, err)
	_, _, err = sessions.PromoteInputWithActivation(ctx, input.ID, "/budget\n\nactivate",
		sessionstore.ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.NoError(t, err)
	limit := 1.0
	_, _, err = sessions.ArmBudget(ctx, sessionstore.BudgetMutation{
		RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	fired, _, err := sessions.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
	require.NoError(t, err)
	_, err = sessions.BeginBudgetDrain(ctx, root.ID, fired.Generation, fired.ParkOwner)
	require.NoError(t, err)

	mgr := newSvc(
		&mockFactory{},
		store,
		sessions,
		sessions,
		subagent.NewStore(db),
		subagent.NewTransactions(db),
		nil,
		sessions,
		nil,
		nil,
	)
	err = mgr.SendToSession(ctx, root.ID, "resume the work")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "budget conflict", "the raw store conflict must not reach the user")
	assert.Contains(t, err.Error(), "park",
		"the error must explain the parking state, got: %s", err.Error())

	_, pendingErr := sessions.PeekPending(ctx, root.ID)
	require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
}
