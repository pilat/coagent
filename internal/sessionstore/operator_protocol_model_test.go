package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

func TestOperatorProtocolModel_ParallelCrossingReleaseAndReplay(t *testing.T) {
	t.Parallel()

	for _, order := range [][]float64{{0.4, 0.7, 0.2}, {0.7, 0.4, 0.2}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store, db, projectID := newTestStore(t)
			root, err := store.CreateSession(
				ctx,
				projectID,
				"priced",
				"",
				map[string]any{"manager_id": "telegram-test"},
			)
			require.NoError(t, err)
			input, err := enqueueInput(ctx, store, root.ID, InputSourceUser, "/budget")
			require.NoError(t, err)
			_, _, err = acceptActivation(ctx, store, input.ID, "/budget\n\nactivate",
				ActivationDraft{ToolID: "set_budget", Command: "/budget"})
			require.NoError(t, err)
			limit := 1.0
			_, err = store.Arm(ctx, budget.Mutation{
				RootSessionID: root.ID, InputID: input.ID, ToolID: "set_budget", Command: "/budget",
				ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
			})
			require.NoError(t, err)

			modelFired := false
			for i, cost := range order {
				callID := fmt.Sprintf("call-%d", i)
				result, responseErr := store.Commit(
					ctx,
					Commit{SessionID: root.ID, RootID: root.ID, ObserveBudget: true, Messages: []*transcript.Message{{
						Role: "assistant", CostUSD: cost,
						ToolCalls: json.RawMessage(fmt.Sprintf(`[{"ID":%q,"Name":"bash"}]`, callID)),
					}}},
				)
				require.NoError(t, responseErr)
				modelFired = modelFired || result.BudgetFired
				assert.Equal(t, modelFired, result.BudgetFired)
			}

			var checkpoints, skipped int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
				WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoints))
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
				WHERE session_id = ? AND role = 'tool'`, root.ID).Scan(&skipped))
			assert.Equal(t, 1, checkpoints)
			assert.Positive(t, skipped)

			_, err = enqueueUser(ctx, store, root.ID, "continue")
			require.NoError(t, err)
			record, err := store.Get(ctx, root.ID)
			require.NoError(t, err)
			assert.Equal(t, budget.Released, record.State)
		})
	}
}
