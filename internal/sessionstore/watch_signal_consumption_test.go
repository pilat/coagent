package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWatchSignals_UnreadOutcomeConsumesNewArm(t *testing.T) {
	for _, outcome := range []string{"answer", "killed", "cleared"} {
		t.Run(outcome, func(t *testing.T) {
			store, db, watcher, target := signalTestPair(t)
			signalTestArm(t, store, watcher, target, false)
			signalTestFire(t, store, target, 1)
			_, err := db.ExecContext(t.Context(), `UPDATE session_inbox SET state = 'handled',
				resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'test' WHERE session_id = ?`, watcher)
			require.NoError(t, err)
			signalTestArm(t, store, watcher, target, false)
			assert.Equal(t, WatchState{Armed: true, Signalled: true}, signalTestState(t, store, watcher, target))

			chain := []int64{target}
			switch outcome {
			case "killed":
				_, err = store.MarkSessionKilledWithOutput(t.Context(), target, 0)
				require.NoError(t, err)
			case "cleared":
				replacement, _, err := store.ReplaceManagerRoot(t.Context(), target, "test", "/project")
				require.NoError(t, err)
				target = replacement.ID
				chain = append(chain, target)
			}

			signalTestFire(t, store, target, 0)
			assert.Equal(t, WatchState{Signalled: true}, signalTestState(t, store, watcher, target))
			_, err = store.ReadWatch(t.Context(), WatchRead{
				WatcherID: watcher, Chain: chain, MaxRows: 30, MaxRunes: 16000,
			})
			require.NoError(t, err)
			assert.Equal(t, WatchState{}, signalTestState(t, store, watcher, target))
			signalTestWake(t, store, watcher, false)
		})
	}
}

func TestWatchSignals_ReadBeforeFollowUpOutcomeKeepsArm(t *testing.T) {
	store, db, watcher, target := signalTestPair(t)
	assert.False(t, signalTestArm(t, store, watcher, target, false))
	signalTestFire(t, store, target, 1)
	_, err := db.ExecContext(t.Context(), `UPDATE session_inbox SET state = 'handled',
		resolved_at = CURRENT_TIMESTAMP, resolution_reason = 'test' WHERE session_id = ?`, watcher)
	require.NoError(t, err)
	assert.True(t, signalTestArm(t, store, watcher, target, false))
	_, err = store.ReadWatch(t.Context(), WatchRead{
		WatcherID: watcher, Chain: []int64{target}, MaxRows: 30, MaxRunes: 16000,
	})
	require.NoError(t, err)
	assert.Equal(t, WatchState{Armed: true}, signalTestState(t, store, watcher, target))
	signalTestFire(t, store, target, 1)
	assert.Equal(t, WatchState{Signalled: true}, signalTestState(t, store, watcher, target))
}
