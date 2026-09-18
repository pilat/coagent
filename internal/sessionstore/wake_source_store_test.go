package sessionstore

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWakeSource_AdvertisedProcessIsAWakeSource(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)

	has, err := store.HasBackgroundWakeSource(ctx, sessionID)
	require.NoError(t, err)
	assert.False(t, has)

	now := time.Now().UTC()
	_, err = db.ExecContext(ctx, `INSERT INTO background_processes
		(id, session_id, root_session_id, tool_call_id, output_path, deadline_at, created_at,
		 advertised_at, state)
		VALUES ('proc-1', ?, ?, 'call-1', '/tmp/out', ?, ?, ?, 'running')`,
		sessionID, sessionID, now.Add(time.Hour), now, now)
	require.NoError(t, err)

	has, err = store.HasBackgroundWakeSource(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestWakeSource_UndeliveredBackgroundLinkStates(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	for _, state := range []string{"spawned", "running", "completed", "error"} {
		parent, err := store.CreateSession(ctx, projectID, "m", "", nil)
		require.NoError(t, err)
		childID, err := store.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
		require.NoError(t, err)

		_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
			(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
			VALUES (?, ?, ?, 0, 0, ?, ?)`, parent.ID, childID, "task-1", state, time.Now().UTC().Unix())
		require.NoError(t, err)

		has, err := store.HasBackgroundWakeSource(ctx, parent.ID)
		require.NoError(t, err)
		assert.True(t, has, "state %s must be a wake source", state)
	}

	for _, state := range []string{"stopped", "killed"} {
		parent, err := store.CreateSession(ctx, projectID, "m", "", nil)
		require.NoError(t, err)
		childID, err := store.CreateSubagentSession(ctx, projectID, parent.ID, parent.ID, "general", "m", "")
		require.NoError(t, err)

		_, err = db.ExecContext(ctx, `INSERT INTO subagent_links
			(parent_id, child_id, task_call_id, blocking, depth, state, created_at)
			VALUES (?, ?, ?, 0, 0, ?, ?)`, parent.ID, childID, "task-1", state, time.Now().UTC().Unix())
		require.NoError(t, err)

		has, err := store.HasBackgroundWakeSource(ctx, parent.ID)
		require.NoError(t, err)
		assert.False(t, has, "state %s must not be a wake source", state)
	}
}

func TestWakeSource_PendingAsyncInboxIsAWakeSource(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, nil, projectID)

	_, err := store.EnqueueAsyncInput(ctx, sessionID, InputSourceProcess, "process done", nil)
	require.NoError(t, err)

	has, err := store.HasBackgroundWakeSource(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, has)
}
