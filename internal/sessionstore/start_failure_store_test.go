package sessionstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestStartFailureLedgerSurvivesRestartRetryAndDelivery(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := t.Context()
	root := sessionstoretest.BareRoot(t, store.db, projectID, "removed-model", "", map[string]any{})
	input, err := enqueueInput(ctx, store, root, InputSourceUser, "preserve this task")
	require.NoError(t, err)
	for i := range 100 {
		reported, err := store.RecordSessionStartFailure(ctx, root, fmt.Sprintf("failure variant %d", i))
		require.NoError(t, err)
		assert.Equal(t, i == 0, reported)
		store = testStore(db)
	}
	pending, err := store.PeekPending(ctx, root)
	require.NoError(t, err)
	assert.Equal(t, input.ID, pending.ID)
	rows, err := store.ChatOutputs(ctx, root, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "failure variant 0", rows[0].Content)
	store = testStore(db)
	reported, err := store.RecordSessionStartFailure(ctx, root, "retry before send")
	require.NoError(t, err)
	assert.False(t, reported)
	replayed, err := store.ChatOutputs(ctx, root, 0, 100)
	require.NoError(t, err)
	assert.Equal(t, rows, replayed)
	require.NoError(t, store.SetChatFocus(ctx, "telegram", "chat", root))
	entered, err := store.EnterChat(ctx, "telegram", "chat", root, rows[0].ID)
	require.NoError(t, err)
	require.True(t, entered)
	store = testStore(db)
	reported, err = store.RecordSessionStartFailure(ctx, root, "retry after send")
	require.NoError(t, err)
	assert.False(t, reported)
	_, cursor, entered, err := store.ChatCursor(ctx, "telegram", "chat")
	require.NoError(t, err)
	require.True(t, entered)
	tail, err := store.ChatOutputs(ctx, root, cursor, 100)
	require.NoError(t, err)
	assert.Empty(t, tail)
	_, err = acceptInput(ctx, store, input.ID, input.RawContent)
	require.NoError(t, err)
	_, err = enqueueInput(ctx, store, root, InputSourceUser, "new task after recovery")
	require.NoError(t, err)
	reported, err = store.RecordSessionStartFailure(ctx, root, "failure for new work")
	require.NoError(t, err)
	assert.True(t, reported)
}

func TestStartFailureRespectsStopAndKillFences(t *testing.T) {
	for _, status := range []SessionStatus{SessionStatusStopping, SessionStatusStopped, SessionStatusTerminating, SessionStatusKilled} {
		t.Run(string(status), func(t *testing.T) {
			store, _, projectID := newTestStore(t)
			root := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", map[string]any{})
			require.NoError(t, store.UpdateSessionStatus(t.Context(), root, status))
			reported, err := store.RecordSessionStartFailure(t.Context(), root, "failure")
			require.NoError(t, err)
			assert.False(t, reported)
			current, err := store.GetSession(t.Context(), root)
			require.NoError(t, err)
			assert.Equal(t, status, current.Status)
		})
	}
}

func TestStartFailureConcurrentProducersCommitOneReceipt(t *testing.T) {
	store, db, projectID := newTestStore(t)
	root := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", map[string]any{})
	_, err := enqueueInput(t.Context(), store, root, InputSourceUser, "work")
	require.NoError(t, err)
	var wg sync.WaitGroup
	var reports atomic.Int64
	failures := make(chan error, 32)
	for i := range 32 {
		wg.Go(func() {
			reported, err := store.RecordSessionStartFailure(t.Context(), root, fmt.Sprintf("failure %d", i))
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
		db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, root).
			Scan(&outputs),
	)
	assert.Equal(t, 1, outputs)
}

func TestStartFailureRollsBackWhenReceiptCannotBeWritten(t *testing.T) {
	store, db, projectID := newTestStore(t)
	root := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", map[string]any{})
	input, err := enqueueInput(t.Context(), store, root, InputSourceUser, "work")
	require.NoError(t, err)
	_, err = db.ExecContext(t.Context(), `CREATE TRIGGER fail_error_receipt BEFORE INSERT ON session_outbox
		BEGIN SELECT RAISE(ABORT, 'outbox unavailable'); END`)
	require.NoError(t, err)
	reported, err := store.RecordSessionStartFailure(t.Context(), root, "failed")
	require.Error(t, err)
	assert.False(t, reported)
	record, err := store.GetSession(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusActive, record.Status)
	pending, err := store.PeekPending(t.Context(), root)
	require.NoError(t, err)
	assert.Equal(t, input.ID, pending.ID)
}
