package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_StateErrorAndOutputCommitTogether(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	states, ok := store.(StateOutputStore)
	require.True(t, ok)

	output, err := states.UpdateSessionIterationWithOutput(ctx, record.ID, 3, SessionStatusError, "❌ LLM error")
	require.NoError(t, err)
	require.NotNil(t, output)

	var status, content string
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, record.ID).Scan(&status))
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT content FROM session_outbox WHERE id = ?`, output.OutputID).Scan(&content),
	)
	assert.Equal(t, string(SessionStatusError), status)
	assert.Equal(t, "❌ LLM error", content)
}

func TestStore_StateErrorCannotOverwriteLifecycleFence(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", map[string]any{"manager_id": "cli"})
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusStopping))
	states := store.(StateOutputStore)

	_, err = states.UpdateSessionIterationWithOutput(ctx, record.ID, 1, SessionStatusError, "❌ cancelled")
	require.Error(t, err)

	var status string
	var outputs int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT status FROM sessions WHERE id = ?`, record.ID).Scan(&status))
	require.NoError(
		t,
		db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_outbox WHERE session_id = ?`, record.ID).Scan(&outputs),
	)
	assert.Equal(t, string(SessionStatusStopping), status)
	assert.Zero(t, outputs)
}

func TestStore_CheckpointCannotOverwriteLifecycleFence(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	record, err := store.CreateSession(ctx, projectID, "model", "", nil)
	require.NoError(t, err)
	require.NoError(t, store.UpdateSessionStatus(ctx, record.ID, SessionStatusTerminating))

	err = store.UpdateSessionIteration(ctx, record.ID, 1, SessionStatusActive)
	require.Error(t, err)
	updated, err := store.GetSession(ctx, record.ID)
	require.NoError(t, err)
	assert.Equal(t, SessionStatusTerminating, updated.Status)
}
