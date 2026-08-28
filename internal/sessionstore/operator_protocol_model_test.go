package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOperatorProtocolModel_ParallelCrossingReleaseAndReplay(t *testing.T) {
	t.Parallel()

	for _, order := range [][]float64{{0.4, 0.7, 0.2}, {0.7, 0.4, 0.2}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, db, projectID := newTestStore(t)
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

			modelFired := false
			for i, cost := range order {
				callID := fmt.Sprintf("call-%d", i)
				result, responseErr := store.InsertBudgetedResponse(ctx, BudgetedResponse{
					SessionID: root.ID, RootID: root.ID,
					Message: &StoredMessage{
						Role: "assistant", CostUSD: cost,
						ToolCalls: json.RawMessage(fmt.Sprintf(`[{"id":%q,"name":"bash"}]`, callID)),
					},
				})
				require.NoError(t, responseErr)
				modelFired = modelFired || result.Fired
				assert.Equal(t, modelFired, result.Fired)
			}

			var checkpoints, skipped int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
				WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoints))
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
				WHERE session_id = ? AND role = 'tool'`, root.ID).Scan(&skipped))
			assert.Equal(t, 1, checkpoints)
			assert.Positive(t, skipped)

			_, err = store.EnqueueModelInput(ctx, root.ID, "continue")
			require.NoError(t, err)
			budget, err := store.GetBudget(ctx, root.ID)
			require.NoError(t, err)
			assert.Equal(t, BudgetReleased, budget.State)
		})
	}
}
