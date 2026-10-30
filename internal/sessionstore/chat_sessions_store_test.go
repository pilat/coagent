package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatSessions_LiveOwnedRootsAndRenderedUnread(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, db, project := newTestStore(t)
	assistant, err := store.EnsureAssistantRoot(ctx, project, "owner", "/assistant", "model", "")
	require.NoError(t, err)
	assistantOutput := chatTestOutput(t, store, assistant.ID, "assistant answer")
	old, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	root, _, err := store.ReplaceManagerRoot(ctx, old.ID, "test", "/project")
	require.NoError(t, err)
	first := chatTestOutput(t, store, root.ID, "first answer")
	latest := chatTestOutput(t, store, root.ID, "latest answer\nsecond line")
	require.NoError(t, store.MarkChatSeen(ctx, "owner", "chat", root.ID, first))
	stopped := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.UpdateSessionStatus(ctx, stopped.ID, SessionStatusStopped))
	foreign := chatTestRoot(t, store, project, "foreign")
	chatTestOutput(t, store, foreign.ID, "foreign answer")
	killed := chatTestRoot(t, store, project, "owner")
	chatTestOutput(t, store, killed.ID, "unread killed answer")
	_, err = store.MarkSessionKilledWithOutput(ctx, killed.ID, 0)
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(ctx, project, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET attributes = '{"manager_id":"owner"}' WHERE id = ?`, child)
	require.NoError(t, err)

	sessions, err := store.ChatSessions(ctx, "owner", "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 3)
	assert.Equal(t, ChatSession{
		ID: 0, SessionID: assistant.ID, Name: "Assistant", Unread: 1,
		LatestUnread: "assistant answer", LatestUnreadID: assistantOutput,
	}, sessions[0])
	byID := make(map[int64]ChatSession)
	for _, session := range sessions {
		byID[session.ID] = session
	}
	assert.Equal(t, ChatSession{
		ID: root.ID, SessionID: root.ID, Name: "test",
		Unread: 1, LatestUnread: "latest answer\nsecond line", LatestUnreadID: latest,
	}, byID[root.ID])
	require.Contains(t, byID, stopped.ID)
	assert.Zero(t, byID[stopped.ID].Unread)
	assert.Empty(t, byID[stopped.ID].LatestUnread)
	assert.Zero(t, byID[stopped.ID].LatestUnreadID)
}

func TestChatSessions_SessionTitle(t *testing.T) {
	t.Parallel()
	store, _, project := newTestStore(t)
	root := chatTestRoot(t, store, project, "owner")
	require.NoError(t, store.SetSessionTitle(t.Context(), "owner", root.ID, "fix login 500"))
	sessions, err := store.ChatSessions(t.Context(), "owner", "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, "test · fix login 500", sessions[0].Name)
}

func TestChatSessions_LifecycleRowsDoNotCountAsUnread(t *testing.T) {
	t.Parallel()

	ctx := t.Context()
	store, _, project := newTestStore(t)
	root, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: project, Model: "model", Attributes: map[string]any{"manager_id": "owner"},
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	sessions, err := store.ChatSessions(ctx, "owner", "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Zero(t, sessions[0].Unread)
	replacement, _, err := store.ReplaceManagerRoot(ctx, root.ID, "test", "/project")
	require.NoError(t, err)
	sessions, err = store.ChatSessions(ctx, "owner", "chat")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, replacement.ID, sessions[0].ID)
	assert.Zero(t, sessions[0].Unread)
	chatTestOutput(t, store, replacement.ID, "new output")
	sessions, err = store.ChatSessions(ctx, "owner", "other-chat")
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.EqualValues(t, 1, sessions[0].Unread)
}
