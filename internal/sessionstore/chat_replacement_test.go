package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatFocus_ClearTransfersEveryChatAtomically(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db, project := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	require.NoError(t, store.SetChatFocus(ctx, "owner", "one", old.ID))
	require.NoError(t, store.SetChatFocus(ctx, "owner", "two", old.ID))
	_, err = db.ExecContext(ctx, `INSERT INTO chat_focus(manager_id, chat_id, session_id)
		VALUES ('other', 'chat', ?);
		CREATE TRIGGER fail_focus_transfer BEFORE UPDATE OF session_id ON chat_focus
		WHEN OLD.chat_id = 'two' BEGIN SELECT RAISE(ABORT, 'interrupted focus transfer'); END`, old.ID)
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, old.ID, InputSourceUser, "/clear")
	require.NoError(t, err)
	_, _, err = store.ReplaceManagerRootForInput(ctx, old.ID, input.ID, "test", "/project")
	require.ErrorContains(t, err, "interrupted focus transfer")
	record, err := store.GetSession(ctx, old.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusActive, record.Status)
	assertInputState(t, db, input.ID, InputStatePending)
	assertChatFocusRows(t, store, old.ID)
	var count int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions`).Scan(&count))
	assert.Equal(t, 1, count, "the successor rolls back with the focus update")
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox`).Scan(&count))
	assert.Equal(t, 1, count, "replacement and clear outputs also roll back")
	_, err = db.ExecContext(ctx, `DROP TRIGGER fail_focus_transfer`)
	require.NoError(t, err)
	replacement, _, err := store.ReplaceManagerRootForInput(ctx, old.ID, input.ID, "test", "/project")
	require.NoError(t, err)
	assertChatFocusRows(t, store, replacement.ID)
	assertInputState(t, db, input.ID, InputStateHandled)
	outputs, err := store.ChatOutputs(ctx, "owner", "one", replacement.ID, 10)
	require.NoError(t, err)
	require.Len(t, outputs, 2)
	assert.Equal(t, OutputSessionReplaced, outputs[0].Type)
	assert.Equal(t, "Session cleared.", outputs[1].Content)
}

func assertChatFocusRows(t *testing.T, store *Store, want int64) {
	t.Helper()

	for _, key := range [][2]string{{"owner", "one"}, {"owner", "two"}, {"other", "chat"}} {
		focus, err := store.ChatFocus(t.Context(), key[0], key[1])
		require.NoError(t, err)
		assert.Equal(t, want, focus, "manager %s chat %s", key[0], key[1])
	}
}
