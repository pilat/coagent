package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

// Confirm sets the durable answer pointer alongside clearing the check: the
// same tx that zeroes candidate_id writes the candidate's row id, so a
// finalizing child can recover the full answer even after the ack.
func TestConfirmedDisposition_SetsConfirmedAnswerPointer(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)

	runner.lastResp = textResponse("the full answer")
	require.NoError(t, runner.recordIteration(context.Background()))

	state, err := store.LoadCompletionCheckState(context.Background(), sessionID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	candidateID := *state.CandidateID

	runner.lastResp = textResponse("why I am stopping")
	require.NoError(t, runner.recordIteration(context.Background()))

	var pointer any
	require.NoError(t, db.QueryRow(
		`SELECT completion_check_confirmed_answer_id FROM sessions WHERE id = ?`, sessionID,
	).Scan(&pointer))
	require.NotNil(t, pointer, "confirm sets the pointer")
	assert.Equal(t, candidateID, pointer)

	var content string
	require.NoError(t, db.QueryRow(
		`SELECT content FROM messages WHERE id = ?`, pointer,
	).Scan(&content))
	assert.Equal(t, "the full answer", content)
}

// An external model-visible input clears a stale pointer along with the
// candidate: a fresh activation never inherits the prior one's answer.
func TestExternalInputClearsConfirmedAnswerPointer(t *testing.T) {
	_, db, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("the full answer")
	require.NoError(t, runner.recordIteration(ctx))
	runner.lastResp = textResponse("why I am stopping")
	require.NoError(t, runner.recordIteration(ctx))

	var pointer any
	require.NoError(t, db.QueryRow(
		`SELECT completion_check_confirmed_answer_id FROM sessions WHERE id = ?`, sessionID,
	).Scan(&pointer))
	require.NotNil(t, pointer)

	input, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "fresh manager input")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "fresh manager input")
	require.NoError(t, err)

	require.NoError(t, db.QueryRow(
		`SELECT completion_check_confirmed_answer_id FROM sessions WHERE id = ?`, sessionID,
	).Scan(&pointer))
	assert.Nil(t, pointer, "promotion clears the stale pointer")
}

// The session record exposes the column so a finalizing child can thread it
// into deriveOutcome; the pointer survives restarts because it is durable.
func TestConfirmedAnswerPointer_SessionRecordExposesColumn(t *testing.T) {
	_, _, store, sessionID, runner := newDispositionLoop(t)
	ctx := context.Background()

	runner.lastResp = textResponse("the full answer")
	require.NoError(t, runner.recordIteration(ctx))
	runner.lastResp = textResponse("why I am stopping")
	require.NoError(t, runner.recordIteration(ctx))

	record, err := store.GetSession(ctx, sessionID)
	require.NoError(t, err)
	require.NotNil(t, record.CompletionCheckConfirmedAnswerID)
	assert.NotZero(t, *record.CompletionCheckConfirmedAnswerID,
		"the record carries the candidate row id")
}
