package sessionstore

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/transcript"
)

func seededOwnedSession(ctx context.Context, t *testing.T, store Store, projectID int64) *SessionRecord {
	t.Helper()

	session, err := store.CreateSession(ctx, projectID, "m", "", map[string]any{"manager_id": "mgr"})
	require.NoError(t, err)

	return session
}

func TestBeginLifecycleStopStartsReplaceableNonReleasingRow(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)

	commit, err := store.BeginLifecycleInput(ctx, input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	var kind, content, sourceKey string
	var releases bool
	require.NoError(t, db.QueryRow(`SELECT type, content, COALESCE(source_key, ''), releases_input
		FROM session_outbox WHERE id = ?`, commit.OutputID).Scan(&kind, &content, &sourceKey, &releases))
	assert.Equal(t, "message_replaceable", kind)
	assert.Equal(t, "⏳ Stopping…", content)
	assert.Equal(t, "input:"+strconv.FormatInt(input.ID, 10)+":stop:started", sourceKey)
	assert.False(t, releases)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&status))
	assert.Equal(t, "stopping", status)
}

func TestBeginLifecycleKillKeepsPersistentReleasingRow(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/kill")
	require.NoError(t, err)

	commit, err := store.BeginLifecycleInput(ctx, input.ID, "kill", "Stopping session...")
	require.NoError(t, err)

	var kind string
	var releases bool
	require.NoError(t, db.QueryRow(`SELECT type, releases_input FROM session_outbox
		WHERE id = ?`, commit.OutputID).Scan(&kind, &releases))
	assert.Equal(t, "message_persistent", kind)
	assert.True(t, releases)
}

func TestCompleteExplicitStopAtomic(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	_, err = store.BeginLifecycleInput(ctx, input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)

	_, err = db.Exec(`INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, datetime('now'), 0, 1)`, session.ID)
	require.NoError(t, err)

	commit, err := store.CompleteExplicitStop(ctx, session.ID, input.ID)
	require.NoError(t, err)
	require.NotZero(t, commit.OutputID)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&status))
	assert.Equal(t, "stopped", status)

	var budgetState, releasedReason string
	require.NoError(t, db.QueryRow(`SELECT state, released_reason FROM session_budgets
		WHERE root_session_id = ?`, session.ID).Scan(&budgetState, &releasedReason))
	assert.Equal(t, "released", budgetState)
	assert.Equal(t, "stopped", releasedReason)

	var kind, content, sourceKey string
	var releases bool
	require.NoError(t, db.QueryRow(`SELECT type, content, COALESCE(source_key, ''), releases_input
		FROM session_outbox WHERE id = ?`, commit.OutputID).Scan(&kind, &content, &sourceKey, &releases))
	assert.Equal(t, "message_persistent", kind)
	assert.Equal(t, "⏸️ Session stopped", content)
	assert.Equal(t, "input:"+strconv.FormatInt(input.ID, 10)+":stop:completed", sourceKey)
	assert.True(t, releases)

	// Idempotent replay returns the original row, not a duplicate.
	replay, err := store.CompleteExplicitStop(ctx, session.ID, input.ID)
	require.NoError(t, err)
	assert.Equal(t, commit.OutputID, replay.OutputID)

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM session_outbox
		WHERE source_key = ?`, "input:"+strconv.FormatInt(input.ID, 10)+":stop:completed").Scan(&count))
	assert.Equal(t, 1, count)
}

func TestCompleteExplicitStopRejectsActiveRoot(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)

	_, err := store.CompleteExplicitStop(ctx, session.ID, 1)
	require.ErrorIs(t, err, ErrStopNotStopping)

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM sessions WHERE id = ?`, session.ID).Scan(&status))
	assert.Equal(t, "active", status)

	var outputs int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`,
		session.ID).Scan(&outputs))
	assert.Equal(t, 0, outputs, "failure publishes no terminal output")
}

func TestSelectInterruptedExplicitStops(t *testing.T) {
	store, db, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)

	// A historical completed stop: started and completed rows both exist.
	old, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	_, err = store.BeginLifecycleInput(ctx, old.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE sessions SET status = 'stopped' WHERE id = ?`, session.ID)
	require.NoError(t, err)
	_, err = store.CompleteExplicitStop(ctx, session.ID, old.ID)
	require.NoError(t, err)

	// A legacy interrupted stop: only the old :result row exists.
	legacy, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	require.NoError(t, store.HandleInput(ctx, legacy.ID, "stop"))
	_, err = db.Exec(`UPDATE sessions SET status = 'stopping' WHERE id = ?`, session.ID)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, 'message_persistent', '⏹ Stopping...', '{"manager_id":"mgr"}', ?, 'fp-legacy', datetime('now'), 1)`,
		session.ID, "input:"+strconv.FormatInt(legacy.ID, 10)+":stop:result")
	require.NoError(t, err)

	stops, err := store.SelectInterruptedExplicitStops(ctx)
	require.NoError(t, err)
	require.Len(t, stops, 1, "the completed stop is excluded, the interrupted one qualifies")
	assert.Equal(t, session.ID, stops[0].SessionID)
	assert.Equal(t, legacy.ID, stops[0].InputID)
}

func TestOutputReadinessOnlyNewestReleasingRow(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)
	require.NoError(t, store.BindManager(ctx, "mgr", "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	}))

	first, err := store.EnqueueOutput(ctx, OutputDraft{
		SessionID: session.ID, Type: OutputMessagePersistent, Content: "old final", ReleasesInput: true,
	})
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	startCommit, err := store.BeginLifecycleInput(ctx, input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	stopCommit, err := store.CompleteExplicitStop(ctx, session.ID, input.ID)
	require.NoError(t, err)

	// Deliver the old final, the stop start, then reach the completion.
	claim, err := store.ClaimOutputHead(ctx, "mgr")
	require.NoError(t, err)
	require.Equal(t, first.OutputID, claim.Output.ID)
	require.NoError(t, store.AckOutput(ctx, "mgr", claim.Output.ID, claim.Output.AttemptID, []string{"1"}, nil))

	claim, err = store.ClaimOutputHead(ctx, "mgr")
	require.NoError(t, err)
	require.Equal(t, startCommit.OutputID, claim.Output.ID)
	require.NoError(t, store.AckOutput(ctx, "mgr", claim.Output.ID, claim.Output.AttemptID, []string{"3"}, nil))

	// Acknowledging the older final while the stop completion is queued stays silent.
	readiness, err := store.OutputReadiness(ctx, first.OutputID)
	require.NoError(t, err)
	assert.False(t, readiness.Ready, "an older releasing row must not publish readiness")

	claim, err = store.ClaimOutputHead(ctx, "mgr")
	require.NoError(t, err)
	require.Equal(t, stopCommit.OutputID, claim.Output.ID)
	require.NoError(t, store.AckOutput(ctx, "mgr", claim.Output.ID, claim.Output.AttemptID, []string{"2"}, nil))

	readiness, err = store.OutputReadiness(ctx, stopCommit.OutputID)
	require.NoError(t, err)
	assert.True(t, readiness.Ready)
	assert.Equal(t, "stopped", readiness.Reason)
}

func TestDirectOutputFailsClosedBehindStopFence(t *testing.T) {
	store, _, projectID := newTestStore(t)
	ctx := context.Background()

	session := seededOwnedSession(ctx, t, store, projectID)
	input, err := store.EnqueueInput(ctx, session.ID, InputSourceUser, "/stop")
	require.NoError(t, err)
	_, err = store.BeginLifecycleInput(ctx, input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)

	_, _, err = store.InsertToolResultWithDirectOutput(ctx, session.ID,
		&transcript.Message{Role: "tool", Content: "late", ToolCallID: "c9", ToolName: "bash"},
		[]string{"late direct output"})
	require.Error(t, err, "direct output must fail closed behind the stop fence")
}
