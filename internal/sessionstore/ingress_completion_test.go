package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

// seedPendingCheck installs a pending completion candidate and a non-zero
// empty streak, the state every external ingress must clear.
func seedPendingCheck(t *testing.T, db *sql.DB, sessionID int64) {
	t.Helper()

	_, err := db.ExecContext(context.Background(), `
		INSERT INTO messages (id, session_id, role, content, finish_type)
		VALUES (9000, ?, 'assistant', 'stale candidate', 'stop')`, sessionID)
	require.NoError(t, err)

	_, err = db.ExecContext(context.Background(), `UPDATE sessions
		SET completion_check_candidate_id = 9000, empty_stop_streak = 2
		WHERE id = ?`, sessionID)
	require.NoError(t, err)
}

func readCompletionState(t *testing.T, db *sql.DB, sessionID int64) (sql.NullInt64, bool, int) {
	t.Helper()

	var candidate sql.NullInt64
	var replyPending bool
	var streak int

	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT
		completion_check_candidate_id, manager_reply_pending, empty_stop_streak
		FROM sessions WHERE id = ?`, sessionID).Scan(&candidate, &replyPending, &streak))

	return candidate, replyPending, streak
}

func TestInputPromotion_ClearsCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	input, err := store.EnqueueInput(ctx, sessionID, InputSourceUser, "fresh user input")
	require.NoError(t, err)

	_, err = store.PromoteInput(ctx, input.ID, "fresh user input")
	require.NoError(t, err)

	candidate, replyPending, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid, "promotion clears the stale candidate")
	assert.Zero(t, streak, "promotion resets the empty streak")
	assert.True(t, replyPending, "a manager-owned promotion opens the reply obligation")
}

func TestInputPromotion_ReadOnlyReceiptOpensNoReplyObligation(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	for _, content := range []string{"/status", "/help", "/schedules", "/compact", "/compact focus"} {
		input, err := store.EnqueueInput(ctx, sessionID, InputSourceUser, content)
		require.NoError(t, err)

		_, err = store.PromoteInput(ctx, input.ID, content)
		require.NoError(t, err)

		_, replyPending, streak := readCompletionState(t, db, sessionID)
		assert.False(t, replyPending, "read-only %q opens no reply obligation", content)
		assert.Zero(t, streak, "read-only %q still resets the empty streak", content)

		_, err = db.ExecContext(ctx, `UPDATE sessions
			SET completion_check_candidate_id = 9000, empty_stop_streak = 2 WHERE id = ?`, sessionID)
		require.NoError(t, err)
	}
}

func TestInputAgentPromotion_ClearsCompletionWithoutReply(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	input, err := store.EnqueueInput(ctx, sessionID, InputSourceAgent, "agent follow-up")
	require.NoError(t, err)

	_, err = store.PromoteInput(ctx, input.ID, "agent follow-up")
	require.NoError(t, err)

	candidate, replyPending, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid)
	assert.Zero(t, streak)
	assert.False(t, replyPending, "a non-manager ingress never sets the reply obligation")
}

func TestDeliveryScheduledTurn_ClearsCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	_, _, inserted, err := store.InsertScheduledToolNotificationPairOnce(
		ctx, sessionID, "delivery-1", "fp-1",
		&transcript.Message{Role: "assistant", ToolCalls: []byte(`[{"id":"c1","name":"read","arguments":{}}]`)},
		&transcript.Message{Role: "tool", Content: "scheduled result", ToolCallID: "c1", ToolName: "read"},
	)
	require.NoError(t, err)
	require.True(t, inserted)

	candidate, _, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid)
	assert.Zero(t, streak)
}

func TestDeliveryDirectToolResult_ClearsCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	_, _, err := store.InsertToolResultWithDirectOutput(ctx, sessionID, &transcript.Message{
		Role: "tool", Content: "external result", ToolCallID: "sleep-1", ToolName: "sleep",
	}, nil)
	require.NoError(t, err)

	candidate, _, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid)
	assert.Zero(t, streak)
}

func TestDeliveryReplay_KeepsNewerCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	result := &transcript.Message{
		Role: "tool", Content: "external result", ToolCallID: "sleep-1", ToolName: "sleep",
	}
	_, _, err := store.InsertToolResultWithDirectOutput(ctx, sessionID, result, nil)
	require.NoError(t, err)

	// Replays insert no new model input and must not clear a newer check.
	seedPendingCheck(t, db, sessionID)

	_, _, err = store.InsertToolResultWithDirectOutput(ctx, sessionID, result, nil)
	require.NoError(t, err)

	candidate, _, streak := readCompletionState(t, db, sessionID)
	assert.True(t, candidate.Valid, "an idempotent replay must not clear a newer check")
	assert.Equal(t, 2, streak)
}

func TestResetContext_ClearsCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	seedPendingCheck(t, db, sessionID)

	_, inserted, err := store.ResetSessionContextOnce(ctx, sessionID, "reset-1", "fp-1",
		[]*transcript.Message{{Role: "user", Content: "fresh start"}})
	require.NoError(t, err)
	require.True(t, inserted)

	candidate, _, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid)
	assert.Zero(t, streak)
}

// A blocking child completion is one cross-table transaction: the delivered
// parent input and the cleared stale check commit together, or not at all.
func TestDeliverCompletion_ClearsCompletionState(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	childID, err := store.CreateSubagentSession(ctx, projectID, sessionID, sessionID, "general", "m", "")
	require.NoError(t, err)
	seedLink(t, db, sessionID, childID, "task-1")
	seedPendingCheck(t, db, sessionID)

	_, won, err := newTestSubagentTransactions(db).DeliverCompletion(ctx, sessionID, []*transcript.Message{
		{Role: "assistant", ToolCalls: []byte(`[{"ID":"ev-1","Name":"subagent_event"}]`)},
		{Role: "tool", Content: "child done", ToolCallID: "ev-1", ToolName: "subagent_event"},
	}, childID, 1)
	require.NoError(t, err)
	require.True(t, won)

	candidate, _, streak := readCompletionState(t, db, sessionID)
	assert.False(t, candidate.Valid)
	assert.Zero(t, streak)
}
