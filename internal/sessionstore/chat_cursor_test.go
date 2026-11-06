package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatCursor_EntryAndAdvanceCompareFocusAndEntryState(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, project := newTestStore(t)
	first := chatTestRoot(t, store, project, "owner")
	second := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", first.ID))
	advanced, err := store.AdvanceChat(ctx, "owner", "chat", first.ID, 3)
	require.NoError(t, err)
	assert.False(t, advanced)
	entered, err := store.EnterChat(ctx, "owner", "chat", first.ID, 3)
	require.NoError(t, err)
	require.True(t, entered)
	for _, test := range []struct {
		id   int64
		want bool
	}{{2, false}, {3, false}, {4, true}} {
		advanced, err = store.AdvanceChat(ctx, "owner", "chat", first.ID, test.id)
		require.NoError(t, err)
		assert.Equal(t, test.want, advanced)
	}
	entered, err = store.EnterChat(ctx, "owner", "chat", first.ID, 8)
	require.NoError(t, err)
	assert.False(t, entered)
	for _, target := range []int64{first.ID, second.ID} {
		require.NoError(t, store.SetChatFocus(ctx, "owner", "chat", target))
		advanced, err = store.AdvanceChat(ctx, "owner", "chat", first.ID, 9)
		require.NoError(t, err)
		assert.False(t, advanced)
		focus, cursor, isEntered, err := store.ChatCursor(ctx, "owner", "chat")
		require.NoError(t, err)
		assert.Equal(t, target, focus)
		assert.Zero(t, cursor)
		assert.False(t, isEntered)
	}
	entered, err = store.EnterChat(ctx, "owner", "chat", first.ID, 9)
	require.NoError(t, err)
	assert.False(t, entered)
	_, cursor, entered, err := store.ChatCursor(ctx, "owner", "other-chat")
	require.NoError(t, err)
	assert.Zero(t, cursor)
	assert.False(t, entered)
}

func TestChatEntry_SnapshotsLastRenderableRowsAndNewestLifecyclePosition(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, db, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	rows, newest, err := store.ChatEntry(ctx, root.ID, 3)
	require.NoError(t, err)
	assert.Empty(t, rows)
	assert.Zero(t, newest)
	var ids []int64
	for _, text := range []string{"one", "two", "three", "four"} {
		ids = append(ids, chatTestOutput(t, store, root.ID, text))
	}
	lifecycle := chatTestOutput(t, store, root.ID, "not rendered")
	_, err = db.ExecContext(ctx, "UPDATE session_outbox SET type = 'session_replaced' WHERE id = ?", lifecycle)
	require.NoError(t, err)
	other := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, other.ID, "other tail")
	rows, newest, err = store.ChatEntry(ctx, root.ID, 3)
	require.NoError(t, err)
	require.Len(t, rows, 3)
	assert.Equal(t, lifecycle, newest)
	for i, row := range rows {
		assert.Equal(t, ids[i+1], row.ID)
		assert.Equal(t, "test", row.Name)
	}
}
