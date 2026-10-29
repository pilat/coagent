package sessionstore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatFocus_CASAndAssistantIdentity(t *testing.T) {
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
	assistant, err := store.EnsureAssistantRoot(ctx, project, "owner", "/assistant", "model", "")
	require.NoError(t, err)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", first.ID))
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", assistant.ID))
	var stored sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `SELECT session_id FROM chat_focus
		WHERE manager_id = 'owner' AND chat_id = 'chat'`).Scan(&stored))
	assert.False(t, stored.Valid)
	focus, err = store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Zero(t, focus)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", 0))
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

func TestChatSeen_MonotonicAndChatScoped(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	first := chatTestOutput(t, store, root.ID, "first")
	second := chatTestOutput(t, store, root.ID, "second")
	seen, err := store.ChatSeen(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	assert.Zero(t, seen)
	for _, id := range []int64{first, second, first} {
		require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, id))
	}
	seen, err = store.ChatSeen(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	assert.Equal(t, second, seen)
	for _, key := range [][2]string{{"other", "chat"}, {"owner", "other-chat"}} {
		seen, err := store.ChatSeen(ctx, key[0], key[1], root.ID)
		require.NoError(t, err)
		assert.Zero(t, seen)
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
	outputs, err := store.ChatOutputs(ctx, "owner", "chat", root.ID, 2)
	require.NoError(t, err)
	require.Equal(t, []ChatOutput{
		{ID: opened.OutputID, SessionID: root.ID, Type: OutputSessionOpened},
		{ID: first, SessionID: root.ID, Type: OutputMessage, Content: "first"},
	}, outputs)
	seen, err := store.ChatSeen(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	assert.Zero(t, seen, "reading the tail does not mark it")
	require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, first))
	closed, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 2)
	require.NoError(t, err)
	outputs, err = store.ChatOutputs(ctx, "owner", "chat", root.ID, 10)
	require.NoError(t, err)
	require.Equal(t, []ChatOutput{
		{ID: second, SessionID: root.ID, Type: OutputMessage, Content: "second"},
		{
			ID: closed.OutputID, SessionID: root.ID, Type: OutputSessionClosed,
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
