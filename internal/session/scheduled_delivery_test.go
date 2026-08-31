package session

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
)

func newScheduledDeliverySessionStore(t *testing.T) (sessionstore.Store, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	result, err := db.ExecContext(
		ctx, `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)
	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	return store, record.ID
}

func TestMessageStore_ScheduledNotificationDeliveryIsExactlyOnce(t *testing.T) {
	store, sessionID := newScheduledDeliverySessionStore(t)
	ms := newMessageStore(store, sessionID, nil)

	applied, err := ms.addToolNotificationPairOnce(
		context.Background(), "schedule:one-shot:7", "call-1", "schedule", "due",
	)
	require.NoError(t, err)
	assert.True(t, applied)

	applied, err = ms.addToolNotificationPairOnce(
		context.Background(), "schedule:one-shot:7", "call-2", "schedule", "due",
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Len(t, ms.getMessages(), 2)

	_, err = ms.addToolNotificationPairOnce(
		context.Background(), "schedule:one-shot:7", "call-3", "schedule", "different",
	)
	require.ErrorIs(t, err, sessionstore.ErrDeliveryConflict)

	stored, err := store.LoadActiveMessages(context.Background(), sessionID)
	require.NoError(t, err)
	assert.Len(t, stored, 2)
}

func TestSession_FreshScheduledDeliveryResetsExactlyOnce(t *testing.T) {
	store, sessionID := newScheduledDeliverySessionStore(t)
	s := &svc{
		id:           sessionID,
		agentsMD:     "PROJECT RULES",
		ms:           newMessageStore(store, sessionID, nil),
		loopDetector: newLoopDetector(),
		todoStore:    todo.New(),
		store:        store,
		prompt:       newPromptBuilder(testPrompt, "", ""),
	}
	require.NoError(t, s.ms.addUserMessage(context.Background(), "old task"))

	applied, err := s.ResetContextAndInjectOnce(
		context.Background(), "schedule:cron:9:20260814T1200Z", "fresh task",
	)
	require.NoError(t, err)
	assert.True(t, applied)
	first := s.ms.getMessages()
	require.Len(t, first, 2)
	assert.Contains(t, first[1].Content, "fresh task")

	applied, err = s.ResetContextAndInjectOnce(
		context.Background(), "schedule:cron:9:20260814T1200Z", "fresh task",
	)
	require.NoError(t, err)
	assert.False(t, applied)
	assert.Equal(t, first, s.ms.getMessages())

	_, err = s.ResetContextAndInjectOnce(
		context.Background(), "schedule:cron:9:20260814T1200Z", "different task",
	)
	require.ErrorIs(t, err, sessionstore.ErrDeliveryConflict)
}
