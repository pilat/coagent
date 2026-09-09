package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type asyncInboxModelRow struct {
	source InputSource
	state  InputState
}

func TestHarnessModel_AsyncInboxMixedFIFOStopErrorKillAndRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	model := []asyncInboxModelRow{
		{source: InputSourceProcess, state: InputStatePending},
		{source: InputSourceUser, state: InputStatePending},
		{source: InputSourceSubagent, state: InputStatePending},
	}
	process, err := store.EnqueueAsyncInput(ctx, record.ID, InputSourceProcess, "process", nil)
	require.NoError(t, err)
	user, err := store.EnqueueInput(ctx, record.ID, InputSourceUser, "user")
	require.NoError(t, err)
	child, err := store.EnqueueAsyncInput(ctx, record.ID, InputSourceSubagent, "subagent", nil)
	require.NoError(t, err)
	assertAsyncInboxMatchesModel(t, db, record.ID, model)

	for i, input := range []*InboxInput{process, user, child} {
		head, peekErr := store.PeekPending(ctx, record.ID)
		require.NoError(t, peekErr)
		assert.Equal(t, input.ID, head.ID, "production peek preserves the mixed-source FIFO")
		message, promoteErr := store.PromoteInput(ctx, input.ID, input.RawContent)
		require.NoError(t, promoteErr)
		model[i].state = InputStateAccepted
		duplicate, duplicateErr := store.PromoteInput(ctx, input.ID, "changed duplicate")
		require.NoError(t, duplicateErr)
		assert.Equal(t, message.ID, duplicate.ID)
		assertAsyncInboxMatchesModel(t, db, record.ID, model)
	}

	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusError))
	_, err = store.EnqueueAsyncInput(ctx, record.ID, InputSourceProcess, "retained error fact", nil)
	require.NoError(t, err)
	model = append(model, asyncInboxModelRow{source: InputSourceProcess, state: InputStatePending})
	store = NewStore(db)
	recoverable, err := store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.NotContains(t, recoverable, record.ID, "async-only error input stays parked")
	_, err = store.EnqueueInput(ctx, record.ID, InputSourceUser, "explicit retry")
	require.NoError(t, err)
	model = append(model, asyncInboxModelRow{source: InputSourceUser, state: InputStatePending})
	recoverable, err = store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.Contains(t, recoverable, record.ID)

	count, err := store.CancelPendingInputsForStop(ctx, []int64{record.ID}, "stopped")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)
	model[4].state = InputStateCancelled
	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusStopped))
	store = NewStore(db)
	recoverable, err = store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.NotContains(t, recoverable, record.ID)
	_, err = store.EnqueueInput(ctx, record.ID, InputSourceAgent, "explicit resume")
	require.NoError(t, err)
	model = append(model, asyncInboxModelRow{source: InputSourceAgent, state: InputStatePending})
	recoverable, err = store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.Contains(t, recoverable, record.ID)
	assertAsyncInboxMatchesModel(t, db, record.ID, model)

	require.NoError(t, store.MarkSessionKilled(ctx, record.ID))
	model[3].state = InputStateCancelled
	model[5].state = InputStateCancelled
	store = NewStore(db)
	recoverable, err = store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.NotContains(t, recoverable, record.ID)
	assertAsyncInboxMatchesModel(t, db, record.ID, model)
}

func assertAsyncInboxMatchesModel(
	t *testing.T,
	db *sql.DB,
	sessionID int64,
	model []asyncInboxModelRow,
) {
	t.Helper()

	rows, err := db.QueryContext(context.Background(), `SELECT source, state FROM session_inbox
		WHERE session_id = ? ORDER BY id`, sessionID)
	require.NoError(t, err)
	defer rows.Close()

	var got []asyncInboxModelRow
	for rows.Next() {
		var source, state string
		require.NoError(t, rows.Scan(&source, &state))
		got = append(got, asyncInboxModelRow{source: InputSource(source), state: InputState(state)})
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, model, got)
}
