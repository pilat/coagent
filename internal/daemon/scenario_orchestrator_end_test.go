package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestOrchestratorStopSpawnedRunning(t *testing.T) {
	// Delegated stops use the human's durable flow and identify the actor to other watchers.
	h, caller, target, watcher := runningEndTarget(t)
	result, err := endToolResult(t, h, caller, target, orchestrator.IDStopSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Stopped session #%d; its running turn was cancelled.", target), result.Output)
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(target).Status)
	assertEndCommand(t, h, caller, target, "/stop", "⏳ Stopping…")
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == sessionstore.StopTerminalContent+"\nCancelled background processes: 0"
	}))
	assertOutcomeSignal(t, h, watcher, target, target,
		fmt.Sprintf("was stopped by session #%d with stop_session", caller), fmt.Sprintf("was stopped by #%d", caller))
	assertEndActorQuiet(t, h, caller, target)
}

func TestOrchestratorStopNothingToStop(t *testing.T) {
	// An idle or already stopped target creates no command, output or outcome signal.
	h := orchestratorProjectHarness(t)
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "finish")
	waitSignalTurn(t, h, target, "done")
	for _, state := range []string{"idle", "stopped"} {
		if state == "stopped" {
			h.say(target, "/stop")
		}
		before, signals := endRowCounts(t, h, target), signalRows(t, h, caller, target)
		result, err := endToolResult(t, h, caller, target, orchestrator.IDStopSession)
		require.NoError(t, err)
		assert.Equal(t, fmt.Sprintf("Session #%d is %s; nothing to stop.", target, state), result.Output)
		assert.Equal(t, before, endRowCounts(t, h, target))
		assert.Equal(t, signals, signalRows(t, h, caller, target))
	}
}

func TestOrchestratorStopCancelsQueuedSpawnPrompt(t *testing.T) {
	// A queued spawn prompt is cancelled without ever starting its runner.
	h := orchestratorProjectHarness(t)
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "still queued")
	assert.False(t, h.mgr.HasActiveLoop(target))
	result, err := endToolResult(t, h, caller, target, orchestrator.IDStopSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf(
		"Stopped session #%d; it had no running turn, and its queued and background work was cancelled.",
		target,
	), result.Output)
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(target).Status)
	var state string
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT state FROM session_inbox WHERE session_id = ? AND source = 'agent'`, target).Scan(&state))
	assert.Equal(t, "cancelled", state)
	assert.False(t, h.mgr.HasActiveLoop(target))
}

func TestOrchestratorKillSpawned(t *testing.T) {
	// Kill is durable and idempotent, names its actor, and preserves the project directory.
	h, caller, target, watcher := runningEndTarget(t)
	path, err := h.store.GetProjectWorkDir(h.ctx, h.session(target).ProjectID)
	require.NoError(t, err)
	result, err := endToolResult(t, h, caller, target, orchestrator.IDKillSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Killed session #%d.", target), result.Output)
	assert.NotNil(t, h.session(target).KilledAt)
	assertEndCommand(t, h, caller, target, "/kill", "Stopping session...")
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == "Session killed. Cancelled background processes: 0"
	}))
	assertOutcomeSignal(t, h, watcher, target, target,
		fmt.Sprintf("was killed by session #%d with kill_session", caller), fmt.Sprintf("was killed by #%d", caller))
	assertEndActorQuiet(t, h, caller, target)
	before := endRowCounts(t, h, target)
	result, err = endToolResult(t, h, caller, target, orchestrator.IDKillSession)
	require.NoError(t, err)
	assert.Equal(t, fmt.Sprintf("Session #%d was already killed.", target), result.Output)
	_, err = endToolResult(t, h, caller, target, orchestrator.IDStopSession)
	require.EqualError(t, err, fmt.Sprintf("session #%d is killed", target))
	assert.Equal(t, before, endRowCounts(t, h, target))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}
