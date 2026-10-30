package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLastSeenChatOutput_WatermarkAndRenderableRows(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, db, project := newTestStore(t)
	root, opened, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	_, found, err := store.LastSeenChatOutput(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	assert.False(t, found)
	require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, opened.OutputID))
	_, found, err = store.LastSeenChatOutput(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	assert.False(t, found)
	seen := chatTestOutput(t, store, root.ID, "seen answer")
	for _, kind := range []OutputType{OutputSessionOpened, OutputSessionReplaced} {
		id := chatTestOutput(t, store, root.ID, "lifecycle fixture")
		_, err := db.ExecContext(ctx, `UPDATE session_outbox SET type = ? WHERE id = ?`, kind, id)
		require.NoError(t, err)
		require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, id))
	}
	chatTestOutput(t, store, root.ID, "unread answer")
	row, found, err := store.LastSeenChatOutput(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, ChatOutput{
		ID: seen, SessionID: root.ID, Name: "test", Type: OutputMessage, Content: "seen answer",
	}, row)
	for _, key := range [][2]string{{"owner", "other-chat"}, {"other", "chat"}} {
		_, otherFound, err := store.LastSeenChatOutput(ctx, key[0], key[1], root.ID)
		require.NoError(t, err)
		assert.False(t, otherFound)
	}
	closed, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 0)
	require.NoError(t, err)
	require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, closed.OutputID))
	row, found, err = store.LastSeenChatOutput(ctx, "owner", "chat", root.ID)
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, OutputSessionClosed, row.Type)
}

func TestChatOutputs_DisplayNameSurvivesKill(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, root.ID, "answer")
	for _, title := range []string{"", "chosen title"} {
		if title != "" {
			require.NoError(t, store.SetSessionTitle(ctx, "owner", root.ID, title))
		}
		rows, err := store.ChatOutputs(ctx, "owner", "chat", root.ID, 10)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		want := "test"
		if title != "" {
			want += " · " + title
		}
		assert.Equal(t, want, rows[0].Name)
	}
	_, err := store.MarkSessionKilledWithOutput(ctx, root.ID, 0)
	require.NoError(t, err)
	rows, err := store.ChatOutputs(ctx, "owner", "chat", root.ID, 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, row := range rows {
		assert.Equal(t, "test · chosen title", row.Name)
	}
}
