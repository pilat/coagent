package managercontrol

import (
	"database/sql"
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

func TestChatCapability_RejectsForeignCursorReadAndAdvance(t *testing.T) {
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
	_, err = two.ChatOutputs(ctx, "shared", root.ID, 0, 10)
	require.Error(t, err)
	_, _, err = two.ChatEntry(ctx, "shared", root.ID, 3)
	require.Error(t, err)
	_, err = two.EnterChat(ctx, "shared", root.ID, output.OutputID)
	require.Error(t, err)
	_, err = two.AdvanceChat(ctx, "shared", root.ID, output.OutputID)
	require.Error(t, err)
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
	session, cursor, entered, err := two.ChatCursor(ctx, "shared")
	require.NoError(t, err)
	assert.Zero(t, session)
	assert.Zero(t, cursor)
	assert.False(t, entered)
	rows, err := one.ChatOutputs(ctx, "shared", root.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "private output", rows[0].Content)
	require.NoError(t, store.WithTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO chat_focus(manager_id,chat_id,session_id)
			VALUES ('two','shared',?)`, root.ID)
		return err
	}))
	_, _, _, err = two.ChatCursor(ctx, "shared")
	require.ErrorContains(t, err, "not a root owned by this manager")
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
	rows, err := chat.ChatOutputs(ctx, "chat", root.ID, 0, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, controllerapi.OutputSessionClosed, rows[1].Type)
	entered, err := chat.EnterChat(ctx, "chat", root.ID, 0)
	require.NoError(t, err)
	require.True(t, entered)
	advanced, err := chat.AdvanceChat(ctx, "chat", root.ID, closed.OutputID)
	require.NoError(t, err)
	require.True(t, advanced)
	reset, err := chat.ResetChatFocus(ctx, "chat", root.ID)
	require.NoError(t, err)
	assert.True(t, reset)
	session, cursor, entered, err := chat.ChatCursor(ctx, "chat")
	require.NoError(t, err)
	assert.Zero(t, session)
	assert.Zero(t, cursor)
	assert.False(t, entered)
}

func TestChatCapability_RejectsAnUnboundManager(t *testing.T) {
	t.Parallel()

	factory, _, _ := newChatControl(t)
	chat := chatCapability(t, factory, "")
	require.ErrorContains(t, chat.BindChat(t.Context(), "chat"), "no manager identity")
	_, _, _, err := chat.ChatCursor(t.Context(), "chat")
	require.ErrorContains(t, err, "no manager identity")
	_, _, err = chat.ChatEntry(t.Context(), "chat", 1, 3)
	require.ErrorContains(t, err, "no manager identity")
	_, err = chat.EnterChat(t.Context(), "chat", 1, 0)
	require.ErrorContains(t, err, "no manager identity")
	_, err = chat.AdvanceChat(t.Context(), "chat", 1, 1)
	require.ErrorContains(t, err, "no manager identity")
	_, err = chat.ChatFocus(t.Context(), "chat")
	require.ErrorContains(t, err, "no manager identity")
	require.ErrorContains(t, chat.SetChatFocus(t.Context(), "chat", 0), "no manager identity")
	_, err = chat.ChatSessions(t.Context(), "chat")
	require.ErrorContains(t, err, "no manager identity")
}

func TestChatCapability_BindsOnlyItsManager(t *testing.T) {
	t.Parallel()
	factory, _, _ := newChatControl(t)
	chat := chatCapability(t, factory, "owner")
	require.NoError(t, chat.BindChat(t.Context(), "chat"))
	other := chatCapability(t, factory, "other")
	focus, err := other.ChatFocus(t.Context(), "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
}

func newChatControl(t *testing.T) (controllerapi.ManagerControllerFactory, *sessionstore.Store, int64) {
	t.Helper()

	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "chat.db")
	db, err := migrate.OpenDB(ctx, path)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })

	provider, err := migrate.NewProvider(db)
	require.NoError(t, err)
	_, err = provider.Up(ctx)
	require.NoError(t, err)
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
