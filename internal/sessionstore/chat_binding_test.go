package sessionstore

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBindChat_PreservesSelection(t *testing.T) {
	t.Parallel()
	store, db, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	for range 2 {
		require.NoError(t, store.BindChat(t.Context(), "owner", "chat"))
	}
	var selected sql.NullInt64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT session_id FROM chat_focus
		WHERE manager_id = 'owner' AND chat_id = 'chat'`).Scan(&selected))
	assert.False(t, selected.Valid)
	require.NoError(t, store.SetChatFocus(t.Context(), "owner", "chat", root.ID))
	require.NoError(t, store.BindChat(t.Context(), "owner", "chat"))
	focus, err := store.ChatFocus(t.Context(), "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, root.ID, focus)
}

func TestFocusOwnerChat_OnlyEntersLiveWorkFromAssistant(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, project := newTestStore(t)
	work := chatTestRoot(t, store, project, "owner")
	other := chatTestRoot(t, store, project, "owner")
	foreign := chatTestRoot(t, store, project, "foreign")
	killed := chatTestRoot(t, store, project, "owner")
	_, err := store.MarkSessionKilledWithOutput(ctx, killed.ID, 0)
	require.NoError(t, err)
	terminating := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.UpdateSessionStatus(ctx, terminating.ID, SessionStatusTerminating))
	child, err := store.CreateSubagentSession(ctx, project, work.ID, work.ID, "general", "model", "")
	require.NoError(t, err)
	assistant, err := store.EnsureAssistantRoot(ctx, project, "owner", "/assistant", "model", "")
	require.NoError(t, err)
	require.ErrorIs(t, store.FocusOwnerChat(ctx, "owner", work.ID), ErrChatNotBound)
	require.NoError(t, store.BindChat(ctx, "owner", "chat"))
	for _, id := range []int64{0, -1, 999999, foreign.ID, child, killed.ID, terminating.ID, assistant.ID} {
		require.EqualError(t, store.FocusOwnerChat(ctx, "owner", id), fmt.Sprintf("#%d is not a live session", id))
		focus, readErr := store.ChatFocus(ctx, "owner", "chat")
		require.NoError(t, readErr)
		assert.Zero(t, focus)
	}
	require.NoError(t, store.FocusOwnerChat(ctx, "owner", work.ID))
	focus, err := store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, work.ID, focus)
	for _, id := range []int64{work.ID, other.ID} {
		require.EqualError(t, store.FocusOwnerChat(ctx, "owner", id),
			fmt.Sprintf("the user is in session #%d; they switch themselves", work.ID))
	}
	focus, err = store.ChatFocus(ctx, "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, work.ID, focus)
}
