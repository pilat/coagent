package sessionstore

import (
	"database/sql"
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
	entered, err := store.EnterChat(t.Context(), "owner", "chat", root.ID, 17)
	require.NoError(t, err)
	require.True(t, entered)
	require.NoError(t, store.BindChat(t.Context(), "owner", "chat"))
	focus, cursor, entered, err := store.ChatCursor(t.Context(), "owner", "chat")
	require.NoError(t, err)
	assert.Equal(t, root.ID, focus)
	assert.EqualValues(t, 17, cursor)
	assert.True(t, entered)
}
