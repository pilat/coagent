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

// A budget-fired confirming stop suppresses the outbox row but the pointer was
// written in the same transaction before suppression: a child recovers the
// full answer through it even though the root's own final was dropped.
func TestConfirmedAnswerPointer_SurvivesBudgetSuppression(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)
	observed := time.Now().UTC()

	// The candidate commits before any budget exists.
	_, err = store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: session.ID, RootID: session.ID, Iteration: 1,
		Message:    &transcript.Message{Role: "assistant", Content: "the full answer"},
		Kind:       ResponseDispositionCandidate,
		Nudge:      &transcript.Message{Role: "user", Content: "second look"},
		ObservedAt: observed,
	})
	require.NoError(t, err)

	state, err := store.LoadCompletionCheckState(ctx, session.ID)
	require.NoError(t, err)
	require.NotNil(t, state.CandidateID)
	candidateID := *state.CandidateID

	// The confirming attempt's cost crosses an armed limit inside the
	// disposition transaction.
	_, err = db.ExecContext(ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, datetime('now'), 0, 0.000001)`, session.ID)
	require.NoError(t, err)

	result, err := store.CommitAcceptedResponseDisposition(ctx, AcceptedResponseDisposition{
		SessionID: session.ID, RootID: session.ID, Iteration: 2,
		Message: &transcript.Message{
			Role: "assistant", Content: "why I am stopping", CostUSD: 0.01,
		},
		Kind:                ResponseDispositionConfirmed,
		ExpectedCandidateID: candidateID,
		ObservedAt:          observed,
	})
	require.NoError(t, err)
	assert.True(t, result.BudgetFired, "the crossing fires on the confirming attempt")

	// The final was suppressed (only the checkpoint is in the outbox), but the
	// pointer is set and resolvable.
	var count int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM session_outbox WHERE session_id = ? AND source_key LIKE 'message:%:final'`,
		session.ID).Scan(&count))
	assert.Zero(t, count, "the model final was suppressed by the fired budget")

	record, err := store.GetSession(ctx, session.ID)
	require.NoError(t, err)
	require.NotNil(t, record.CompletionCheckConfirmedAnswerID)
	assert.Equal(t, candidateID, *record.CompletionCheckConfirmedAnswerID)

	content, err := store.LoadMessageContentByID(ctx, session.ID, *record.CompletionCheckConfirmedAnswerID)
	require.NoError(t, err)
	assert.Equal(t, "the full answer", content, "the child recovers the candidate through the pointer")
}

func readSessionStatus(t *testing.T, db *sql.DB, sessionID int64) SessionStatus {
	t.Helper()

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, sessionID).Scan(&status))

	return SessionStatus(status)
}

// A suspended session's iteration persist flows through the ordinary path:
// the wake protocol owns status transitions (activatePromotedInputSession),
// so UpdateSessionIteration must not add its own status fence beyond the
// stopping/terminating/killed one.
func TestUpdateSessionIterationAllowsSuspendedPersist(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session, err := store.CreateSession(ctx, projectID, "m", "", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, session.ID, SessionStatusSuspended))

	require.NoError(t, store.UpdateSessionIteration(ctx, session.ID, 9, SessionStatusSuspended))
	assert.Equal(t, SessionStatusSuspended, readSessionStatus(t, db, session.ID))
}
