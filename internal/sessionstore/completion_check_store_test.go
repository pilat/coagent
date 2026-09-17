package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func seedCompletionSession(t *testing.T, store Store, db *sql.DB, projectID int64) int64 {
	t.Helper()

	rec, err := store.CreateSession(context.Background(), projectID, "model", "",
		map[string]any{"manager_id": "test-manager"})
	require.NoError(t, err)

	return rec.ID
}

func assistantStopMessage(content string) *transcript.Message {
	return &transcript.Message{Role: "assistant", Content: content, FinishType: "stop"}
}

func TestCompletionCheck_CandidateThenConfirmCommitsOneNudgeAndOneOutput(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	candidate, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: assistantStopMessage("first answer"),
		Kind:    ResponseDispositionCandidate,
		Nudge:   &transcript.Message{Role: "user", Content: "second look"},
	})
	require.NoError(t, err)
	require.Positive(t, candidate.MessageID)
	require.Positive(t, candidate.NudgeMessageID)

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	assert.Equal(t, candidate.MessageID, *state.CandidateID)

	var outbox int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Zero(t, outbox, "an unconfirmed candidate creates no outbox row")

	confirmed, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 2,
		Message:             assistantStopMessage("confirmed answer"),
		Kind:                ResponseDispositionConfirmed,
		Output:              "confirmed answer",
		ExpectedCandidateID: candidate.MessageID,
	})
	require.NoError(t, err)
	require.NotNil(t, confirmed.Output)

	state, err = store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID, "confirmation clears the pending check")

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outbox))
	assert.Equal(t, 1, outbox)
}

func TestCompletionCheck_StaleCandidateIsAConflict(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	candidate, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: assistantStopMessage("first answer"),
		Kind:    ResponseDispositionCandidate,
		Nudge:   &transcript.Message{Role: "user", Content: "second look"},
	})
	require.NoError(t, err)

	_, err = store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 2,
		Message:             assistantStopMessage("unrelated stop"),
		Kind:                ResponseDispositionConfirmed,
		Output:              "unrelated stop",
		ExpectedCandidateID: candidate.MessageID + 1000,
	})
	require.ErrorIs(t, err, ErrCompletionCheckConflict)
}

func TestCompletionCheck_ToolResponseClearsPendingCheck(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, nil, projectID)

	candidate, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: assistantStopMessage("first answer"),
		Kind:    ResponseDispositionCandidate,
		Nudge:   &transcript.Message{Role: "user", Content: "second look"},
	})
	require.NoError(t, err)

	_, err = store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 2,
		Message: &transcript.Message{
			Role: "assistant", Content: "calling", FinishType: "tool_calls",
			ToolCalls: []byte(`[{"id":"c1","name":"read"}]`),
		},
		Kind:                ResponseDispositionToolCall,
		ExpectedCandidateID: candidate.MessageID,
	})
	require.NoError(t, err)

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID)
}

func TestCompletionCheck_EmptyStreakPersistsAcrossCalls(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, nil, projectID)

	for i := 1; i <= 3; i++ {
		result, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
			SessionID: sessionID, RootID: sessionID, Iteration: i,
			Message:         &transcript.Message{Role: "assistant", Content: "  ", FinishType: "stop"},
			Kind:            ResponseDispositionEmptyStop,
			Nudge:           &transcript.Message{Role: "user", Content: "continue"},
			EmptyStopStreak: i,
		})
		require.NoError(t, err)
		assert.Equal(t, i, result.EmptyStopStreak)
	}

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, 3, state.EmptyStopStreak)
}

func TestCompletionCheck_ProjectionErrorCommitsTerminalOnce(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	result, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: assistantStopMessage("paid attempt"),
		Kind:    ResponseDispositionProjectionError,
		Output:  "wake projection failed",
	})
	require.NoError(t, err)
	assert.True(t, result.TerminalCommitted)

	var status string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status))
	assert.Equal(t, "error", status)
}
