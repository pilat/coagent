package managercontrol

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
)

type chatTestBackend struct {
	Backend
	store   *sessionstore.Store
	project int64
}

func TestChatCapability_RejectsForeignFocusReadAndMark(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	factory, store, project := newChatControl(t)
	one := chatCapability(t, factory, "one")
	two := chatCapability(t, factory, "two")
	root, err := store.CreateSession(ctx, project, "model", "", map[string]any{"manager_id": "one"})
	require.NoError(t, err)
	output, err := store.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: root.ID, Type: sessionstore.OutputMessage, Content: "private output",
	})
	require.NoError(t, err)
	require.NoError(t, one.SetChatFocus(ctx, "shared", root.ID))
	require.EqualError(t, two.SetChatFocus(ctx, "shared", root.ID), fmt.Sprintf("#%d is not a live session", root.ID))
	_, err = two.ChatOutputs(ctx, "shared", root.ID, 10)
	require.Error(t, err)
	require.Error(t, two.MarkChatSeen(ctx, "shared", root.ID, output.OutputID))
	_, err = two.ResetChatFocus(ctx, "shared", root.ID)
	require.Error(t, err)
	_, err = two.SessionKilled(ctx, root.ID)
	require.Error(t, err)
	focus, err := two.ChatFocus(ctx, "shared")
	require.NoError(t, err)
	assert.Zero(t, focus)
	focus, err = one.ChatFocus(ctx, "shared")
	require.NoError(t, err)
	assert.Equal(t, root.ID, focus)
	seen, err := store.ChatSeen(ctx, "two", "shared", root.ID)
	require.NoError(t, err)
	assert.Zero(t, seen)
	rows, err := one.ChatOutputs(ctx, "shared", root.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "private output", rows[0].Content)
}

func TestChatCapability_AssistantUsesZeroAndItsOwnWatermark(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	factory, store, project := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	assistant, err := factory.ForManager("owner").EnsureAssistantRoot(ctx)
	require.NoError(t, err)
	work, err := store.CreateSession(ctx, project, "model", "", map[string]any{"manager_id": "owner"})
	require.NoError(t, err)
	require.NoError(t, chat.SetChatFocus(ctx, "chat", work.ID))
	require.NoError(t, chat.SetChatFocus(ctx, "chat", 0))
	focus, err := chat.ChatFocus(ctx, "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
	focus, err = store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
	require.NoError(t, chat.SetChatFocus(ctx, "chat", assistant.SessionID))
	output, err := store.EnqueueOutput(ctx, sessionstore.OutputDraft{
		SessionID: assistant.SessionID, Type: sessionstore.OutputMessage, Content: "assistant answer",
	})
	require.NoError(t, err)
	rows, err := chat.ChatOutputs(ctx, "chat", 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Zero(t, rows[1].SessionID)
	assert.Equal(t, "assistant answer", rows[1].Content)
	require.NoError(t, chat.MarkChatSeen(ctx, "chat", 0, output.OutputID))
	seen, err := store.ChatSeen(ctx, "owner", "chat", assistant.SessionID)
	require.NoError(t, err)
	assert.Equal(t, output.OutputID, seen)
	sessions, err := chat.ChatSessions(ctx, "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	assert.Equal(t, controllerapi.ChatSessionInfo{
		ID: 0, SessionID: assistant.SessionID, Name: "Assistant",
	}, sessions[0])
}

func TestChatCapability_KillFenceDoesNotBecomeKilledBeforeClose(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	factory, store, project := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	root, err := store.CreateSession(ctx, project, "model", "", map[string]any{"manager_id": "owner"})
	require.NoError(t, err)
	require.NoError(t, chat.SetChatFocus(ctx, "chat", root.ID))
	input, err := store.Enqueue(ctx, sessionstore.Input{
		SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/kill",
	})
	require.NoError(t, err)
	_, err = store.BeginLifecycleInput(ctx, input.Input.ID, "kill", "Stopping session...")
	require.NoError(t, err)
	killed, err := chat.SessionKilled(ctx, root.ID)
	require.NoError(t, err)
	assert.False(t, killed)
	require.EqualError(t, chat.SetChatFocus(ctx, "chat", root.ID), fmt.Sprintf("#%d is not a live session", root.ID))
	closed, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 0)
	require.NoError(t, err)
	killed, err = chat.SessionKilled(ctx, root.ID)
	require.NoError(t, err)
	assert.True(t, killed)
	require.EqualError(t, chat.SetChatFocus(ctx, "chat", root.ID), fmt.Sprintf("#%d is not a live session", root.ID))
	rows, err := chat.ChatOutputs(ctx, "chat", root.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, controllerapi.OutputSessionClosed, rows[1].Type)
	require.NoError(t, chat.MarkChatSeen(ctx, "chat", root.ID, closed.OutputID))
	reset, err := chat.ResetChatFocus(ctx, "chat", root.ID)
	require.NoError(t, err)
	assert.True(t, reset)
}

func TestChatCapability_RejectsAnUnboundManager(t *testing.T) {
	t.Parallel()

	factory, _, _ := newChatControl(t)
	chat := chatCapability(t, factory, "")
	require.ErrorContains(t, chat.BindChat(t.Context(), "chat"), "no manager identity")
	_, err := chat.ChatFocus(t.Context(), "chat")
	require.ErrorContains(t, err, "no manager identity")
	require.ErrorContains(t, chat.SetChatFocus(t.Context(), "chat", 0), "no manager identity")
	_, err = chat.ChatSessions(t.Context(), "chat")
	require.ErrorContains(t, err, "no manager identity")
}

func TestChatCapability_BindsOnlyItsManager(t *testing.T) {
	t.Parallel()
	factory, store, _ := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	require.NoError(t, chat.BindChat(t.Context(), "chat"))
	require.ErrorIs(t, store.FocusOwnerChat(t.Context(), "other", 1), sessionstore.ErrChatNotBound)
	require.EqualError(t, store.FocusOwnerChat(t.Context(), "owner", 999999), "#999999 is not a live session")
}

func (b *chatTestBackend) EnsureAssistantRoot(
	ctx context.Context,
	managerID string,
) (*sessionstore.SessionRecord, error) {
	record, err := b.store.EnsureAssistantRoot(ctx, b.project, managerID, "/assistant", "model", "")
	if err != nil {
		return nil, fmt.Errorf("ensure test assistant: %w", err)
	}

	return record, nil
}

func newChatControl(t *testing.T) (controllerapi.ManagerControllerFactory, *sessionstore.Store, int64) {
	t.Helper()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := migrate.OpenDB(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, migrate.Run(ctx, db, path))
	store := sessionstore.NewStore(db)
	project, err := store.GetOrCreateProject(ctx, t.TempDir())
	require.NoError(t, err)
	backend := &chatTestBackend{store: store, project: project}

	return New(backend, store, nil, nil, nil, nil), store, project
}

func chatCapability(
	t *testing.T,
	factory controllerapi.ManagerControllerFactory,
	managerID string,
) controllerapi.ChatFocusController {
	t.Helper()

	chat, ok := factory.ForManager(managerID).(controllerapi.ChatFocusController)
	require.True(t, ok)

	return chat
}
