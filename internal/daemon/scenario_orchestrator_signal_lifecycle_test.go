package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestOrchestratorSignalOneUnreadUntilRead(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	host := orchestratorHost{s: h.mgr}
	_, _, err := host.Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	_, _, err = host.SendToSession(h.ctx, watcher, target, "first")
	require.NoError(t, err)
	h.waitAnswer(target, "done")
	assertAnswerSignal(t, h, watcher, target)
	h.waitAnswer(watcher, "done")
	signalReply(t, h, target, "second", "done")
	assert.Equal(t, 1, signalRows(t, h, watcher, target))
	_, err = host.ReadSession(h.ctx, watcher, target, true)
	require.NoError(t, err)
	signalReply(t, h, target, "third", "done")
	h.waitUntil("standing signal after read", func() bool { return signalRows(t, h, watcher, target) == 2 })
}

func TestOrchestratorSignalRespectsWatcherStop(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	h.say(watcher, "/stop")
	h.waitUntil(
		"watcher stopped",
		func() bool { return h.session(watcher).Status == sessionstore.SessionStatusStopped },
	)
	fireOrchestratorBudget(t, h, watcher)
	_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, watcher, target, "work")
	require.NoError(t, err)
	h.waitAnswer(target, "done")
	h.waitUntil("stopped watcher signal pending", func() bool { return signalRows(t, h, watcher, target) == 1 })
	assertStoppedSignal(t, h, watcher)
	record, err := h.store.Get(h.ctx, watcher)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	h.say(watcher, "continue")
	h.waitAnswer(watcher, "done")
	assertAnswerSignal(t, h, watcher, target)
	assert.True(t, hasUserContaining(h.messages(watcher), "continue"))
	var signalPosition, humanPosition int
	for i, message := range h.messages(watcher) {
		if strings.Contains(message.Content, "[Signal from coagent:") {
			signalPosition = i
		}
		if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "continue") {
			humanPosition = i
		}
	}
	assert.Less(t, signalPosition, humanPosition)
}

func assertStoppedSignal(t *testing.T, h *harness, watcher int64) {
	t.Helper()
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(watcher).Status)
	assert.False(t, h.mgr.HasActiveLoop(watcher))
	var pending int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE session_id = ? AND source = 'subagent' AND state = 'pending'`, watcher).Scan(&pending))
	assert.Equal(t, 1, pending)
	assert.False(t, hasUserContaining(h.messages(watcher), "[Signal from coagent:"))
	assert.Zero(t, h.countOutbox(watcher, func(content string) bool { return len(content) >= 4 && content[:4] == "🔔" }))
}

func TestOrchestratorSignalStopsWhenModeOff(t *testing.T) {
	for _, standing := range []bool{false, true} {
		t.Run(map[bool]string{false: "one-shot", true: "standing"}[standing], func(t *testing.T) {
			release := make(chan struct{})
			h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasUserContaining(messages, "BLOCK_TARGET") {
					<-release
				}
				return textReply("done")
			}})
			defer closeOnce(release)
			h.startInboxWake()
			watcher := modeRoot(t, h)
			target := h.createRoot(managerAttrs("owner"))
			host := orchestratorHost{s: h.mgr}
			if standing {
				_, _, err := host.Subscribe(h.ctx, watcher, target)
				require.NoError(t, err)
			}
			_, _, err := host.SendToSession(h.ctx, watcher, target, "BLOCK_TARGET")
			require.NoError(t, err)
			_, err = h.store.SetOrchestrator(h.ctx, watcher, false)
			require.NoError(t, err)
			close(release)
			waitSignalTurn(t, h, target, "done")
			assert.Zero(t, signalRows(t, h, watcher, target))
			state, err := h.store.WatchStates(h.ctx, watcher)
			require.NoError(t, err)
			assert.False(t, state[target].Armed)
			_, err = h.store.SetOrchestrator(h.ctx, watcher, true)
			require.NoError(t, err)
			signalReply(t, h, target, "next", "done")
			if standing {
				assertAnswerSignal(t, h, watcher, target)
			} else {
				assert.Zero(t, signalRows(t, h, watcher, target))
			}
		})
	}
}

func TestOrchestratorSignalSurvivesRestart(t *testing.T) {
	t.Run("target mid-turn", func(t *testing.T) {
		release := make(chan struct{})
		h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			<-release
			return textReply("unreached")
		}})
		defer closeOnce(release)
		h.startInboxWake()
		watcher := modeRoot(t, h)
		target := h.createRoot(managerAttrs("owner"))
		_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, watcher, target, "blocked work")
		require.NoError(t, err)
		h.waitUntil("target entered", func() bool { return hasUserContaining(h.messages(target), "blocked work") })
		h = h.restart(trivialRespond)
		h.resume()
		h.waitAnswer(target, "done")
		assertAnswerSignal(t, h, watcher, target)
	})
	t.Run("stopped watcher pending signal", func(t *testing.T) {
		h := newHarness(t, harnessOptions{respond: trivialRespond})
		h.startInboxWake()
		watcher := modeRoot(t, h)
		target := h.createRoot(managerAttrs("owner"))
		h.say(watcher, "/stop")
		h.waitUntil("stopped", func() bool { return h.session(watcher).Status == sessionstore.SessionStatusStopped })
		_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, watcher, target, "work")
		require.NoError(t, err)
		h.waitAnswer(target, "done")
		h.waitUntil("signal pending", func() bool { return signalRows(t, h, watcher, target) == 1 })
		h = h.restart(trivialRespond)
		h.resume()
		assertStoppedSignal(t, h, watcher)
		assert.Equal(t, 1, signalRows(t, h, watcher, target))
	})
}

func TestOrchestratorClearedWatcherGoesInert(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	_, _, err := (orchestratorHost{s: h.mgr}).Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	replacement := clearReadTarget(t, h, watcher)
	signalReply(t, h, target, "work", "done")
	assert.Zero(t, signalRows(t, h, watcher, target))
	assert.Zero(t, signalRows(t, h, replacement, target))
}
