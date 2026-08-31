package sessionstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

func TestScheduledDeliveryStore_ToolNotificationIsExactlyOnceAndConflictsFailClosed(t *testing.T) {
	t.Parallel()

	store, db, projectID := newTestStore(t)
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)

	assistant := &transcript.Message{Role: llmwire.RoleAssistant, ToolCalls: []byte(`[{"id":"c1","name":"schedule"}]`)}
	result := &transcript.Message{
		Role: llmwire.RoleTool, ToolCallID: "c1", ToolName: "schedule", Content: "due",
	}

	asstID, resultID, inserted, err := store.InsertToolNotificationPairOnce(
		ctx, sess.ID, "schedule:one-shot:7", "fingerprint-a", assistant, result,
	)
	require.NoError(t, err)
	assert.True(t, inserted)
	assert.Positive(t, asstID)
	assert.Positive(t, resultID)
	var episodeStartedAt time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT episode_started_at FROM sessions WHERE id = ?`, sess.ID).Scan(&episodeStartedAt))
	assert.False(t, episodeStartedAt.IsZero())

	asstID, resultID, inserted, err = store.InsertToolNotificationPairOnce(
		ctx, sess.ID, "schedule:one-shot:7", "fingerprint-a", assistant, result,
	)
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Zero(t, asstID)
	assert.Zero(t, resultID)
	var replayedEpisodeStart time.Time
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT episode_started_at FROM sessions WHERE id = ?`, sess.ID).Scan(&replayedEpisodeStart))
	assert.Equal(t, episodeStartedAt, replayedEpisodeStart)

	_, _, _, err = store.InsertToolNotificationPairOnce(
		ctx, sess.ID, "schedule:one-shot:7", "different-payload", assistant, result,
	)
	require.ErrorIs(t, err, ErrDeliveryConflict)

	messages, err := store.LoadActiveMessages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2, "producer retry must not append a second synthetic pair")
}

func TestScheduledDeliveryStore_ContextResetIsAtomicIdempotentAndClearsDerivedState(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	_, err = store.InsertMessage(ctx, sess.ID, &transcript.Message{Role: llmwire.RoleUser, Content: "old task"})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionTodoItems(ctx, sess.ID, []byte(`[{"content":"old"}]`)))

	opening := []*transcript.Message{
		{Role: llmwire.RoleUser, Content: "project context"},
		{Role: llmwire.RoleUser, Content: "fresh task"},
	}
	ids, inserted, err := store.ResetSessionContextOnce(
		ctx, sess.ID, "schedule:cron:9:20260814T1200Z", "fresh-a", opening,
	)
	require.NoError(t, err)
	assert.True(t, inserted)
	require.Len(t, ids, 2)

	ids, inserted, err = store.ResetSessionContextOnce(
		ctx, sess.ID, "schedule:cron:9:20260814T1200Z", "fresh-a", opening,
	)
	require.NoError(t, err)
	assert.False(t, inserted)
	assert.Empty(t, ids)

	active, err := store.LoadActiveMessages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, active, 2)
	assert.Equal(t, "project context", active[0].Content)
	assert.Equal(t, "fresh task", active[1].Content)

	var todos string
	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT todo_items FROM sessions WHERE id = ?`, sess.ID,
	).Scan(&todos))
	assert.JSONEq(t, `[]`, todos)
}

func TestScheduledDeliveryStore_ContextResetRollsBackClaimAndTranscriptOnInsertFailure(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()
	sess, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)

	_, err = store.InsertMessage(ctx, sess.ID, &transcript.Message{Role: llmwire.RoleUser, Content: "old task"})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionTodoItems(ctx, sess.ID, []byte(`[{"content":"keep"}]`)))

	_, err = db.ExecContext(ctx, `
		CREATE TRIGGER fail_context_reset_insert
		BEFORE INSERT ON messages
		WHEN NEW.content = 'explode'
		BEGIN
			SELECT RAISE(ABORT, 'forced reset failure');
		END`)
	require.NoError(t, err)

	_, _, err = store.ResetSessionContextOnce(
		ctx,
		sess.ID,
		"schedule:cron:9:20260814T1200Z",
		"fresh-a",
		[]*transcript.Message{
			{Role: llmwire.RoleUser, Content: "partial opening"},
			{Role: llmwire.RoleUser, Content: "explode"},
		},
	)
	require.ErrorContains(t, err, "forced reset failure")

	active, err := store.LoadActiveMessages(ctx, sess.ID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	assert.Equal(t, "old task", active[0].Content)

	var todos string
	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT todo_items FROM sessions WHERE id = ?`, sess.ID,
	).Scan(&todos))
	assert.JSONEq(t, `[{"content":"keep"}]`, todos)

	var claims int
	require.NoError(t, db.QueryRowContext(
		ctx, `SELECT COUNT(*) FROM session_deliveries WHERE session_id = ?`, sess.ID,
	).Scan(&claims))
	assert.Zero(t, claims, "failed transcript mutation must not consume its retry identity")
}
