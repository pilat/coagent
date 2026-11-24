package sessionstore

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
	"github.com/pilat/coagent/internal/transcript"
)

func TestInboxStore_PromoteReactivatesOnlyFirstAcceptance(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	rec := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	require.NoError(t, store.UpdateSessionStatus(ctx, rec, SessionStatusCompleted))
	input, err := enqueueInput(ctx, store, rec, InputSourceUser, "durable input")
	require.NoError(t, err)

	first, err := acceptInput(ctx, store, input.ID, "[stamp] durable input")
	require.NoError(t, err)
	reloaded, err := store.GetSession(ctx, rec)
	require.NoError(t, err)
	require.Equal(t, SessionStatusActive, reloaded.Status)

	require.NoError(t, store.UpdateSessionStatus(ctx, rec, SessionStatusCompleted))
	second, err := acceptInput(ctx, store, input.ID, "ignored retry content")
	require.ErrorIs(t, err, ErrInputResolved)
	assert.Nil(t, second)
	messages, err := store.LoadActiveMessages(ctx, rec)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, first.ID, messages[0].ID)
	assert.Equal(t, "[stamp] durable input", messages[0].Content)

	reloaded, err = store.GetSession(ctx, rec)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusCompleted, reloaded.Status, "accepted retry must not reopen the session")
}

func TestInboxStore_PromoteRollsBackWhenLifecycleGuardLoses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		block func(*testing.T, context.Context, *Store, *sql.DB, int64)
	}{
		{
			name: "stopping",
			block: func(t *testing.T, ctx context.Context, store *Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusStopping))
			},
		},
		{
			name: "terminating",
			block: func(t *testing.T, ctx context.Context, store *Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusTerminating))
			},
		},
		{
			name: "killed status",
			block: func(t *testing.T, ctx context.Context, store *Store, _ *sql.DB, sessionID int64) {
				require.NoError(t, store.UpdateSessionStatus(ctx, sessionID, SessionStatusKilled))
			},
		},
		{
			name: "killed_at",
			block: func(t *testing.T, ctx context.Context, _ *Store, db *sql.DB, sessionID int64) {
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
			rec := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
			input, err := enqueueInput(ctx, store, rec, InputSourceUser, "durable input")
			require.NoError(t, err)
			tt.block(t, ctx, store, db, rec)

			_, err = acceptInput(ctx, store, input.ID, "[stamp] durable input")
			require.ErrorIs(t, err, ErrSessionStopping)

			var messageCount int
			require.NoError(t, db.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM messages WHERE session_id = ?`, rec,
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

func TestInboxStore_StoppedSessionPromotionRequiresExplicitInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		source  InputSource
		wantErr bool
	}{
		{name: "user resumes", source: InputSourceUser},
		{name: "agent resumes", source: InputSourceAgent},
		{name: "process stays parked", source: InputSourceProcess, wantErr: true},
		{name: "subagent stays parked", source: InputSourceSubagent, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			store, db, projectID := newTestStore(t)
			record := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
			var input *InboxInput
			var err error
			if tt.source == InputSourceProcess || tt.source == InputSourceSubagent {
				input, err = enqueueFact(ctx, store, record, tt.source, "durable input", nil)
			} else {
				input, err = enqueueInput(ctx, store, record, tt.source, "durable input")
			}
			require.NoError(t, err)
			require.NoError(t, store.UpdateSessionStatus(ctx, record, SessionStatusStopped))

			_, err = acceptInput(ctx, store, input.ID, "[stamp] durable input")
			if tt.wantErr {
				require.ErrorIs(t, err, ErrSessionNotAcceptingInput)
			} else {
				require.NoError(t, err)
			}

			reloaded, err := store.GetSession(ctx, record)
			require.NoError(t, err)
			if tt.wantErr {
				assert.Equal(t, SessionStatusStopped, reloaded.Status)
				pending, peekErr := store.PeekPending(ctx, record)
				require.NoError(t, peekErr)
				assert.Equal(t, input.ID, pending.ID)

				var messages int
				require.NoError(t, db.QueryRowContext(ctx,
					`SELECT COUNT(*) FROM messages WHERE session_id = ?`, record,
				).Scan(&messages))
				assert.Zero(t, messages)
			} else {
				assert.Equal(t, SessionStatusActive, reloaded.Status)
			}
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

func TestInboxStore_AsyncOnlyErrorAndStoppedSessionsStayParked(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	errorSession := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err := enqueueFact(ctx, store, errorSession, InputSourceProcess, "process", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, errorSession, SessionStatusError))

	stopped := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err = enqueueFact(ctx, store, stopped, InputSourceSubagent, "subagent", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, stopped, SessionStatusStopped))

	recoverable, err := store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.NotContains(t, recoverable, errorSession)
	assert.NotContains(t, recoverable, stopped)

	_, err = enqueueInput(ctx, store, errorSession, InputSourceUser, "retry")
	require.NoError(t, err)
	_, err = enqueueInput(ctx, store, stopped, InputSourceAgent, "resume")
	require.NoError(t, err)
	recoverable, err = store.ListSessionsWithRecoverableInput(ctx)
	require.NoError(t, err)
	assert.Contains(t, recoverable, errorSession)
	assert.Contains(t, recoverable, stopped)
}

func TestInboxStore_ReadOnlyCommandBehindAsyncInputDoesNotResumeStoppedSession(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err := enqueueFact(ctx, store, record, InputSourceProcess, "process", nil)
	require.NoError(t, err)
	_, err = enqueueInput(ctx, store, record, InputSourceUser, "\t/help\n")
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record, SessionStatusStopped))

	recoverable, err := store.ListSessionsWithRecoverableInput(ctx)

	require.NoError(t, err)
	assert.NotContains(t, recoverable, record)
}

func TestInboxStore_ReadOnlyCommandAtFIFOHeadRunsWithoutReleasingAsyncInput(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err := enqueueInput(ctx, store, record, InputSourceUser, "\t/help\n")
	require.NoError(t, err)
	_, err = enqueueFact(ctx, store, record, InputSourceProcess, "process", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record, SessionStatusStopped))

	recoverable, err := store.ListSessionsWithRecoverableInput(ctx)

	require.NoError(t, err)
	assert.Contains(t, recoverable, record)
}

func createPendingRecoveryFixtures(ctx context.Context, t *testing.T, store *Store, projectID int64) []int64 {
	t.Helper()

	var ids []int64
	for _, content := range []string{"first pending", "second pending"} {
		rec := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
		_, err := enqueueInput(ctx, store, rec, InputSourceUser, content)
		require.NoError(t, err)
		ids = append(ids, rec)
	}

	stopped := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err := enqueueInput(ctx, store, stopped, InputSourceUser, "parked pending")
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, stopped, SessionStatusStopped))
	ids = append(ids, stopped)

	killedID := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)

	killed, err := store.GetSession(ctx, killedID)
	require.NoError(t, err)
	_, err = enqueueInput(ctx, store, killed.ID, InputSourceUser, "killed pending")
	require.NoError(t, err)
	require.NoError(t, store.WithTx(ctx, func(tx *sql.Tx) error { return MarkSessionKilledTx(ctx, tx, killed.ID) }))

	return ids
}

func createAcceptedRecoveryFixtures(
	ctx context.Context,
	t *testing.T,
	store *Store,
	projectID int64,
) []int64 {
	t.Helper()

	var ids []int64
	for i, suffix := range []string{"user last", "tool progress", "final assistant persisted"} {
		rec := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
		input, err := enqueueInput(ctx, store, rec, InputSourceUser, suffix)
		require.NoError(t, err)
		promoted, err := acceptInput(ctx, store, input.ID, suffix)
		require.NoError(t, err)

		switch i {
		case 1:
			_, err = appendMessage(ctx, store, rec, &transcript.Message{Role: "tool", Content: "durable result"})
			require.NoError(t, err)
		case 2:
			_, err = appendMessage(ctx, store, rec, &transcript.Message{Role: "assistant", Content: "durable final"})
			require.NoError(t, err)
		}

		if i == 1 {
			require.NoError(t, compactHead(ctx, store, []int64{promoted.ID}))
		}

		accepted, err := store.HasAcceptedInput(ctx, rec)
		require.NoError(t, err)
		assert.True(t, accepted)
		ids = append(ids, rec)
	}

	return ids
}

func createExcludedRecoveryFixtures(ctx context.Context, t *testing.T, store *Store, projectID int64) {
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
		rec := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
		input, err := enqueueInput(ctx, store, rec, InputSourceUser, "excluded")
		require.NoError(t, err)
		_, err = acceptInput(ctx, store, input.ID, "excluded")
		require.NoError(t, err)
		if fixture.kill {
			require.NoError(
				t,
				store.WithTx(ctx, func(tx *sql.Tx) error { return MarkSessionKilledTx(ctx, tx, rec) }),
			)
		} else {
			require.NoError(t, store.UpdateSessionStatus(ctx, rec, fixture.status))
		}
	}

	headerOnly := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	_, err := appendMessage(ctx, store, headerOnly, &transcript.Message{Role: "user", Content: "AGENTS.md header"})
	require.NoError(t, err)
	accepted, err := store.HasAcceptedInput(ctx, headerOnly)
	require.NoError(t, err)
	assert.False(t, accepted, "a plain user-role header is not accepted controller input")

	handled := sessionstoretest.BareRoot(t, store.db, projectID, "model", "", nil)
	input, err := enqueueInput(ctx, store, handled, InputSourceUser, "/status")
	require.NoError(t, err)
	require.NoError(t, resolveHandled(ctx, store, input.ID, "status command"))
}
