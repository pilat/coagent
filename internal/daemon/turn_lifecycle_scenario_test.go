package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// A scheduled turn bypasses the inbox: its announcement and its narration form
// a new generation even though nothing was promoted.
func TestHarnessScenario_ScheduledTurnChain(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "schedule") {
			return &llmwire.Response{Text: "Report ready."}
		}

		return &llmwire.Response{Text: "Hi there."}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "hello", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.waitUntil("first turn completed", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusCompleted
	})
	// Wait for the loop teardown so the scheduled turn deterministically
	// re-announces the session instead of racing the runner cleanup.
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })

	delivered, err := h.mgr.DeliverScheduleTick(h.ctx, root, "delivery-chain-1", "produce the weekly report")
	require.NoError(t, err)
	require.True(t, delivered)

	waitForVisibleMessage(t, collector, root, "Report ready.")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "scheduled_turn_chain.json", controller)
	waitForIdleAfterMessage(t, collector, root, "Report ready.")

	assertHarnessTrace(t, "scheduled_turn_chain.json", collector.snapshot(), root)
}

// A live explicit stop: the fence and its replaceable start row commit while
// the model call is in flight, cleanup settles the unresolved call, and one
// terminal transaction releases the root with its persistent completion.
func TestHarnessScenario_LiveStopChain(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce bool
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		if !releaseOnce {
			releaseOnce = true
			close(entered)
			<-release
		}

		return &llmwire.Response{Text: "never reached"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		close(release)
		h.shutdown()
	}()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")

	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})

	controller := newChainController(t, h)
	drainScenarioClaims(t, "stop_live_chain.json", controller)
	collector.waitFor(t, "stopped readiness", func(events []controllerapi.SessionNotification) bool {
		return containsStateWithReason(events, root, sessionevent.StateIdle, "stopped")
	})

	assertHarnessTrace(t, "stop_live_chain.json", collector.snapshot(), root)
}

// An interrupted explicit stop converges on restart: the first process dies
// right after the fence commit; the next one finishes cleanup and commits the
// same terminal transaction without running any model or tool work.
func TestHarnessScenario_InterruptedStopChain(t *testing.T) {
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		return &llmwire.Response{Text: "must not run"}
	}

	dbPath := filepath.Join(t.TempDir(), "interrupted-stop.db")
	h1 := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	root, err := h1.sessStore.CreateSession(h1.ctx, h1.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, h1.sessStore.BindManager(h1.ctx, scenarioManagerID, "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	}))
	input, err := h1.sessStore.EnqueueInput(h1.ctx, root.ID, sessionstore.InputSourceUser, "/stop")
	require.NoError(t, err)
	_, err = h1.sessStore.BeginLifecycleInput(h1.ctx, input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	h1.shutdown()

	var modelCalls int
	respond2 := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls++

		return &llmwire.Response{Text: "must not run"}
	}
	h2 := newSubagentHarnessOnDB(t, dbPath, respond2, nil)
	defer h2.shutdown()
	require.NoError(t, h2.mgr.Start(h2.ctx))

	record, err := h2.sessStore.GetSession(h2.ctx, root.ID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	require.Zero(t, modelCalls, "stop recovery must never run the model")

	collector := collectEvents(h2.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	controller := newChainController(t, h2)
	drainScenarioClaims(t, "stop_interrupted_chain.json", controller)
	collector.waitFor(t, "stopped readiness", func(events []controllerapi.SessionNotification) bool {
		return containsStateWithReason(events, root.ID, sessionevent.StateIdle, "stopped")
	})

	assertHarnessTrace(t, "stop_interrupted_chain.json", collector.snapshot(), root.ID)
}

// A later ordinary input on a stopped root is a fresh generation on preserved
// history: the pre-stop progress chain is never edited again.
func TestHarnessScenario_LaterFreshTurnAfterStop(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "continue please") {
			return &llmwire.Response{Text: "Resumed and done."}
		}

		if hasToolResultFor(msgs, "ls") {
			return &llmwire.Response{Text: "Working on it, done for now."}
		}

		return &llmwire.Response{
			Text: "Working on it",
			ToolCalls: []llmwire.ToolCall{{
				ID: "fresh-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
			}},
		}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, root, "Working on it, done for now.")
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })

	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})

	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "continue please"))
	waitForVisibleMessage(t, collector, root, "Resumed and done.")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "stop_then_fresh_turn.json", controller)
	waitForIdleAfterMessage(t, collector, root, "Resumed and done.")

	assertHarnessTrace(t, "stop_then_fresh_turn.json", collector.snapshot(), root)
}

// Explicit compact success remains the regression reference: a replaceable
// start becomes a persistent terminal result.
func TestHarnessScenario_CompactSuccessChain(t *testing.T) {
	var calls int
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return &llmwire.Response{Text: "First answer."}
		}

		// Later calls serve both the compaction summary and the post-compact
		// turn; the summary text lands in the brief either way.
		return &llmwire.Response{Text: "Compacted summary."}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer collector.stop()

	root, err := h.mgr.Send(h.ctx, h.projectID, "first prompt", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	waitForVisibleMessage(t, collector, root, "First answer.")

	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/compact keep the TODO state"))
	collector.waitFor(t, "compaction finished", func(events []controllerapi.SessionNotification) bool {
		return containsMessage(events, root, "✅ Context compacted")
	})

	controller := newChainController(t, h)
	drainScenarioClaims(t, "compact_success_chain.json", controller)
	collector.waitFor(t, "idle after compact", func(events []controllerapi.SessionNotification) bool {
		return containsState(events, root, sessionevent.StateIdle)
	})

	assertHarnessTrace(t, "compact_success_chain.json", collector.snapshot(), root)
}

func containsMessage(
	events []controllerapi.SessionNotification,
	sessionID int64,
	message string,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Message == message {
			return true
		}
	}

	return false
}

func containsState(
	events []controllerapi.SessionNotification,
	sessionID int64,
	status sessionevent.State,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Status == status {
			return true
		}
	}

	return false
}

func containsStateWithReason(
	events []controllerapi.SessionNotification,
	sessionID int64,
	status sessionevent.State,
	reason string,
) bool {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Status == status &&
			event.Notification.Reason == reason {
			return true
		}
	}

	return false
}
