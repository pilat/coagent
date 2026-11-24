package sessionstore

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchSignals_FireArmRead(t *testing.T) {
	t.Run("one-shot fires once", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		require.NoError(t, store.WithTx(t.Context(), func(tx *sql.Tx) error {
			_, err := ArmWatchTx(t.Context(), tx, watcher, target, false)

			return err
		}))
		fire := WatchFire{TargetID: target, Line: "host line", Notice: "host notice"}
		count, err := store.FireWatches(t.Context(), fire)
		require.NoError(t, err)
		assert.Equal(t, 1, count)
		rows, err := store.ListPending(t.Context(), watcher)
		require.NoError(t, err)
		require.Len(t, rows, 1)
		assert.Equal(t, InputSourceSubagent, rows[0].Source)
		assert.Equal(t, fire.Line, rows[0].RawContent)
		assert.EqualValues(t, target, rows[0].Attributes[InputAttributeSignalTargetID])
		assert.Equal(t, fire.Notice, rows[0].Attributes[InputAttributeSignalNotice])
		count, err = store.FireWatches(t.Context(), fire)
		require.NoError(t, err)
		assert.Zero(t, count)
		assert.Equal(t, WatchState{Signalled: true}, signalTestState(t, store, watcher, target))
	})

	t.Run("standing fires again only after empty read", func(t *testing.T) {
		store, db, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, true)
		signalTestFire(t, store, target, 1)
		signalTestFire(t, store, target, 0)
		_, err := db.ExecContext(t.Context(), `UPDATE session_inbox SET state = 'handled',
			resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'test' WHERE session_id = ?`, watcher)
		require.NoError(t, err)
		page, err := store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher, Chain: []int64{target}, Latest: true, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		assert.Empty(t, page.Rows)
		assert.Equal(t, WatchState{Subscribed: true}, signalTestState(t, store, watcher, target))
		signalTestFire(t, store, target, 1)
	})

	t.Run("one-shot and standing give one signal", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, true)
		signalTestArm(t, store, watcher, target, false)
		signalTestFire(t, store, target, 1)
		assert.Equal(t, WatchState{Subscribed: true, Signalled: true}, signalTestState(t, store, watcher, target))
		was, err := store.Unsubscribe(t.Context(), watcher, target)
		require.NoError(t, err)
		assert.True(t, was)
		signalTestArm(t, store, watcher, target, false)
		was, err = store.Unsubscribe(t.Context(), watcher, target)
		require.NoError(t, err)
		assert.False(t, was)
		assert.True(t, signalTestState(t, store, watcher, target).Armed)
	})

	t.Run("arm positions at end and preserves unread signal", func(t *testing.T) {
		store, db, watcher, target := signalTestPair(t)
		var last int64
		for range 3 {
			last = watchTestOutput(t, store, target, "", "old")
		}
		signalTestArm(t, store, watcher, target, false)
		watchTestOutput(t, store, target, "", "new")
		signalTestFire(t, store, target, 1)
		signalTestArm(t, store, watcher, target, true)
		signalTestArm(t, store, watcher, target, false)
		var cursor int64
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT read_through FROM session_watches
			WHERE watcher_id = ? AND target_id = ?`, watcher, target).Scan(&cursor))
		assert.Equal(t, last, cursor)
		assert.Equal(
			t,
			WatchState{Subscribed: true, Armed: true, Signalled: true},
			signalTestState(t, store, watcher, target),
		)
		page, err := store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher, Chain: []int64{target}, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"new"}, watchContents(page))
		assert.Equal(t, WatchState{Subscribed: true, Armed: true}, signalTestState(t, store, watcher, target))
	})

	t.Run("read before acceptance", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		signalTestArm(t, store, watcher, target, true)
		signalTestFire(t, store, target, 1)
		_, err := store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher, Chain: []int64{target}, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		signalTestFire(t, store, target, 0)
		rows, err := store.ListPending(t.Context(), watcher)
		require.NoError(t, err)
		assert.Len(t, rows, 1)
		assert.True(t, signalTestState(t, store, watcher, target).Signalled)
	})

	t.Run("mode off and killed watcher", func(t *testing.T) {
		for _, killed := range []bool{false, true} {
			store, _, watcher, target := signalTestPair(t)
			signalTestArm(t, store, watcher, target, true)
			signalTestArm(t, store, watcher, target, false)
			if killed {
				_, err := store.MarkSessionKilledWithOutput(t.Context(), watcher, 0)
				require.NoError(t, err)
			} else {
				_, err := store.SetOrchestrator(t.Context(), watcher, false)
				require.NoError(t, err)
			}
			signalTestFire(t, store, target, 0)
			assert.Equal(t, WatchState{Subscribed: true}, signalTestState(t, store, watcher, target))
			rows, err := store.ListPending(t.Context(), watcher)
			require.NoError(t, err)
			assert.Empty(t, rows)
		}
	})

	t.Run("clear moves target rows only", func(t *testing.T) {
		store, db, watcher, target := signalTestPair(t)
		cursor := watchTestOutput(t, store, target, "", "old")
		signalTestArm(t, store, watcher, target, true)
		signalTestArm(t, store, target, watcher, false)
		replacement, _, err := store.ReplaceManagerRoot(t.Context(), target, "test", "/project")
		require.NoError(t, err)
		assert.Equal(t, WatchState{Subscribed: true}, signalTestState(t, store, watcher, replacement.ID))
		assert.True(t, signalTestState(t, store, target, watcher).Armed)
		var position int64
		require.NoError(t, db.QueryRowContext(t.Context(), `SELECT read_through FROM session_watches
			WHERE watcher_id = ? AND target_id = ?`, watcher, replacement.ID).Scan(&position))
		assert.Equal(t, cursor, position)
		signalTestArm(t, store, watcher, replacement.ID, false)
		signalTestFire(t, store, replacement.ID, 1)
		_, err = store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher, Chain: []int64{target, replacement.ID}, Latest: true, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		assert.Equal(t, WatchState{Subscribed: true}, signalTestState(t, store, watcher, replacement.ID))
		assert.True(t, signalTestState(t, store, target, watcher).Armed)
	})

	t.Run("ordinary read defaults to no signal state", func(t *testing.T) {
		store, _, watcher, target := signalTestPair(t)
		watchTestOutput(t, store, target, "", "answer")
		_, err := store.ReadWatch(t.Context(), WatchRead{
			WatcherID: watcher, Chain: []int64{target}, MaxRows: 30, MaxRunes: 16000,
		})
		require.NoError(t, err)
		assert.Equal(t, WatchState{}, signalTestState(t, store, watcher, target))
		signalTestFire(t, store, target, 0)
	})
}

func TestWatchSignals_ActorIsNotSignalled(t *testing.T) {
	store, _, actor, target := signalTestPair(t)
	record, err := store.GetSession(t.Context(), target)
	require.NoError(t, err)
	other := chatTestRoot(t, store, record.ProjectID)
	_, err = store.SetOrchestrator(t.Context(), other.ID, true)
	require.NoError(t, err)
	signalTestArm(t, store, actor, target, false)
	signalTestArm(t, store, actor, target, true)
	signalTestArm(t, store, other.ID, target, false)

	count, err := store.FireWatches(t.Context(), WatchFire{
		TargetID: target, ActorID: actor, Line: "host line", Notice: "host notice",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, count)
	assert.Equal(t, WatchState{Subscribed: true}, signalTestState(t, store, actor, target))
	assert.Equal(t, WatchState{Signalled: true}, signalTestState(t, store, other.ID, target))
	rows, err := store.ListPending(t.Context(), actor)
	require.NoError(t, err)
	assert.Empty(t, rows)
	rows, err = store.ListPending(t.Context(), other.ID)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "host line", rows[0].RawContent)
}

func signalTestPair(t *testing.T) (*Store, *sql.DB, int64, int64) {
	t.Helper()
	store, db, project := newTestStore(t)
	watcher := chatTestRoot(t, store, project)
	target := chatTestRoot(t, store, project)
	_, err := store.SetOrchestrator(t.Context(), watcher.ID, true)
	require.NoError(t, err)
	return store, db, watcher.ID, target.ID
}

func signalTestState(t *testing.T, store *Store, watcher, target int64) WatchState {
	t.Helper()
	states, err := store.WatchStates(t.Context(), watcher)
	require.NoError(t, err)
	require.Contains(t, states, target)
	return states[target]
}

func signalTestFire(t *testing.T, store *Store, target int64, want int) {
	t.Helper()
	count, err := store.FireWatches(t.Context(), WatchFire{TargetID: target, Line: "host line", Notice: "host notice"})
	require.NoError(t, err)
	assert.Equal(t, want, count)
}

func signalTestArm(t *testing.T, store *Store, watcher, target int64, standing bool) bool {
	t.Helper()
	unread, err := store.ArmWatch(t.Context(), watcher, target, standing)
	require.NoError(t, err)

	return unread
}
