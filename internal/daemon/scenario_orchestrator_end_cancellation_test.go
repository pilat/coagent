package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
)

type endBlockingClient struct {
	*scriptedLLM
	entered   chan struct{}
	cancelled chan struct{}
	release   <-chan struct{}
}

func TestOrchestratorKillFromInsideATurnSurvivesCallerStop(t *testing.T) {
	// A target's lifecycle command completes even when the caller's turn is stopped midway.
	entered, release := make(chan struct{}), make(chan struct{})
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
		closeOnce(release)
	})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "BLOCK_TARGET")
	waitForScenarioSignal(t, bootstrapEntered, "target bootstrap provider entered")
	targetRunner, ok := h.mgr.runners.load(target)
	require.True(t, ok)
	targetSession := targetRunner.Service()
	require.NotNil(t, targetSession)
	blocked := &endBlockingClient{
		scriptedLLM: &scriptedLLM{},
		entered:     entered,
		cancelled:   make(chan struct{}),
		release:     release,
	}
	// A blocked HTTP handler cannot hold its cancelled client; install this on the runner's next model step.
	targetSession.SwitchModel(blocked, sessionprompt.ModelSection{ID: h.session(target).Model})
	close(bootstrapRelease)
	waitForScenarioSignal(t, entered, "target runner entered blocking client")
	turns = scriptTurns{{marker: "END_TARGET", calls: []scriptCall{{
		id: "end-target", name: orchestrator.IDKillSession, params: fmt.Sprintf(`{"session_id":%d}`, target),
	}}, answer: "target killed"}}
	h.say(caller, "END_TARGET")
	waitForScenarioSignal(t, blocked.cancelled, "kill cancelled the target runner")
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
	closeOnce(release)
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

func (c *endBlockingClient) Chat(
	ctx context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	// Hold the runner after cancellation so the caller can be stopped before target cleanup resumes.
	close(c.entered)
	<-ctx.Done()
	close(c.cancelled)
	<-c.release
	return nil, ctx.Err()
}
