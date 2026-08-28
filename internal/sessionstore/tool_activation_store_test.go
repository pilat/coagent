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

	consumed, err := store.ConsumeActivation(
		ctx, input.ID, root.ID, "set_budget", "/budget", "call-1",
	)
	require.NoError(t, err)
	assert.Equal(t, ActivationConsumed, consumed.State)

	replayed, err := store.ConsumeActivation(
		ctx, input.ID, root.ID, "set_budget", "/budget", "call-1",
	)
	require.NoError(t, err)
	assert.Equal(t, consumed.ResolvedAt, replayed.ResolvedAt)

	_, err = store.ConsumeActivation(ctx, input.ID, root.ID, "set_budget", "/budget", "call-2")
	require.ErrorIs(t, err, ErrActivationConflict)
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
