package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
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
	store := sessionstore.NewStore(db)
	projectID := testProject(t, store, "/tmp/park-race")
	root, err := sessions.CreateSession(ctx, projectID, "priced", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-park",
	})
	require.NoError(t, err)

	input, err := sessions.Enqueue(
		ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/budget"},
	)
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{
		SessionID: root.ID,
		Accept: []sessionstore.Accept{
			{
				InputID: input.Input.ID,
				State:   sessionstore.InputStateAccepted,
				Content: "/budget\n\nactivate",
				LinkRef: -1,
			},
		},
		Activation: &sessionstore.ActivationChange{InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget"},
	})
	require.NoError(t, err)
	limit := 1.0
	_, err = sessions.Arm(ctx, budget.Mutation{
		RootSessionID: root.ID, InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	fired, _, err := sessions.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
	require.NoError(t, err)
	_, err = sessions.BeginBudgetDrain(ctx, root.ID, fired.Generation, fired.ParkOwner)
	require.NoError(t, err)

	mgr, _ := newScenarioDaemon(
		context.Background(),
		scriptedBuildInput(
			t,
			&config.Config{Model: "fake-model"},
			sessions,
			nil,
			func(*config.Config) (llm.Client, error) { return &scriptedLLM{respond: trivialRespond}, nil },
		),
		sessions,
		subagent.NewStore(db, store),
		nil,
		nil,
		nil,
		db,
	)
	err = mgr.sendToSession(ctx, root.ID, "resume the work")
	require.Error(t, err)

	assert.NotContains(t, err.Error(), "budget conflict", "the raw store conflict must not reach the user")
	assert.Contains(t, err.Error(), "park",
		"the error must explain the parking state, got: %s", err.Error())

	_, pendingErr := sessions.PeekPending(ctx, root.ID)
	require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
}
