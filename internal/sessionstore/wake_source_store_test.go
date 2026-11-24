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

func TestWakeSource_ArmedWait(t *testing.T) {
	t.Run("standing is not a wake source", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, true)
		signalTestWake(t, store, watcher, false)
	})
	t.Run("one-shot and pending signal", func(t *testing.T) {
		store, db, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, false)
		signalTestWake(t, store, watcher, true)
		signalTestFire(t, store, target, 1)
		signalTestWake(t, store, watcher, true)
		_, err := db.ExecContext(t.Context(), `UPDATE session_inbox SET state = 'handled',
			resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'test' WHERE session_id = ?`, watcher)
		require.NoError(t, err)
		signalTestWake(t, store, watcher, false)
		signalTestArm(t, store, watcher, target, false)
		signalTestWake(t, store, watcher, false)
	})
	t.Run("mutual waits do not count", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		_, err := store.SetOrchestrator(t.Context(), target, true)
		require.NoError(t, err)
		signalTestArm(t, store, watcher, target, false)
		signalTestArm(t, store, target, watcher, false)
		signalTestWake(t, store, watcher, false)
		signalTestWake(t, store, target, false)
	})
	t.Run("mode off does not count", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, false)
		_, err := store.SetOrchestrator(t.Context(), watcher, false)
		require.NoError(t, err)
		signalTestWake(t, store, watcher, false)
	})
}

func TestWakeSource_InactiveReciprocalWait(t *testing.T) {
	for _, inactive := range []string{"mode off", "killed", "unread signal"} {
		t.Run(inactive, func(t *testing.T) {
			store, db, watcher, target := signalTestPair(t)
			_, err := store.SetOrchestrator(t.Context(), target, true)
			require.NoError(t, err)
			signalTestArm(t, store, watcher, target, false)
			signalTestArm(t, store, target, watcher, false)
			signalTestWake(t, store, watcher, false)
			signalTestWake(t, store, target, false)

			switch inactive {
			case "mode off":
				_, err = store.SetOrchestrator(t.Context(), watcher, false)
				require.NoError(t, err)
			case "killed":
				_, err = store.MarkSessionKilledWithOutput(t.Context(), watcher, 0)
				require.NoError(t, err)
			case "unread signal":
				signalTestFire(t, store, target, 1)
				signalTestArm(t, store, watcher, target, false)
				_, err = db.ExecContext(t.Context(), `UPDATE session_inbox SET state = 'handled',
					resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'test' WHERE session_id = ?`, watcher)
				require.NoError(t, err)
			}

			signalTestWake(t, store, watcher, false)
			signalTestWake(t, store, target, true)
		})
	}
}

func signalTestWake(t *testing.T, store *Store, watcher int64, want bool) {
	t.Helper()
	wake, err := store.HasBackgroundWakeSource(t.Context(), watcher)
	require.NoError(t, err)
	assert.Equal(t, want, wake)
	obligation, err := store.HasBackgroundObligationByRoot(t.Context(), watcher)
	require.NoError(t, err)
	assert.Equal(t, want, obligation)
}

func TestWakeSource_UndeliveredBackgroundLinkStates(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	for _, state := range []string{"spawned", "running", "completed", "error"} {
		parent, err := store.CreateSession(ctx, projectID, "m", "", nil)
		require.NoError(t, err)
		childID, err := createChild(ctx, store, projectID, parent.ID, parent.ID, "general", "m", "")
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
		childID, err := createChild(ctx, store, projectID, parent.ID, parent.ID, "general", "m", "")
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

	_, err := enqueueFact(ctx, store, sessionID, InputSourceProcess, "process done", nil)
	require.NoError(t, err)

	has, err := store.HasBackgroundWakeSource(ctx, sessionID)
	require.NoError(t, err)
	assert.True(t, has)
}

func TestWakeSource_LiveProcessesAreOwnAdvertisedAndRunning(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)
	sessionID := seedCompletionSession(t, store, db, projectID)
	other := seedCompletionSession(t, store, db, projectID)

	now := time.Now().UTC()
	insert := func(id string, owner int64, created time.Time, advertised any, state string, finished any) {
		t.Helper()
		_, err := db.ExecContext(ctx, `INSERT INTO background_processes
			(id, session_id, root_session_id, tool_call_id, output_path, deadline_at, created_at,
			 advertised_at, state, finished_at)
			VALUES (?, ?, ?, ?, '/tmp/out', ?, ?, ?, ?, ?)`,
			id, owner, owner, "call-"+id, now.Add(time.Hour), created, advertised, state, finished)
		require.NoError(t, err)
	}
	insert("later", sessionID, now.Add(time.Second), now, "running", nil)
	insert("earlier", sessionID, now, now, "running", nil)
	insert("foreground", sessionID, now, nil, "running", nil)
	insert("done", sessionID, now, now, "cancelled", now)
	insert("foreign", other, now, now, "running", nil)

	live, err := store.ListLiveProcesses(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, live, 2)
	assert.Equal(t, "earlier", live[0].ID)
	assert.Equal(t, "call-earlier", live[0].ToolCallID)
	assert.WithinDuration(t, now, live[0].CreatedAt, time.Second)
	assert.Equal(t, "later", live[1].ID)
}
