package budget_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/tool"
)

func TestTargetedBudgetTool_SessionSchemaIsString(t *testing.T) {
	t.Parallel()
	instance := budget.NewTargetedTool(nil, 1, func(context.Context, int64) (int64, bool, error) {
		return 1, true, nil
	})
	var params struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(instance.Parameters(), &params))
	assert.Equal(t, "string", params.Properties["session"].Type)
	assert.Contains(t, params.Properties["session"].Description, `"#244"`)
}

func TestTargetedBudgetTool_GrantTargetAndReceipts(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	f := newBudgetFixture(ctx, t)
	caller, err := f.store.GetSession(ctx, f.rootID)
	require.NoError(t, err)
	target, err := f.store.CreateSession(ctx, caller.ProjectID, "other", "", caller.Attributes)
	require.NoError(t, err)
	var resolved []int64
	instance := budget.NewTargetedTool(f.store, f.rootID, func(_ context.Context, id int64) (int64, bool, error) {
		resolved = append(resolved, id)
		if id == 0 {
			return f.rootID, true, nil
		}
		require.Equal(t, target.ID, id)
		return target.ID, true, nil
	})
	assert.Contains(t, string(instance.Parameters()), `"session"`)
	params := mustJSON(t, map[string]any{"action": "set", "session": fmt.Sprintf("#%d", target.ID), "duration": "1m"})
	_, err = instance.Execute(ctx, params)
	require.EqualError(t, err, "This change requires a current user message beginning with /budget.")
	result, err := instance.Execute(budgetGrantContext(ctx, f.grant()), params)
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("Budget armed for #%d: 1m0s wall time", target.ID)}, result.DirectMessages)
	record, err := f.store.Get(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	_, err = f.store.Get(ctx, f.rootID)
	require.ErrorIs(t, err, budget.ErrNotFound)
	result, err = instance.Execute(ctx, mustJSON(t, map[string]any{"action": "get", "session": target.ID}))
	require.NoError(t, err)
	assert.Equal(t, "Budget armed (generation 1).", result.Output)
	grant := f.newGrant(ctx)
	result, err = instance.Execute(
		budgetGrantContext(ctx, grant),
		mustJSON(t, map[string]any{"action": "clear", "session": target.ID}),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("Budget cleared for #%d", target.ID)}, result.DirectMessages)
	grant = f.newGrant(ctx)
	result, err = instance.Execute(
		budgetGrantContext(ctx, grant),
		mustJSON(t, map[string]any{"action": "set", "duration": "1m"}),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"Budget armed: 1m0s wall time"}, result.DirectMessages)
	assert.Equal(t, int64(0), resolved[len(resolved)-1])
}

func TestTargetedBudgetTool_UsesCurrentTargetPricing(t *testing.T) {
	t.Parallel()
	f := newBudgetFixture(t.Context(), t)
	priced := false
	instance := budget.NewTargetedTool(f.store, f.rootID, func(context.Context, int64) (int64, bool, error) {
		return f.rootID, priced, nil
	})
	ctx := budgetGrantContext(t.Context(), f.grant())
	params := mustJSON(t, map[string]any{"action": "set", "cost_usd": 2})
	_, err := instance.Execute(ctx, params)
	require.EqualError(t, err, "cost budget unavailable: the current model has no catalog pricing")
	priced = true
	result, err := instance.Execute(ctx, params)
	require.NoError(t, err)
	assert.Equal(t, []string{"Budget armed: $2.000000 additional persisted cost"}, result.DirectMessages)
}

func TestBudgetTool_WithoutResolverKeepsSchemaAndOwnRoot(t *testing.T) {
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

func TestTargetedBudgetTool_InvalidSession(t *testing.T) {
	for _, ref := range []any{"#", "##12", -1, true, 1.5} {
		t.Run(fmt.Sprint(ref), func(t *testing.T) {
			t.Parallel()
			instance := budget.NewTargetedTool(nil, 1, func(context.Context, int64) (int64, bool, error) {
				t.Fatal("invalid references must not reach the resolver")
				return 0, false, nil
			})
			_, err := instance.Execute(t.Context(), mustJSON(t, map[string]any{"action": "get", "session": ref}))
			require.EqualError(t, err, "session must be a session id")
		})
	}
}

func budgetGrantContext(ctx context.Context, grant budget.Grant) context.Context {
	return tool.WithActivationGrant(tool.WithCallID(ctx, grant.ToolCallID), tool.ActivationGrant{
		SessionID: grant.RootID, InputID: grant.InputID, ToolID: grant.ToolID, Command: grant.Command,
	})
}
