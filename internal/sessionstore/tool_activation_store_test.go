package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestToolActivationStore_GrantIsBoundToOwnedUserInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/budget $2")
	require.NoError(t, err)

	message, grant, err := store.PromoteInputWithActivation(
		ctx, input.ID, "/budget $2\n\nCall set_budget alone.",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"},
	)
	require.NoError(t, err)
	assert.Equal(t, input.ID, grant.InputID)
	assert.Equal(t, message.ID, mustAcceptedMessageID(t, store, root.ID))

	binding := ActivationBinding{
		InputID: input.ID, SessionID: root.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "call-1",
	}
	require.NoError(t, store.ConsumeActivationBinding(ctx, binding))

	consumed, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, ActivationConsumed, consumed.State)

	// The identical replay is a no-op, never a second application.
	require.NoError(t, store.ConsumeActivationBinding(ctx, binding))

	// A different tool call can never reuse the grant.
	conflicting := binding
	conflicting.ToolCallID = "call-2"
	require.ErrorIs(t, store.ConsumeActivationBinding(ctx, conflicting), ErrActivationConflict)
}

func TestToolActivationStore_RejectsAgentAndSubagentProvenance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)

	agentInput, err := store.EnqueueInput(ctx, root.ID, InputSourceAgent, "/budget $2")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, agentInput.ID, "/budget $2",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.ErrorIs(t, err, ErrActivationConflict)

	childID, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	childInput, err := store.EnqueueInput(ctx, childID, InputSourceUser, "/budget $2")
	require.NoError(t, err)
	_, _, err = store.PromoteInputWithActivation(ctx, childInput.ID, "/budget $2",
		ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.ErrorIs(t, err, ErrActivationConflict)
}

func promoteActivation(t *testing.T, store Store, sessionID int64, toolID, command string) *ToolActivation {
	t.Helper()

	ctx := context.Background()
	input, err := store.EnqueueInput(ctx, sessionID, InputSourceUser, command)
	require.NoError(t, err)
	_, grant, err := store.PromoteInputWithActivation(ctx, input.ID, command, ActivationDraft{
		ToolID: toolID, Command: command,
	})
	require.NoError(t, err)

	return grant
}

func TestConsumeActivationTx_IdempotentConsumeAndConflictRejection(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	promoteActivation(t, store, root.ID, "set_budget", "/budget")

	binding := ActivationBinding{
		SessionID: root.ID,
		ToolID:    "set_budget", Command: "/budget", ToolCallID: "call-1",
	}
	activation, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)
	binding.InputID = activation.InputID

	require.NoError(t, store.ConsumeActivationBinding(ctx, binding))
	// The identical replay is a no-op, never a second application.
	require.NoError(t, store.ConsumeActivationBinding(ctx, binding))

	consumed, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, ActivationConsumed, consumed.State)
	assert.Equal(t, "call-1", consumed.ToolCallID)

	// A different tool call can never reuse the grant.
	consumedBinding := binding
	consumedBinding.ToolCallID = "call-2"
	require.ErrorIs(t, store.ConsumeActivationBinding(ctx, consumedBinding), ErrActivationConflict)
}

func TestConsumeActivationTx_RejectsMismatchedBindings(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	promoteActivation(t, store, root.ID, "set_budget", "/budget")

	activation, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)

	base := ActivationBinding{
		InputID: activation.InputID, SessionID: root.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "call-1",
	}

	wrongSession := base
	wrongSession.SessionID = root.ID + 1
	require.ErrorIs(t, store.ConsumeActivationBinding(ctx, wrongSession), ErrActivationConflict)

	wrongTool := base
	wrongTool.ToolID = "other_tool"
	require.ErrorIs(t, store.ConsumeActivationBinding(ctx, wrongTool), ErrActivationConflict)

	wrongCommand := base
	wrongCommand.Command = "/budgetx"
	require.ErrorIs(t, store.ConsumeActivationBinding(ctx, wrongCommand), ErrActivationConflict)

	// The grant is still pending after every refusal.
	current, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, ActivationPending, current.State)
}

func TestExpireActivation_ConsumedGrantSettlesAsNoOp(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	promoteActivation(t, store, root.ID, "set_budget", "/budget")

	activation, err := store.CurrentActivation(ctx, root.ID)
	require.NoError(t, err)
	require.NoError(t, store.ConsumeActivationBinding(ctx, ActivationBinding{
		InputID: activation.InputID, SessionID: root.ID,
		ToolID: "set_budget", Command: "/budget", ToolCallID: "call-1",
	}))

	expired, err := store.ExpireActivation(ctx, activation.InputID, root.ID)
	require.NoError(t, err, "a consumed grant is already settled; expiry must not conflict")
	assert.Equal(t, ActivationConsumed, expired.State)

	withOutput, _, err := store.ExpireActivationWithOutput(ctx, activation.InputID, root.ID, "receipt")
	require.NoError(t, err)
	assert.Equal(t, ActivationConsumed, withOutput.State)
}

func mustAcceptedMessageID(t *testing.T, store Store, sessionID int64) int64 {
	t.Helper()

	input, err := store.PeekPending(context.Background(), sessionID)
	if err == nil {
		t.Fatalf("unexpected pending input %d", input.ID)
	}
	require.ErrorIs(t, err, ErrNoPendingInput)

	messages, err := store.LoadActiveMessages(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotEmpty(t, messages)

	return messages[len(messages)-1].ID
}
