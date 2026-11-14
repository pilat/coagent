package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
)

func TestEnqueueBudgetCheckpointRequiresHumanInput(t *testing.T) {
	for _, kind := range []string{"human", "stop"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			store, _, project := newTestStore(t)
			root := chatTestRoot(t, store, project, "owner")
			mutation := targetedBudgetMutation(t, store, root.ID, root.ID, "arm")
			armed, err := store.Arm(t.Context(), mutation)
			require.NoError(t, err)
			before, _, err := store.FireBudget(t.Context(), root.ID, armed.Generation, "duration", 0, "checkpoint")
			require.NoError(t, err)
			input := Input{SessionID: root.ID, Source: InputSourceUser, Content: "continue"}
			if kind == "stop" {
				input.Content = "/stop"
			}
			result, err := store.Enqueue(t.Context(), input)
			require.NoError(t, err)
			require.True(t, result.Applied)
			record, err := store.Get(t.Context(), root.ID)
			require.NoError(t, err)
			if kind == "stop" {
				assert.Equal(t, before, record)
			} else {
				assert.Equal(t, budget.Released, record.State)
				assert.Equal(t, "resumed", record.ReleasedReason)
			}
			pending, err := store.ListPending(t.Context(), root.ID)
			require.NoError(t, err)
			require.Len(t, pending, 1)
			assert.Equal(t, input.Content, pending[0].RawContent)
		})
	}
}
