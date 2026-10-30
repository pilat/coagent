package sessionstore

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestEnqueueBudgetCheckpointRequiresHumanInput(t *testing.T) {
	for _, phase := range []string{"requested", "draining", "parked"} {
		for _, kind := range []string{"human", "observer", "assistant", "stop"} {
			t.Run(phase+"/"+kind, func(t *testing.T) {
				t.Parallel()
				store, _, project := newTestStore(t)
				root := chatTestRoot(t, store, project, "owner")
				mutation := targetedBudgetMutation(t, store, root.ID, root.ID, "arm")
				armed, err := store.Arm(t.Context(), mutation)
				require.NoError(t, err)
				fired, _, err := store.FireBudget(t.Context(), root.ID, armed.Generation, "duration", 0, "checkpoint")
				require.NoError(t, err)
				if phase != "requested" {
					_, err = store.BeginBudgetDrain(t.Context(), root.ID, fired.Generation, fired.ParkOwner)
					require.NoError(t, err)
				}
				if phase == "parked" {
					_, err = store.MarkBudgetParked(t.Context(), root.ID, fired.Generation, fired.ParkOwner)
					require.NoError(t, err)
				}
				before, err := store.Get(t.Context(), root.ID)
				require.NoError(t, err)
				input := Input{SessionID: root.ID, Source: InputSourceUser, Content: "continue"}
				switch kind {
				case "observer":
					input.Attributes = map[string]any{ObserverSessionIDAttribute: nil}
				case "assistant", "stop":
					input.Attributes = map[string]any{"from": "assistant"}
				}
				if kind == "stop" {
					input.Content = "/stop"
				}
				result, enqueueErr := store.Enqueue(t.Context(), input)
				record, err := store.Get(t.Context(), root.ID)
				require.NoError(t, err)
				switch {
				case kind == "stop":
					require.NoError(t, enqueueErr)
					require.True(t, result.Applied)
					assert.Equal(t, before, record)
				case kind != "human":
					require.ErrorIs(t, enqueueErr, ErrBudgetParked)
					require.EqualError(
						t,
						enqueueErr,
						fmt.Sprintf(
							"session #%d is parked at its budget checkpoint; raise or clear its budget with set_budget first",
							root.ID,
						),
					)
					assert.Equal(t, before, record)
				case phase == "draining":
					require.ErrorIs(t, enqueueErr, budget.ErrConflict)
					assert.Equal(t, before, record)
				default:
					require.NoError(t, enqueueErr)
					require.True(t, result.Applied)
					assert.Equal(t, budget.Released, record.State)
					assert.Equal(t, "resumed", record.ReleasedReason)
				}
				pending, err := store.ListPending(t.Context(), root.ID)
				require.NoError(t, err)
				if enqueueErr != nil {
					assert.Empty(t, pending)
				} else {
					require.Len(t, pending, 1)
					assert.Equal(t, input.Content, pending[0].RawContent)
				}
			})
		}
	}
}
