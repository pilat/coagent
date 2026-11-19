package daemon

import (
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestOrchestratorKillFromInsideATurnSurvivesCallerStop(t *testing.T) {
	// A target's lifecycle command completes even when the caller's turn is stopped midway.
	bootstrapEntered, bootstrapRelease := make(chan struct{}), make(chan struct{})
	var turns scriptTurns
	h := orchestratorProjectHarness(t, func(system string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BLOCK_TARGET") {
			close(bootstrapEntered)
			<-bootstrapRelease
			return callReply("target-bootstrap", "ls", `{"path":"."}`)
		}
		return turns.respond(system, messages)
	})
	t.Cleanup(func() {
		closeOnce(bootstrapRelease)
	})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "BLOCK_TARGET")
	waitForScenarioSignal(t, bootstrapEntered, "target bootstrap provider entered")
	hold := h.holdRunner(target)
	close(bootstrapRelease)
	waitForScenarioSignal(t, hold.entered, "target runner entered blocking client")
	turns = scriptTurns{{marker: "END_TARGET", calls: []scriptCall{{
		id: "end-target", name: orchestrator.IDKillSession, params: fmt.Sprintf(`{"session_id":%d}`, target),
	}}, answer: "target killed"}}
	h.say(caller, "END_TARGET")
	waitForScenarioSignal(t, hold.cancelled, "kill cancelled the target runner")
	require.Equal(t, sessionstore.SessionStatusTerminating, h.session(target).Status)
	callerRunner, ok := h.mgr.runners.load(caller)
	require.True(t, ok)
	callerCancelled := make(chan struct{})
	cancelCaller := callerRunner.cancel
	var cancelOnce sync.Once
	// The stopping row precedes cancellation; wait for the actual cancel before releasing the target.
	callerRunner.cancel = func() {
		cancelCaller()
		cancelOnce.Do(func() { close(callerCancelled) })
	}
	stopped := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		stopped <- h.mgr.sendToSession(h.ctx, caller, "/stop")
		close(done)
	}()
	waitForScenarioSignal(t, callerCancelled, "human stop cancelled the caller runner")
	require.Equal(t, sessionstore.SessionStatusStopping, h.session(caller).Status)
	closeOnce(hold.release)
	require.True(t, closedInTime(done), "caller stop must complete")
	require.NoError(t, <-stopped)
	require.NotNil(t, h.session(target).KilledAt, "target kill must complete after the caller's context is cancelled")
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(caller).Status)
	for _, id := range []int64{caller, target} {
		assert.NotEqual(t, sessionstore.SessionStatusStopping, h.session(id).Status)
		assert.NotEqual(t, sessionstore.SessionStatusTerminating, h.session(id).Status)
		assert.False(t, h.mgr.HasActiveLoop(id))
	}
	closed := 0
	for _, row := range h.outbox(target) {
		if row.Type == "session_closed" {
			closed++
			assert.Equal(t, "Session killed. Cancelled background processes: 0", row.Content)
		}
	}
	assert.Equal(t, 1, closed)
}
