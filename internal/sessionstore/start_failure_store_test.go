package sessionstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartFailureLedgerSurvivesRestartRetryAndDelivery(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := t.Context()
	root, err := store.CreateSession(ctx, projectID, "removed-model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	input, err := enqueueInput(ctx, store, root.ID, InputSourceUser, "preserve this task")
	require.NoError(t, err)
	for i := range 100 {
		reported, err := store.RecordSessionStartFailure(ctx, root.ID, fmt.Sprintf("failure variant %d", i))
		require.NoError(t, err)
		assert.Equal(t, i == 0, reported)
		store = testStore(db)
	}
	pending, err := store.PeekPending(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, input.ID, pending.ID)
	rows, err := store.ChatOutputs(ctx, root.ID, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "failure variant 0", rows[0].Content)
	store = testStore(db)
	reported, err := store.RecordSessionStartFailure(ctx, root.ID, "retry before send")
	require.NoError(t, err)
	assert.False(t, reported)
	replayed, err := store.ChatOutputs(ctx, root.ID, 0, 100)
	require.NoError(t, err)
	assert.Equal(t, rows, replayed)
	require.NoError(t, store.SetChatFocus(ctx, "telegram", "chat", root.ID))
	entered, err := store.EnterChat(ctx, "telegram", "chat", root.ID, rows[0].ID)
	require.NoError(t, err)
	require.True(t, entered)
	store = testStore(db)
	reported, err = store.RecordSessionStartFailure(ctx, root.ID, "retry after send")
	require.NoError(t, err)
	assert.False(t, reported)
	_, cursor, entered, err := store.ChatCursor(ctx, "telegram", "chat")
	require.NoError(t, err)
	require.True(t, entered)
	tail, err := store.ChatOutputs(ctx, root.ID, cursor, 100)
	require.NoError(t, err)
	assert.Empty(t, tail)
	_, err = acceptInput(ctx, store, input.ID, input.RawContent)
	require.NoError(t, err)
	_, err = enqueueInput(ctx, store, root.ID, InputSourceUser, "new task after recovery")
	require.NoError(t, err)
	reported, err = store.RecordSessionStartFailure(ctx, root.ID, "failure for new work")
	require.NoError(t, err)
	assert.True(t, reported)
}

func TestStartFailureRespectsStopAndKillFences(t *testing.T) {
	for _, status := range []SessionStatus{SessionStatusStopping, SessionStatusStopped, SessionStatusTerminating, SessionStatusKilled} {
		t.Run(string(status), func(t *testing.T) {
			store, _, projectID := newTestStore(t)
			root, err := store.CreateSession(
				t.Context(),
				projectID,
				"model",
				"",
				map[string]any{"manager_id": "telegram"},
			)
			require.NoError(t, err)
			require.NoError(t, store.UpdateSessionStatus(t.Context(), root.ID, status))
			reported, err := store.RecordSessionStartFailure(t.Context(), root.ID, "failure")
			require.NoError(t, err)
			assert.False(t, reported)
			current, err := store.GetSession(t.Context(), root.ID)
			require.NoError(t, err)
			assert.Equal(t, status, current.Status)
		})
	}
}

func TestStartFailureConcurrentProducersCommitOneReceipt(t *testing.T) {
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(t.Context(), projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	_, err = enqueueInput(t.Context(), store, root.ID, InputSourceUser, "work")
	require.NoError(t, err)
	var wg sync.WaitGroup
	var reports atomic.Int64
	failures := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			reported, err := store.RecordSessionStartFailure(t.Context(), root.ID, fmt.Sprintf("failure %d", i))
			if reported {
				reports.Add(1)
			}
			failures <- err
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), reports.Load())
	var outputs int
	require.NoError(
		t,
		db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, root.ID).
			Scan(&outputs),
	)
	assert.Equal(t, 1, outputs)
}

func TestStartFailureRollsBackWhenReceiptCannotBeWritten(t *testing.T) {
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(t.Context(), projectID, "model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	input, err := enqueueInput(t.Context(), store, root.ID, InputSourceUser, "work")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_error_receipt BEFORE INSERT ON session_outbox
		BEGIN SELECT RAISE(ABORT, 'outbox unavailable'); END`)
	require.NoError(t, err)
	reported, err := store.RecordSessionStartFailure(t.Context(), root.ID, "failed")
	require.Error(t, err)
	assert.False(t, reported)
	record, err := store.GetSession(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusActive, record.Status)
	pending, err := store.PeekPending(t.Context(), root.ID)
	require.NoError(t, err)
	assert.Equal(t, input.ID, pending.ID)
}
