package sessionstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestInboxStore_PromoteReactivatesOnlyFirstAcceptance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, rec.ID, SessionStatusCompleted))
	input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, "durable input")
	require.NoError(t, err)

	first, err := store.PromoteInput(ctx, input.ID, "[stamp] durable input")
	require.NoError(t, err)
	reloaded, err := store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, SessionStatusActive, reloaded.Status)

	require.NoError(t, store.UpdateSessionStatus(ctx, rec.ID, SessionStatusCompleted))
	second, err := store.PromoteInput(ctx, input.ID, "ignored retry content")
	require.NoError(t, err)
	assert.Equal(t, first.ID, second.ID)

	reloaded, err = store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusCompleted, reloaded.Status, "accepted retry must not reopen the session")
}

func TestInboxStore_PromoteRollsBackWhenLifecycleGuardLoses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block func(*testing.T, context.Context, Store, *sql.DB, int64)
	}{
		{
			name: "stopping",
			block: func(t *testing.T, ctx context.Context, store Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusStopping))
			},
		},
		{
			name: "stopped",
			block: func(t *testing.T, ctx context.Context, store Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusStopped))
			},
		},
		{
			name: "terminating",
			block: func(t *testing.T, ctx context.Context, store Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusTerminating))
			},
		},
		{
			name: "killed status",
			block: func(t *testing.T, ctx context.Context, store Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusKilled))
			},
		},
		{
			name: "killed_at",
			block: func(t *testing.T, ctx context.Context, _ Store, db *sql.DB, sessionID int64) {
				_, err := db.ExecContext(
					ctx,
					`UPDATE sessions SET killed_at = ? WHERE id = ?`,
					time.Now().UTC(),
					sessionID,
				)
				require.NoError(t, err)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store, db, projectID := newTestStore(t)
			rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
			require.NoError(t, err)
			input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, "durable input")
			require.NoError(t, err)
			tt.block(t, ctx, store, db, rec.ID)

			_, err = store.PromoteInput(ctx, input.ID, "[stamp] durable input")
			require.ErrorIs(t, err, ErrSessionNotAcceptingInput)

			var messageCount int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM messages WHERE session_id = ?`, rec.ID,
			).Scan(&messageCount))
			assert.Zero(t, messageCount, "failed activation must roll back the inserted user row")

			var state string
			var acceptedMessageID sql.NullInt64
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT state, accepted_message_id FROM session_inbox WHERE id = ?`, input.ID,
			).Scan(&state, &acceptedMessageID))
			assert.Equal(t, "pending", state, "failed activation must roll back the inbox transition")
			assert.False(t, acceptedMessageID.Valid)
		})
	}
}

func TestInboxStore_ListSessionsWithRecoverableInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	pending := createPendingRecoveryFixtures(ctx, t, store, projectID)
	accepted := createAcceptedRecoveryFixtures(ctx, t, store, projectID)
	createExcludedRecoveryFixtures(ctx, t, store, projectID)

	recoverable, err := store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.Equal(t, append(pending, accepted...), recoverable)
}

func createPendingRecoveryFixtures(ctx context.Context, t *testing.T, store Store, projectID int64) []int64 {
	t.Helper()

	var ids []int64
	for _, content := range []string{"first pending", "second pending"} {
		rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
		require.NoError(t, err)
		_, err = store.EnqueueInput(ctx, rec.ID, InputSourceUser, content)
		require.NoError(t, err)
		ids = append(ids, rec.ID)
	}

	stopped, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	_, err = store.EnqueueInput(ctx, stopped.ID, InputSourceUser, "parked pending")
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, stopped.ID, SessionStatusStopped))

	killed, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	_, err = store.EnqueueInput(ctx, killed.ID, InputSourceUser, "killed pending")
	require.NoError(t, err)
	require.NoError(t, store.MarkSessionKilled(ctx, killed.ID))

	return ids
}

func createAcceptedRecoveryFixtures(
	ctx context.Context,
	t *testing.T,
	store Store,
	projectID int64,
) []int64 {
	t.Helper()

	var ids []int64
	for i, suffix := range []string{"user last", "tool progress", "final assistant persisted"} {
		rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
		require.NoError(t, err)
		input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, suffix)
		require.NoError(t, err)
		promoted, err := store.PromoteInput(ctx, input.ID, suffix)
		require.NoError(t, err)

		switch i {
		case 1:
			_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{Role: "tool", Content: "durable result"})
			require.NoError(t, err)
		case 2:
			_, err = store.InsertMessage(ctx, rec.ID, &transcript.Message{Role: "assistant", Content: "durable final"})
			require.NoError(t, err)
		}

		if i == 1 {
			require.NoError(t, store.MarkCompacted(ctx, []int64{promoted.ID}))
		}

		accepted, err := store.HasAcceptedInput(ctx, rec.ID)
		require.NoError(t, err)
		assert.True(t, accepted)
		ids = append(ids, rec.ID)
	}

	return ids
}

func createExcludedRecoveryFixtures(ctx context.Context, t *testing.T, store Store, projectID int64) {
	t.Helper()

	tests := []struct {
		status SessionStatus
		kill   bool
	}{
		{status: SessionStatusCompleted},
		{status: SessionStatusStopping},
		{status: SessionStatusStopped},
		{status: SessionStatusTerminating},
		{status: SessionStatusKilled},
		{status: SessionStatusActive, kill: true},
	}
	for _, fixture := range tests {
		rec, err := store.CreateSession(ctx, projectID, "model", "", nil)
		require.NoError(t, err)
		input, err := store.EnqueueInput(ctx, rec.ID, InputSourceUser, "excluded")
		require.NoError(t, err)
		_, err = store.PromoteInput(ctx, input.ID, "excluded")
		require.NoError(t, err)
		if fixture.kill {
			require.NoError(t, store.MarkSessionKilled(ctx, rec.ID))
		} else {
			require.NoError(t, store.UpdateSessionStatus(ctx, rec.ID, fixture.status))
		}
	}

	headerOnly, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	_, err = store.InsertMessage(ctx, headerOnly.ID, &transcript.Message{Role: "user", Content: "AGENTS.md header"})
	require.NoError(t, err)
	accepted, err := store.HasAcceptedInput(ctx, headerOnly.ID)
	require.NoError(t, err)
	assert.False(t, accepted, "a plain user-role header is not accepted controller input")

	handled, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, handled.ID, InputSourceUser, "/status")
	require.NoError(t, err)
	require.NoError(t, store.HandleInput(ctx, input.ID, "status command"))
}
