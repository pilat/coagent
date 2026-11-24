package daemon

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestTurnOverFollowsTheTurn(t *testing.T) {
	t.Run("input through answer and lifecycle", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h := orchestratorProjectHarness(t, func(string, []llmwire.Message) *llmwire.Response {
			once.Do(func() { close(entered) })
			<-release
			return textReply("done")
		})
		t.Cleanup(func() { closeOnce(release) })
		target, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
		require.NoError(t, err)
		assertAppTurnOver(t, h, target, target, true)
		_, err = h.mgr.SendForApp(h.ctx, "bot", target, "work", "")
		require.NoError(t, err)
		assertAppTurnOver(t, h, target, target, false)
		h.startInboxWake()
		waitForScenarioSignal(t, entered, "app turn provider entered")
		assertAppTurnOver(t, h, target, target, false)
		closeOnce(release)
		waitSignalTurn(t, h, target, "done")
		assertAppTurnOver(t, h, target, target, true)
		replacement := clearReadTarget(t, h, target)
		assertAppTurnOver(t, h, target, replacement, true)
		for _, state := range []sessionstore.SessionStatus{
			sessionstore.SessionStatusSuspended, sessionstore.SessionStatusStopping,
		} {
			require.NoError(t, h.store.UpdateSessionStatus(h.ctx, replacement, state))
			assertAppTurnOver(t, h, target, replacement, false)
		}
		require.NoError(t, h.store.UpdateSessionStatus(h.ctx, replacement, sessionstore.SessionStatusStopped))
		fireOrchestratorBudget(t, h, replacement)
		assertAppTurnOver(t, h, target, replacement, true)
		_, err = h.mgr.KillForApp(h.ctx, "bot", target)
		require.NoError(t, err)
		assertAppTurnOver(t, h, target, replacement, true)
	})
	t.Run("run error with pending input", func(t *testing.T) {
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
			once.Do(func() { close(entered) })
			<-release
			return nil
		}})
		t.Cleanup(func() { closeOnce(release) })
		target := h.createRoot(nil)
		require.NoError(t, h.mgr.sendToSession(h.ctx, target, "A"))
		waitForScenarioSignal(t, entered, "failing provider call")
		require.NoError(t, h.mgr.sendToSession(h.ctx, target, "B"))
		assertAppTurnOver(t, h, target, target, false)
		closeOnce(release)
		h.waitUntil("failed turn settled", func() bool {
			return h.session(target).Status == sessionstore.SessionStatusError && !h.mgr.HasActiveLoop(target)
		})
		pending, err := h.store.ListPending(h.ctx, target)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, "B", pending[0].RawContent)
		assertAppTurnOver(t, h, target, target, true)
	})
	t.Run("errored root waiting for admission", func(t *testing.T) {
		h := newHarness(t, harnessOptions{respond: trivialRespond})
		target := h.createRoot(nil)
		require.NoError(t, h.store.UpdateSessionStatus(h.ctx, target, sessionstore.SessionStatusError))
		held := 0
		t.Cleanup(func() {
			for held > 0 {
				h.mgr.runners.release(false, 0)
				held--
			}
		})
		for range maxTotal {
			require.True(t, h.mgr.runners.tryAdmit(false, 0))
			held++
		}
		h.startInboxWake()
		_, err := h.mgr.SendForApp(h.ctx, "bot", target, "retry work", "")
		require.NoError(t, err)
		h.waitUntil("app retry deferred for admission", func() bool { return runnerWaitingCount(h.mgr.runners) == 1 })
		assert.False(t, h.mgr.HasActiveLoop(target))
		assert.Equal(t, sessionstore.SessionStatusError, h.session(target).Status)
		pending, err := h.store.ListPending(h.ctx, target)
		require.NoError(t, err)
		require.Len(t, pending, 1)
		assert.Equal(t, sessionstore.InputSourceAgent, pending[0].Source)
		assertAppTurnOver(t, h, target, target, false)
		h.mgr.runners.release(false, 0)
		held--
		h.mgr.drain(h.ctx)
		h.waitAnswer(target, "done")
		assertAppTurnOver(t, h, target, target, true)
	})
	t.Run("advertised background process", func(t *testing.T) {
		h := newHarness(t, harnessOptions{respond: trivialRespond})
		service := installScenarioProcessService(t, h)
		target := h.createRoot(nil)
		assertAppTurnOver(t, h, target, target, true)
		process := startScenarioProcess(t, service, target, target, "sleep 30")
		defer func() {
			_, err := service.CancelProcess(h.ctx, process.ID, backgroundprocess.IntentAgentCancelled)
			require.NoError(t, err)
		}()
		assertAppTurnOver(t, h, target, target, false)
	})
}

func assertAppTurnOver(t *testing.T, h *harness, requested, target int64, want bool) {
	t.Helper()
	id, over, err := h.mgr.TurnOver(h.ctx, requested)
	require.NoError(t, err)
	assert.Equal(t, target, id)
	assert.Equal(t, want, over)
}
