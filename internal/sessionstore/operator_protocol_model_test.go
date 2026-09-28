package sessionstore

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestOperatorProtocolModel_ParallelCrossingReleaseAndReplay(t *testing.T) {
	t.Parallel()

	for _, order := range [][]float64{{0.4, 0.7, 0.2}, {0.7, 0.4, 0.2}, {1.1, 0.2, 0.4}} {
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

			modelCost := 0.0
			modelSkipped := 0
			for i, cost := range order {
				callID := fmt.Sprintf("call-%d", i)
				modelCost += cost
				modelFired := modelCost >= limit
				if modelFired {
					modelSkipped++
				}
				result, responseErr := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
					SessionID: root.ID, RootID: root.ID, Iteration: i + 1, Kind: ResponseDispositionToolCall,
					Message: &transcript.Message{
						Role: "assistant", CostUSD: cost, Content: "suppressed model prose",
						ToolCalls: json.RawMessage(fmt.Sprintf(`[{"id":%q,"name":"bash"}]`, callID)),
					},
				})
				require.NoError(t, responseErr)
				assert.Equal(t, modelFired, result.BudgetFired)
				budget, budgetErr := store.GetBudget(ctx, root.ID)
				require.NoError(t, budgetErr)
				assert.Equal(t, modelFired, budget.State == BudgetFired)
				_, _, persistedCost, usageErr := store.GetSessionTreeUsage(ctx, root.ID)
				require.NoError(t, usageErr)
				assert.InDelta(t, modelCost, persistedCost, 0.000001)
			}

			var checkpoints, skipped int
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox
				WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoints))
			require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages
				WHERE session_id = ? AND role = 'tool'`, root.ID).Scan(&skipped))
			assert.Equal(t, 1, checkpoints)
			assert.Equal(t, modelSkipped, skipped)
			var checkpoint string
			require.NoError(t, db.QueryRowContext(ctx, `SELECT content FROM session_outbox
				WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, root.ID).Scan(&checkpoint))
			assert.NotContains(t, checkpoint, "suppressed model prose")

			_, err = store.EnqueueModelInput(ctx, root.ID, "continue")
			require.NoError(t, err)
			budget, err := store.GetBudget(ctx, root.ID)
			require.NoError(t, err)
			assert.Equal(t, BudgetReleased, budget.State)
		})
	}
}
