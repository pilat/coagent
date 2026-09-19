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

// A budget-fired candidate commit suppresses the attempt's own answer: the
// check is dropped with it, so neither the resumed check state nor the live
// progress-card note may resurface the text.
func TestCompletionCheck_BudgetFiredCandidateDropsTheAnswer(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	input, err := store.EnqueueInput(ctx, sessionID, InputSourceUser, "go")
	require.NoError(t, err)
	_, err = store.PromoteInput(ctx, input.ID, "go")
	require.NoError(t, err)

	// The limit sits below the candidate attempt's cost, so the disposition
	// transaction itself observes the crossing and suppresses the answer.
	_, err = db.ExecContext(ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, datetime('now'), 0, 0.000001)`, sessionID)
	require.NoError(t, err)

	result, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: sessionID, RootID: sessionID, Iteration: 1,
		Message: &transcript.Message{
			Role: "assistant", Content: "the full answer", FinishType: "stop", CostUSD: 0.01,
		},
		Kind:  ResponseDispositionCandidate,
		Nudge: &transcript.Message{Role: "user", Content: "second look"},
	})
	require.NoError(t, err)
	assert.True(t, result.BudgetFired, "the crossing fires on the candidate attempt")

	state, err := store.LoadCompletionCheckState(ctx, sessionID)
	require.NoError(t, err)
	assert.Nil(t, state.CandidateID,
		"the suppressed candidate is dropped, not deferred as a pending check")

	var outbox []string
	rows, err := db.QueryContext(ctx,
		`SELECT content FROM session_outbox WHERE session_id = ? ORDER BY id`, sessionID)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var content string
		require.NoError(t, rows.Scan(&content))
		outbox = append(outbox, content)
	}
	require.NoError(t, rows.Err())
	require.Len(t, outbox, 1, "only the host budget checkpoint commits")
	assert.Contains(t, outbox[0], "Budget checkpoint reached")
	assert.NotContains(t, outbox[0], "the full answer")

	facts, err := store.CaptureProgress(ctx, sessionID)
	require.NoError(t, err)
	assert.NotContains(t, facts.LatestModelProgress, "the full answer",
		"the fired-budget card note must not carry the suppressed answer")
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
