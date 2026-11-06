package sessionstore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatFocus_CAS(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db, project := newTestStore(t)
	first := chatTestRoot(t, store, project, "owner")
	second := chatTestRoot(t, store, project, "owner")
	focus, err := store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", first.ID))
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", second.ID))
	reset, err := store.ResetChatFocus(ctx, "owner", "chat", first.ID)
	require.NoError(t, err)
	assert.False(t, reset)
	focus, err = store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, second.ID, focus)
	other, err := store.ChatFocus(ctx, "owner", "other-chat")
	require.NoError(t, err)
	assert.Zero(t, other)
	reset, err = store.ResetChatFocus(ctx, "owner", "chat", second.ID)
	require.NoError(t, err)
	assert.True(t, reset)
	var stored sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT session_id FROM chat_focus
		WHERE manager_id = 'owner' AND chat_id = 'chat'`).Scan(&stored))
	assert.False(t, stored.Valid)
	focus, err = store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
}

func TestChatFocus_RefusesNonLiveOwnedRootsWithoutChangingSelection(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, project := newTestStore(t)
	current := chatTestRoot(t, store, project, "owner")
	foreign := chatTestRoot(t, store, project, "other")
	terminating := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.UpdateSessionStatus(ctx, terminating.ID, SessionStatusTerminating))
	killed := chatTestRoot(t, store, project, "owner")
	_, err := store.MarkSessionKilledWithOutput(ctx, killed.ID, 0)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(ctx, project, current.ID, current.ID, "general", "model", "")
	require.NoError(t, err)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", current.ID))
	for _, id := range []int64{foreign.ID, terminating.ID, killed.ID, child, 999999, -1} {
		err := store.SetChatFocus(ctx, "owner", "chat", id)
		require.EqualError(t, err, fmt.Sprintf("#%d is not a live session", id))
		focus, err := store.ChatFocus(ctx, "owner", "chat")
		require.NoError(t, err)
		assert.Equal(t, current.ID, focus)
	}
}

func TestChatOutputs_OrderedBatchesIncludeLifecycleRows(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, project := newTestStore(t)
	root, opened, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	first := chatTestOutput(t, store, root.ID, "first")
	other := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, other.ID, "other session")
	second := chatTestOutput(t, store, root.ID, "second")
	outputs, err := store.ChatOutputs(ctx, root.ID, 0, 2)
	require.NoError(t, err)
	require.Equal(t, []ChatOutput{
		{ID: opened.OutputID, SessionID: root.ID, Name: "test", Type: OutputSessionOpened},
		{ID: first, SessionID: root.ID, Name: "test", Type: OutputMessage, Content: "first"},
	}, outputs)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", root.ID))
	_, cursor, entered, err := store.ChatCursor(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Zero(t, cursor, "reading the tail does not acknowledge it")
	assert.False(t, entered)
	ok, err := store.EnterChat(ctx, "owner", "chat", root.ID, first)
	require.NoError(t, err)
	require.True(t, ok)
	closed, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 2)
	require.NoError(t, err)
	outputs, err = store.ChatOutputs(ctx, root.ID, first, 10)
	require.NoError(t, err)
	require.Equal(t, []ChatOutput{
		{ID: second, SessionID: root.ID, Name: "test", Type: OutputMessage, Content: "second"},
		{
			ID: closed.OutputID, SessionID: root.ID, Name: "test", Type: OutputSessionClosed,
			Content: "Session killed. Cancelled background processes: 2",
		},
	}, outputs)
}

func chatTestRoot(t *testing.T, store *Store, project int64, owner string) *SessionRecord {
	t.Helper()

	root, err := store.CreateSession(t.Context(), project, "model", "", map[string]any{"manager_id": owner})
	require.NoError(t, err)

	return root
}

func chatTestOutput(t *testing.T, store *Store, sessionID int64, content string) int64 {
	t.Helper()

	output, err := store.EnqueueOutput(t.Context(), OutputDraft{
		SessionID: sessionID, Type: OutputMessage, Content: content,
	})
	require.NoError(t, err)
	require.NotNil(t, output)

	return output.OutputID
}
