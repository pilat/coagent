package sessionstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInboxStore_EnqueuePeekAndListFIFO(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	firstSession, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	secondSession, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	first, err := store.EnqueueInput(ctx, secondSession.ID, InputSourceAgent, "first")
	require.NoError(t, err)
	second, err := store.EnqueueInput(ctx, secondSession.ID, InputSourceUser, "second")
	require.NoError(t, err)
	_, err = store.EnqueueInput(ctx, firstSession.ID, InputSourceUser, "third")
	require.NoError(t, err)

	assert.Less(t, first.ID, second.ID)
	assert.Equal(t, InputStatePending, first.State)
	assert.Equal(t, InputSourceAgent, first.Source)

	peeked, err := store.PeekPending(ctx, secondSession.ID)
	require.NoError(t, err)
	require.NotNil(t, peeked)
	assert.Equal(t, first.ID, peeked.ID)
	assert.Equal(t, "first", peeked.RawContent)

	sessionIDs, err := store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.Equal(t, []int64{secondSession.ID, firstSession.ID}, sessionIDs)
}

func TestInboxStore_EnqueueRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	tests := []struct {
		name   string
		status string
		killed bool
	}{
		{name: "killed status", status: "killed"},
		{name: "terminating status", status: "terminating"},
		{name: "stopping status", status: "stopping"},
		{name: "killed timestamp", status: "active", killed: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
			require.NoError(t, err)

			var killedAt any
			if tt.killed {
				killedAt = time.Now().UTC()
			}
			_, err = db.ExecContext(ctx,
				`UPDATE sessions SET status = ?, killed_at = ? WHERE id = ?`,
				tt.status, killedAt, rec.ID,
			)
			require.NoError(t, err)

			_, err = store.EnqueueInput(ctx, rec.ID, InputSourceUser, "hello")
			require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
		})
	}

	_, err := store.EnqueueInput(ctx, 999_999, InputSourceUser, "hello")
	require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
	_, err = store.EnqueueInput(ctx, 1, InputSource("system"), "hello")
	require.ErrorContains(t, err, "invalid input source")
	_, err = store.EnqueueInput(ctx, 1, InputSourceUser, "")
	require.ErrorContains(t, err, "empty input content")
}

func TestInboxStore_PromoteIsAtomicAndIdempotent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, "durable input")
	require.NoError(t, err)

	first, err := store.PromoteInput(ctx, input.ID, "[stamp] durable input")
	require.NoError(t, err)
	second, err := store.PromoteInput(ctx, input.ID, "ignored retry content")
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID)
	assert.Equal(t, rec.ID, first.SessionID)
	assert.Equal(t, "user", first.Role)
	assert.Equal(t, "[stamp] durable input", first.Content)
	assert.True(t, first.CreatedAt.Equal(input.ReceivedAt))

	var state string
	var acceptedMessageID int64
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT state, accepted_message_id FROM session_inbox WHERE id = ?`, input.ID,
	).Scan(&state, &acceptedMessageID))
	assert.Equal(t, "accepted", state)
	assert.Equal(t, first.ID, acceptedMessageID)

	var messageCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, rec.ID,
	).Scan(&messageCount))
	assert.Equal(t, 1, messageCount)

	peeked, err := store.PeekPending(ctx, rec.ID)
	require.ErrorIs(t, err, ErrNoPendingInput)
	assert.Nil(t, peeked)
}

func TestInboxStore_RejectAndCancelPending(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	firstSession, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	secondSession, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)

	rejected, err := store.EnqueueInput(ctx, firstSession.ID, InputSourceUser, "reject")
	require.NoError(t, err)
	cancelledOne, err := store.EnqueueInput(ctx, firstSession.ID, InputSourceUser, "cancel one")
	require.NoError(t, err)
	cancelledTwo, err := store.EnqueueInput(ctx, secondSession.ID, InputSourceAgent, "cancel two")
	require.NoError(t, err)

	require.NoError(t, store.RejectInput(ctx, rejected.ID, "invalid skill"))
	count, err := store.CancelPendingInputs(ctx, []int64{firstSession.ID}, "stopped")
	require.NoError(t, err)
	assert.Equal(t, int64(1), count)

	tests := []struct {
		id     int64
		state  string
		reason string
	}{
		{id: rejected.ID, state: "rejected", reason: "invalid skill"},
		{id: cancelledOne.ID, state: "cancelled", reason: "stopped"},
		{id: cancelledTwo.ID, state: "pending"},
	}
	for _, tt := range tests {
		var state string
		var reason sql.NullString
		require.NoError(t, db.QueryRowContext(ctx,
			`SELECT state, resolution_reason FROM session_inbox WHERE id = ?`, tt.id,
		).Scan(&state, &reason))
		assert.Equal(t, tt.state, state)
		assert.Equal(t, tt.reason, reason.String)
	}

	err = store.RejectInput(ctx, rejected.ID, "again")
	require.ErrorIs(t, err, ErrInputResolved)
	_, err = store.PromoteInput(ctx, cancelledOne.ID, "cancel one")
	require.ErrorIs(t, err, ErrInputResolved)
	_, err = store.PromoteInput(ctx, 999_999, "missing")
	require.ErrorIs(t, err, ErrInputNotFound)
	_, err = store.CancelPendingInputs(ctx, nil, "")
	require.NoError(t, err)
}

func TestInboxStore_HandleCommandWithoutTranscriptMessage(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, "/status")
	require.NoError(t, err)

	require.NoError(t, store.HandleInput(ctx, input.ID, "status command"))

	var state, reason string
	var messageID sql.NullInt64
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT state, resolution_reason, accepted_message_id
		FROM session_inbox WHERE id = ?`, input.ID,
	).Scan(&state, &reason, &messageID))
	assert.Equal(t, "handled", state)
	assert.Equal(t, "status command", reason)
	assert.False(t, messageID.Valid)

	var messageCount int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, rec.ID,
	).Scan(&messageCount))
	assert.Zero(t, messageCount)

	err = store.HandleInput(ctx, input.ID, "again")
	require.ErrorIs(t, err, ErrInputResolved)
}
