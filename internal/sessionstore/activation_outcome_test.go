package sessionstore

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func TestActivationOutcomeRecoversCommittedEvidence(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := t.Context()
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	candidate, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID:  record.ID,
		RootID:     record.ID,
		Iteration:  1,
		Message:    &transcript.Message{Role: "assistant", Content: "considered answer", FinishType: "stop"},
		Kind:       ResponseDispositionCandidate,
		Nudge:      &transcript.Message{Role: "user", Content: "check"},
		ObservedAt: time.Now(),
	})
	require.NoError(t, err)
	_, err = store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: record.ID, RootID: record.ID, Iteration: 2,
		Message: &transcript.Message{Role: "assistant", Content: "ack", FinishType: "stop"},
		Kind:    ResponseDispositionConfirmed, ExpectedCandidateID: candidate.MessageID, ObservedAt: time.Now(),
	})
	require.NoError(t, err)
	recovered, err := NewStore(db).LoadActivationOutcome(ctx, record.ID, false)
	require.NoError(t, err)
	assert.Equal(t, ActivationCompleted, recovered.Kind)
	assert.Equal(t, "considered answer", recovered.Text)

	_, err = db.ExecContext(ctx, `UPDATE sessions SET empty_stop_streak = 6, status = 'error' WHERE id = ?`, record.ID)
	require.NoError(t, err)
	recovered, err = NewStore(db).LoadActivationOutcome(ctx, record.ID, true)
	require.NoError(t, err)
	assert.Equal(t, ActivationCompleted, recovered.Kind)
	assert.Equal(t, SessionStatusCompleted, recovered.Status)
	assert.Equal(t, EmptyStopTerminalNotice(6), recovered.Text)
}

func TestActivationOutcomeLoadFailureIsNotIncomplete(t *testing.T) {
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(t.Context(), projectID, "model", "", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionIteration(t.Context(), record.ID, 3, SessionStatusActive))
	_, err = db.ExecContext(t.Context(), `ALTER TABLE messages RENAME TO unavailable_messages`)
	require.NoError(t, err)
	recovered, err := store.LoadActivationOutcome(t.Context(), record.ID, false)
	require.NoError(t, err)
	assert.Equal(t, ActivationFailed, recovered.Kind)
	assert.Equal(t, "could not load final messages after 3 iterations", recovered.Text)
	assert.Error(t, recovered.Diagnostic)
}
