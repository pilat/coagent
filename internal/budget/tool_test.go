package budget_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/tool"
)

func TestBudgetTool_KeepsSchemaAndOwnRoot(t *testing.T) {
	t.Parallel()
	f := newBudgetFixture(t.Context(), t)
	instance := budget.NewTool(f.store, f.rootID, true)
	assert.JSONEq(
		t,
		`{"type":"object","properties":{"action":{"type":"string","enum":["get","set","clear"]},"cost_usd":{"type":"number"},"duration":{"type":"string"}},"required":["action"]}`,
		string(instance.Parameters()),
	)
	result, err := instance.Execute(budgetGrantContext(t.Context(), f.grant()),
		mustJSON(t, map[string]any{"action": "set", "duration": "1m", "session": 999999}))
	require.NoError(t, err)
	assert.Equal(t, []string{"Budget armed: 1m0s wall time"}, result.DirectMessages)
	record, err := f.store.Get(t.Context(), f.rootID)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
}

func budgetGrantContext(ctx context.Context, grant budget.Grant) context.Context {
	return tool.WithActivationGrant(tool.WithCallID(ctx, grant.ToolCallID), tool.ActivationGrant{
		SessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID, Command: grant.Command,
	})
}
