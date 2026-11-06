package sessionstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatSessions_ScheduledRoots(t *testing.T) {
	t.Parallel()
	store, db, project := newTestStore(t)
	cron := chatTestRoot(t, store, project, "owner")
	once := chatTestRoot(t, store, project, "owner")
	sleep := chatTestRoot(t, store, project, "owner")
	for _, id := range []int64{cron.ID} {
		_, err := db.ExecContext(
			t.Context(),
			`INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *', '')`,
			id,
		)
		require.NoError(t, err)
	}
	for id, meta := range map[int64]string{once.ID: `{}`, sleep.ID: `{"tool_call_id":"sleep-call"}`} {
		_, err := db.ExecContext(
			t.Context(),
			`INSERT INTO schedules(session_id,one_shot_at,metadata,input_message) VALUES (?,?,?,'')`,
			id,
			time.Now().Add(time.Hour),
			meta,
		)
		require.NoError(t, err)
	}
	entries, err := store.ChatSessions(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, entries, 3)
	for _, entry := range entries {
		assert.Equal(t, entry.ID != sleep.ID, entry.Scheduled)
	}
}

func TestChatSessions_LiveOwnedRootsInActivityOrder(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db, project := newTestStore(t)
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	root, _, err := store.ReplaceManagerRoot(ctx, old.ID, "test", "/project")
	require.NoError(t, err)
	chatTestOutput(t, store, root.ID, "first answer")
	chatTestOutput(t, store, root.ID, "latest answer\nsecond line")
	stopped := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.UpdateSessionStatus(ctx, stopped.ID, SessionStatusStopped))
	foreign := chatTestRoot(t, store, project, "foreign")
	chatTestOutput(t, store, foreign.ID, "foreign answer")
	killed := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, killed.ID, "killed answer")
	_, err = store.MarkSessionKilledWithOutput(ctx, killed.ID, 0)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(ctx, project, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET attributes = '{"manager_id":"owner"}' WHERE id = ?`, child)
	require.NoError(t, err)

	sessions, err := store.ChatSessions(ctx, "owner")
	require.NoError(t, err)
	require.Len(t, sessions, 2)
	byID := make(map[int64]ChatSession)
	for _, session := range sessions {
		byID[session.ID] = session
	}
	assert.Equal(t, ChatSession{
		ID: root.ID, Name: "test",
	}, byID[root.ID])
	require.Contains(t, byID, stopped.ID)
	assert.Equal(t, stopped.ID, sessions[0].ID)
}

func TestChatSessions_SessionTitle(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.SetSessionTitle(t.Context(), "owner", root.ID, "fix login 500"))
	sessions, err := store.ChatSessions(t.Context(), "owner")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "test · fix login 500", sessions[0].Name)
}
