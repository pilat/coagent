package sessionstore

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/subagent"
)

func TestShieldCommands_ToggleTreeAndReplay(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	child, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "general", "model", "")
	require.NoError(t, err)
	nested, err := store.CreateSubagentSession(ctx, projectID, child, root.ID, "general", "model", "")
	require.NoError(t, err)

	raiseInput, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "  /shieldsup\n")
	require.NoError(t, err)
	raise, started, err := store.BeginShieldRaise(ctx, raiseInput.ID, false, true)
	require.NoError(t, err)
	assert.True(t, raise.Changed)
	assert.False(t, raise.NeedsStop)
	assert.NotZero(t, started.OutputID)
	assertTreeShields(t, db, true, root.ID, child, nested)

	interrupted, err := store.SelectInterruptedShieldRaises(ctx)
	require.NoError(t, err)
	require.Equal(t, []InterruptedShieldRaise{{SessionID: root.ID, InputID: raiseInput.ID}}, interrupted)

	completed, err := store.CompleteShieldRaise(ctx, root.ID, raiseInput.ID)
	require.NoError(t, err)
	replayed, err := store.CompleteShieldRaise(ctx, root.ID, raiseInput.ID)
	require.NoError(t, err)
	assert.Equal(t, completed.OutputID, replayed.OutputID)
	assert.Empty(t, mustInterruptedRaises(t, store))

	idempotentInput, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/shieldsup")
	require.NoError(t, err)
	idempotent, output, err := store.BeginShieldRaise(ctx, idempotentInput.ID, true, true)
	require.NoError(t, err)
	assert.False(t, idempotent.Changed)
	assert.NotZero(t, output.OutputID)
	assertTreeShields(t, db, true, root.ID, child, nested)

	busyInput, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/shieldsdown")
	require.NoError(t, err)
	_, err = store.ResolveShieldDown(ctx, busyInput.ID, true)
	require.NoError(t, err)
	assertTreeShields(t, db, true, root.ID, child, nested)
	assert.Equal(t, ShieldLowerBusyContent, shieldOutputContent(t, db, busyInput.ID))

	downInput, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/shieldsdown")
	require.NoError(t, err)
	_, err = store.ResolveShieldDown(ctx, downInput.ID, false)
	require.NoError(t, err)
	assertTreeShields(t, db, false, root.ID, child, nested)
	assert.Equal(t, ShieldLoweredContent, shieldOutputContent(t, db, downInput.ID))
	assert.Zero(t, transcriptCount(t, db, root.ID))
}

func TestShieldRaise_ActiveTreeParksRootAtCompletion(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/shieldsup")
	require.NoError(t, err)

	raise, _, err := store.BeginShieldRaise(ctx, input.ID, true, true)
	require.NoError(t, err)
	assert.True(t, raise.NeedsStop)
	stopping, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusStopping, stopping.Status)

	_, err = store.CompleteShieldRaise(ctx, root.ID, input.ID)
	require.NoError(t, err)
	stopped, err := store.GetSession(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusStopped, stopped.Status)
	assert.True(t, stopped.ShieldsUp)
}

func TestShieldRaise_RequiresSandboxAndManagerOwnedRootInput(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)

	input, err := store.EnqueueInput(ctx, root.ID, InputSourceUser, "/shieldsup")
	require.NoError(t, err)
	raise, _, err := store.BeginShieldRaise(ctx, input.ID, false, false)
	require.NoError(t, err)
	assert.False(t, raise.Changed)
	assertTreeShields(t, db, false, root.ID)
	assert.Equal(t, ShieldSandboxDisabledContent, shieldOutputContent(t, db, input.ID))

	agentInput, err := store.EnqueueInput(ctx, root.ID, InputSourceAgent, "/shieldsup")
	require.NoError(t, err)
	_, _, err = store.BeginShieldRaise(ctx, agentInput.ID, false, true)
	require.ErrorIs(t, err, ErrInvalidShieldCommand)
	assertTreeShields(t, db, false, root.ID)
}

func TestSessionShields_InheritOnChildAndClearReplacement(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	root, _, err := store.CreateManagerRoot(ctx, ManagerRootCreate{
		ProjectID: projectID, Model: "model", Attributes: map[string]any{"manager_id": "cli"},
		Name: "project", WorkDir: t.TempDir(),
	})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `UPDATE sessions SET shields_up = TRUE WHERE id = ?`, root.ID)
	require.NoError(t, err)

	child, err := newTestSubagentTransactions(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: root.ID, RootID: root.ID, AgentType: "general",
		Model: "model", State: subagent.StateSpawned, TaskCallID: "call-1", InitialInput: "work",
	})
	require.NoError(t, err)
	childRecord, err := store.GetSession(ctx, child)
	require.NoError(t, err)
	assert.True(t, childRecord.ShieldsUp)
	nested, err := newTestSubagentTransactions(db).Create(ctx, subagent.Create{
		ProjectID: projectID, ParentID: child, RootID: root.ID, AgentType: "general",
		Model: "model", State: subagent.StateSpawned, TaskCallID: "call-2", InitialInput: "nested work",
	})
	require.NoError(t, err)
	nestedRecord, err := store.GetSession(ctx, nested)
	require.NoError(t, err)
	assert.True(t, nestedRecord.ShieldsUp)

	replacement, _, err := store.ReplaceManagerRoot(ctx, root.ID, "project", t.TempDir())
	require.NoError(t, err)
	assert.True(t, replacement.ShieldsUp)

	independent, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	assert.False(t, independent.ShieldsUp)
}

func assertTreeShields(t *testing.T, db *sql.DB, want bool, ids ...int64) {
	t.Helper()
	for _, id := range ids {
		var got bool
		require.NoError(t, db.QueryRowContext(context.Background(),
			`SELECT shields_up FROM sessions WHERE id = ?`, id).Scan(&got))
		assert.Equal(t, want, got, "session %d", id)
	}
}

func mustInterruptedRaises(t *testing.T, store Store) []InterruptedShieldRaise {
	t.Helper()
	raises, err := store.SelectInterruptedShieldRaises(context.Background())
	require.NoError(t, err)
	return raises
}

func shieldOutputContent(t *testing.T, db *sql.DB, inputID int64) string {
	t.Helper()
	var content string
	require.NoError(t, db.QueryRowContext(context.Background(), `SELECT content FROM session_outbox
		WHERE source_key LIKE 'input:' || ? || ':%' ORDER BY id DESC LIMIT 1`, inputID).Scan(&content))
	return content
}

func transcriptCount(t *testing.T, db *sql.DB, sessionID int64) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count))
	return count
}
