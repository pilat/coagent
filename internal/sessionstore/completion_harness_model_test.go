package sessionstore

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

// The durable completion check survives process restart: a crash between the
// hidden candidate and its confirmation must resume the same pending check,
// not duplicate the nudge or accept an unrelated stop.
func TestHarnessModel_CompletionCheckRestartAndDelivery(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	candidate, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: assistantStopMessage("first answer"),
		Kind:    ResponseDispositionCandidate,
		Nudge:   &transcript.Message{Role: "user", Content: "second look"},
		// A manager-owned turn opens the reply obligation alongside the check.
		ManagerReplyPending: true,
		// A prior empty attempt persists through the candidate commit.
		EmptyStopStreak: 2,
	})
	require.NoError(t, err)
	require.Positive(t, candidate.MessageID)
	require.Positive(t, candidate.NudgeMessageID)

	// Restart: a fresh store over the same database carries no memory.
	restarted := NewStore(db)

	state, err := restarted.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, candidate.MessageID, *state.CandidateID,
		"restart must preserve the pending candidate identity")
	assert.True(t, state.ManagerReplyPending,
		"restart must preserve the manager reply obligation")
	assert.Equal(t, 2, state.EmptyStopStreak,
		"restart must preserve the durable empty streak")

	confirmed, err := restarted.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 2,
		Message:             assistantStopMessage("confirmed answer"),
		Kind:                ResponseDispositionConfirmed,
		Output:              "confirmed answer",
		ExpectedCandidateID: candidate.MessageID,
		ManagerReplyPending: true,
		EmptyStopStreak:     0,
	})
	require.NoError(t, err)
	require.NotNil(t, confirmed.Output)

	var outbox int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Equal(t, 1, outbox, "confirmation publishes exactly one releasing output")

	var releases int
	var content string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT releases_input, content FROM session_outbox WHERE session_id = ?`, sessionID).
		Scan(&releases, &content))
	assert.Equal(t, 1, releases, "the confirmed output releases the manager input")
	assert.NotContains(t, content, "first answer",
		"the hidden candidate text must never reach the manager")

	state, err = restarted.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "confirmation clears the pending check")
	assert.False(t, state.ManagerReplyPending, "a releasing output clears the reply obligation")

	// Replaying the confirmed commit with the same candidate id is a stale
	// transition, not a second publication.
	_, err = restarted.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 3,
		Message:             assistantStopMessage("replayed confirmation"),
		Kind:                ResponseDispositionConfirmed,
		Output:              "replayed confirmation",
		ExpectedCandidateID: candidate.MessageID,
	})
	require.ErrorIs(t, err, ErrCompletionCheckConflict)

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Equal(t, 1, outbox, "a stale replay must not publish a second output")
}

// The empty-stop ladder is durable per attempt: restarts between attempts
// continue the same count, and the terminal notice commits exactly once.
func TestHarnessModel_CompletionEmptyStopEscalation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	for attempt := 1; attempt <= 5; attempt++ {
		result, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
			SessionID: sessionID, RootID: sessionID, Iteration: attempt,
			Message:         &transcript.Message{Role: "assistant", Content: "  ", FinishType: "stop"},
			Kind:            ResponseDispositionEmptyStop,
			Nudge:           &transcript.Message{Role: "user", Content: "continue"},
			EmptyStopStreak: attempt,
		})
		require.NoError(t, err)
		assert.Equal(t, attempt, result.EmptyStopStreak)
		assert.Nil(t, result.Output, "attempt %d stays hidden", attempt)

		// Restart at each boundary: the streak must continue, never reset.
		store = NewStore(db)
	}

	var outbox int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Zero(t, outbox, "warn-level empty stops publish nothing")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 5, state.EmptyStopStreak)

	terminal, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 6,
		Message:         &transcript.Message{Role: "assistant", Content: "", FinishType: "stop"},
		Kind:            ResponseDispositionEmptyStop,
		Output:          EmptyStopTerminalNotice(EmptyStopTerminalStreak),
		EmptyStopStreak: EmptyStopTerminalStreak,
	})
	require.NoError(t, err)
	require.NotNil(t, terminal.Output)

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Equal(t, 1, outbox, "the sixth empty stop commits one terminal notice")

	var sourceKey string
	var fingerprint string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COALESCE(source_key, ''), COALESCE(fingerprint, '') FROM session_outbox WHERE session_id = ?`,
		sessionID).Scan(&sourceKey, &fingerprint))
	assert.Equal(t, fmt.Sprintf("message:%d:final", terminal.MessageID), sourceKey,
		"the terminal notice is keyed to its attempt for idempotent replay")

	// The no-duplicate guarantee lives in the outbox identity: redelivering
	// the same source key with the same fingerprint is a no-op returning the
	// existing row, never a second notice.
	replay, err := store.EnqueueOutput(ctx, OutputDraft{
		SessionID: sessionID, Type: OutputMessagePersistent,
		Content:   EmptyStopTerminalNotice(EmptyStopTerminalStreak),
		SourceKey: sourceKey, Fingerprint: fingerprint, ReleasesInput: true,
	})
	require.NoError(t, err)
	assert.True(t, replay.Existing, "same-key same-fingerprint redelivery must reuse the row")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Equal(t, 1, outbox, "the terminal notice commits exactly once")

	// Reusing the key with different content fails closed instead of
	// replacing the committed notice.
	_, err = store.EnqueueOutput(ctx, OutputDraft{
		SessionID: sessionID, Type: OutputMessagePersistent,
		Content:   "different notice",
		SourceKey: sourceKey, Fingerprint: "different-fingerprint", ReleasesInput: true,
	})
	require.ErrorIs(t, err, ErrOutputConflict)
}

// The wake-source projection reads the producer ledger first: a completed but
// undelivered non-blocking child owns the next turn, while a delivered link or
// a stopped/killed one owns nothing.
func TestHarnessModel_CompletionWakeRaceClosed(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	parent, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := store.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		VALUES (?, ?, ?, 0, 0, 'completed', ?)`,
		parent.ID, childID, "task-1", time.Now().UTC().Unix())
	require.NoError(t, err)

	has, err := store.HasBackgroundWakeSource(ctx, parent.ID)
	require.NoError(t, err)
	assert.True(t, has,
		"a completed undelivered child promises automatic delivery: ledger first, inbox later")

	_, err = db.ExecContext(ctx, `UPDATE subagent_links
		SET delivered_at = ? WHERE child_id = ?`, time.Now().UTC().Unix(), childID)
	require.NoError(t, err)

	has, err = store.HasBackgroundWakeSource(ctx, parent.ID)
	require.NoError(t, err)
	assert.False(t, has, "a delivered link no longer owns the next turn")

	for _, state := range []string{"stopped", "killed"} {
		other, err := store.CreateSession(ctx, projectID, "m", "", nil)
		require.NoError(t, err)
		otherChild, err := store.CreateSubagentSession(ctx, projectID, other.ID, other.ID, "general", "m", "")
		require.NoError(t, err)

		_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
			(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
			VALUES (?, ?, ?, 0, 0, ?, ?)`,
			other.ID, otherChild, "task-1", state, time.Now().UTC().Unix())
		require.NoError(t, err)

		has, err := store.HasBackgroundWakeSource(ctx, other.ID)
		require.NoError(t, err)
		assert.False(t, has, "state %s promises no wake and must not bypass the check", state)
	}
}

// The background delivery leg keeps the wake until the inbox is promoted: the
// undelivered completed link, then the pending inbox row, owns the next turn,
// and promotion clears the stale check in the same commit.
func TestHarnessModel_CompletionDeliveryThenPromotionInvalidates(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	parent, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	childID, err := store.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
		VALUES (?, ?, ?, 0, 0, 'completed', ?)`,
		parent.ID, childID, "task-1", time.Now().UTC().Unix())
	require.NoError(t, err)

	seedPendingCheck(t, db, parent.ID)

	link, err := subagent.NewStore(db).GetLink(ctx, childID)
	require.NoError(t, err)

	won, err := newTestSubagentTransactions(db).DeliverBackgroundCompletion(ctx, *link, 1)
	require.NoError(t, err)
	require.True(t, won)

	has, err := store.HasBackgroundWakeSource(ctx, parent.ID)
	require.NoError(t, err)
	assert.True(t, has, "the pending inbox row owns the next turn after ledger delivery")

	input, err := store.PeekPending(ctx, parent.ID)
	require.NoError(t, err)

	_, err = store.PromoteInput(ctx, input.ID, input.RawContent)
	require.NoError(t, err)

	candidate, _, streak := readCompletionState(t, db, parent.ID)
	assert.False(t, candidate.Valid, "promotion clears the stale candidate")
	assert.Zero(t, streak, "promotion resets the empty streak")
}
