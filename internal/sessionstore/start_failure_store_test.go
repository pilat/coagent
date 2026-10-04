package sessionstore

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartFailureLedgerSurvivesRestartRetryAndDelivery(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := t.Context()
	root, err := store.CreateSession(ctx, projectID, "removed-model", "", map[string]any{"manager_id": "telegram"})
	require.NoError(t, err)
	require.NoError(t, store.BindManager(ctx, "telegram", "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	}))
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
	claim, err := store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.Equal(t, "failure variant 0", claim.Output.Content)
	require.NoError(
		t,
		store.RetryOutput(
			ctx,
			"telegram",
			claim.Output.ID,
			claim.Output.AttemptID,
			"temporary transport failure",
			time.Now().Add(-time.Second),
		),
	)
	store = testStore(db)
	reported, err := store.RecordSessionStartFailure(ctx, root.ID, "recovered daemon still cannot start")
	require.NoError(t, err)
	assert.False(t, reported)
	retry, err := store.ClaimOutputHead(ctx, "telegram")
	require.NoError(t, err)
	assert.Equal(t, claim.Output.ID, retry.Output.ID)
	require.NoError(t, store.AckOutput(ctx, "telegram", retry.Output.ID, retry.Output.AttemptID, []string{"1"}, nil))
	store = testStore(db)
	reported, err = store.RecordSessionStartFailure(ctx, root.ID, "another retry after delivery")
	require.NoError(t, err)
	assert.False(t, reported)
	_, err = store.ClaimOutputHead(ctx, "telegram")
	require.ErrorIs(t, err, ErrNoOutput)
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
	store, _, projectID := newTestStore(t)
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
	status, err := store.OutputQueueStatus(t.Context(), "telegram")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)
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
