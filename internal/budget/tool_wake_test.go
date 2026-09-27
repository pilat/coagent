package budget

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestBudgetToolWakesAfterCommittedMutation(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	f := newBudgetFixture(ctx, t)
	var observed []sessionstore.BudgetState
	budgetTool := NewTool(f.svc, f.rootID, true, func() {
		record, err := f.svc.Get(ctx, f.rootID)
		require.NoError(t, err)
		observed = append(observed, record.State)
	})
	authorized := func(grant Grant) context.Context {
		return tool.WithActivationGrant(tool.WithCallID(ctx, grant.ToolCallID), tool.ActivationGrant{
			SessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID, Command: grant.Command,
		})
	}

	_, err := budgetTool.Execute(ctx, []byte(`{"action":"get"}`))
	require.NoError(t, err)
	_, err = budgetTool.Execute(ctx, []byte(`{"action":"set","duration":"1m"}`))
	require.Error(t, err)
	assert.Empty(t, observed)

	_, err = budgetTool.Execute(authorized(f.grant()), []byte(`{"action":"set","duration":"1m"}`))
	require.NoError(t, err)
	assert.Equal(t, []sessionstore.BudgetState{sessionstore.BudgetArmed}, observed)
	_, err = budgetTool.Execute(authorized(f.newGrant(ctx)), []byte(`{"action":"clear"}`))
	require.NoError(t, err)
	assert.Equal(t, []sessionstore.BudgetState{sessionstore.BudgetArmed, sessionstore.BudgetReleased}, observed)
}
