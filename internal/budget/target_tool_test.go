package budget_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type mutationRecorder struct {
	budget.Store
	last budget.Mutation
}

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
	_, err = f.db.ExecContext(ctx, `UPDATE sessions SET agent_type = 'assistant' WHERE id = ?`, f.rootID)
	require.NoError(t, err)
	target, err := f.store.CreateSession(ctx, caller.ProjectID, "other", "", caller.Attributes)
	require.NoError(t, err)
	var resolved []int64
	recorder := &mutationRecorder{Store: f.store}
	instance := budget.NewTargetedTool(recorder, f.rootID, func(_ context.Context, id int64) (int64, bool, error) {
		resolved = append(resolved, id)
		if id == 0 {
			return f.rootID, true, nil
		}
		require.Equal(t, target.ID, id)
		return target.ID, true, nil
	})
	assert.Contains(t, string(instance.Parameters()), `"session"`)
	assert.Contains(t, instance.Description(), "set and clear act at once on an ordinary request")
	assert.Contains(
		t,
		instance.Description(),
		"changing your own budget requires the current real user message to begin with /budget",
	)
	params := mustJSON(t, map[string]any{"action": "set", "session": fmt.Sprintf("#%d", target.ID), "duration": "1m"})
	_, err = instance.Execute(ctx, params)
	require.EqualError(t, err, "budget change requires a tool call id")
	result, err := instance.Execute(tool.WithCallID(ctx, "delegated-arm"), params)
	require.NoError(t, err)
	assert.Zero(t, recorder.last.InputID)
	assert.Empty(t, recorder.last.Command)
	assert.Equal(t, f.rootID, recorder.last.GrantSessionID)
	assert.Equal(t, "delegated-arm", recorder.last.ToolCallID)
	assert.Equal(t, []string{fmt.Sprintf("Budget armed for #%d: 1m0s wall time", target.ID)}, result.DirectMessages)
	record, err := f.store.Get(ctx, target.ID)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	_, err = f.store.Get(ctx, f.rootID)
	require.ErrorIs(t, err, budget.ErrNotFound)
	result, err = instance.Execute(ctx, mustJSON(t, map[string]any{"action": "get", "session": target.ID}))
	require.NoError(t, err)
	assert.Equal(t, "Budget armed (generation 1).", result.Output)
	result, err = instance.Execute(tool.WithCallID(ctx, "delegated-clear"),
		mustJSON(t, map[string]any{"action": "clear", "session": target.ID}))
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("Budget cleared for #%d", target.ID)}, result.DirectMessages)
	assert.Zero(t, recorder.last.InputID)
	assert.Empty(t, recorder.last.Command)
	assert.Equal(t, f.rootID, recorder.last.GrantSessionID)
	for _, action := range []string{"set", "clear"} {
		args := map[string]any{"action": action}
		if action == "set" {
			args["duration"] = "1m"
		}
		_, err = instance.Execute(tool.WithCallID(ctx, "own-no-grant"), mustJSON(t, args))
		require.EqualError(t, err, "This change requires a current user message beginning with /budget.")
	}
	grant := f.grant()
	result, err = instance.Execute(budgetGrantContext(ctx, grant), params)
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("Budget armed for #%d: 1m0s wall time", target.ID)}, result.DirectMessages)
	assert.Equal(t, grant.InputID, recorder.last.InputID)
	assert.Equal(t, "/budget", recorder.last.Command)
	activation, err := f.store.CurrentActivation(ctx, f.rootID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.ActivationConsumed, activation.State)
	grant = f.newGrant(ctx)
	result, err = instance.Execute(
		budgetGrantContext(ctx, grant),
		mustJSON(t, map[string]any{"action": "clear", "session": target.ID}),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{fmt.Sprintf("Budget cleared for #%d", target.ID)}, result.DirectMessages)
	grant = f.newGrant(ctx)
	assert.Equal(t, "/budget", recorder.last.Command)
	assert.NotZero(t, recorder.last.InputID)
	result, err = instance.Execute(
		budgetGrantContext(ctx, grant),
		mustJSON(t, map[string]any{"action": "set", "duration": "1m"}),
	)
	require.NoError(t, err)
	assert.Equal(t, []string{"Budget armed: 1m0s wall time"}, result.DirectMessages)
	assert.Equal(t, grant.InputID, recorder.last.InputID)
	assert.Equal(t, int64(0), resolved[len(resolved)-1])
}

func TestTargetedBudgetTool_CurrentGrantErrorsNeverDelegate(t *testing.T) {
	for _, action := range []string{"set", "clear"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			instance := budget.NewTargetedTool(nil, 1, func(context.Context, int64) (int64, bool, error) {
				return 2, true, nil
			})
			ctx := tool.WithActivationGrant(tool.WithCallID(t.Context(), "new-call"), tool.ActivationGrant{
				SessionID: 1, ToolID: budget.ToolID, Command: "/budget", InputID: 10, ToolCallID: "old-call",
			})
			args := map[string]any{"action": action, "session": "2"}
			if action == "set" {
				args["duration"] = "1m"
			}
			_, err := instance.Execute(ctx, mustJSON(t, args))
			require.EqualError(t, err, "This change requires a current user message beginning with /budget.")
		})
	}
}

func TestTargetedBudgetTool_ClearResolvesBeforeGrant(t *testing.T) {
	t.Parallel()
	cause := errors.New("target refused")
	instance := budget.NewTargetedTool(nil, 1, func(context.Context, int64) (int64, bool, error) {
		return 0, false, cause
	})
	_, err := instance.Execute(t.Context(), mustJSON(t, map[string]any{"action": "clear", "session": "2"}))
	require.ErrorIs(t, err, cause)
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

func (s *mutationRecorder) Arm(ctx context.Context, mutation budget.Mutation) (*budget.Record, error) {
	s.last = mutation
	return s.Store.Arm(ctx, mutation)
}

func (s *mutationRecorder) Clear(ctx context.Context, mutation budget.Mutation) (*budget.Record, error) {
	s.last = mutation
	return s.Store.Clear(ctx, mutation)
}
