package sessionstore_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

func TestHarnessModel_StopHookBlockSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	input, err := enqueueInput(ctx, store, sessionID, sessionstore.InputSourceUser, "original request")
	require.NoError(t, err)
	_, err = acceptInput(ctx, store, input.ID, "original request")
	require.NoError(t, err)

	candidateID := commitStopHookCandidate(t, store, sessionID)
	feedback := commitStopHookBlock(t, store, sessionID, candidateID)

	var outputs int

	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, sessionID).Scan(&outputs))
	assert.Zero(t, outputs)

	restarted := testStore(db)
	state, err := restarted.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, state.NudgedThisGeneration)
	assert.Nil(t, state.CandidateID)
	assert.Empty(t, state.CandidateText)

	published, err := restarted.Commit(ctx, sessionstore.Commit{
		SessionID: sessionID,
		Messages:  []*transcript.Message{assistantStopMessage("replacement answer B")},
		Unfired: sessionstore.Parts{Outputs: []sessionstore.Output{{
			Type: sessionstore.OutputMessage, Content: "replacement answer B",
			MessageRef: 0, Phase: "final", ReleasesInput: true,
		}}},
	})
	require.NoError(t, err)
	require.Len(t, published.Outputs, 1)
	assertStopHookReplacementPublished(t, db, sessionID, feedback)
}

func commitStopHookCandidate(t *testing.T, store *sessionstore.Store, sessionID int64) int64 {
	t.Helper()

	committed, err := store.Commit(context.Background(), sessionstore.Commit{
		SessionID: sessionID,
		Messages:  []*transcript.Message{assistantStopMessage("refused answer A")},
		Unfired: sessionstore.Parts{
			Messages: []*transcript.Message{{Role: "user", Content: sessionprompt.RenderCompletionNudge(nil)}},
			State: sessionstore.StatePatch{
				Candidate: &sessionstore.CandidateChange{NextRef: 0}, MarkCompletionNudge: true,
			},
		},
	})
	require.NoError(t, err)
	require.Len(t, committed.MessageIDs, 2)
	assert.Empty(t, committed.Outputs)

	return committed.MessageIDs[0]
}

func commitStopHookBlock(t *testing.T, store *sessionstore.Store, sessionID, candidateID int64) string {
	t.Helper()

	feedback := sessionprompt.RenderStopHookFeedback("reason R: finish the outstanding work")
	committed, err := store.Commit(context.Background(), sessionstore.Commit{
		SessionID: sessionID,
		Messages:  []*transcript.Message{assistantStopMessage("confirmed")},
		State: sessionstore.StatePatch{
			Candidate: &sessionstore.CandidateChange{Expected: candidateID, NextRef: -1},
		},
		Unfired: sessionstore.Parts{
			Messages: []*transcript.Message{{Role: "user", Content: feedback}},
			State:    sessionstore.StatePatch{MarkCompletionNudge: true},
		},
	})
	require.NoError(t, err)
	assert.Empty(t, committed.Outputs)

	return feedback
}

func assertStopHookReplacementPublished(t *testing.T, db *sql.DB, sessionID int64, feedback string) {
	t.Helper()

	var outputs, hostMessages int
	var content string

	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*), COALESCE(MAX(content), '') FROM session_outbox WHERE session_id = ?`, sessionID).
		Scan(&outputs, &content))
	assert.Equal(t, 1, outputs)
	assert.Equal(t, "replacement answer B", content)
	assert.NotContains(t, content, "refused answer A")
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'user' AND content = ?`, sessionID, feedback).
		Scan(&hostMessages))
	assert.Equal(t, 1, hostMessages)
}
