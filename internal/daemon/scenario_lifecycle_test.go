package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// Parent kill stops all unfinished descendants and records one warning per killed descendant across the depth limit.
func TestCascadeKill_BackgroundDescendant(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			entered <- struct{}{}
			<-release // hang until kill cancels the loop ctx

			return textReply("unreached")
		}
		return textReply("idle")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()
	ctx := h.ctx
	root := h.createRoot(nil)
	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })
	h.waitUntil("child model entered", func() bool { return len(entered) >= 1 })
	h.startInboxWake()
	grandchild, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: child.ChildID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background grandchild running", func() bool { return h.mgr.HasActiveLoop(grandchild.ChildID) })
	h.waitUntil("grandchild model entered", func() bool { return len(entered) >= 2 })
	childInput, err := h.store.Enqueue(
		ctx, sessionstore.Input{
			SessionID:  child.ChildID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "pending",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	grandchildInput, err := h.store.Enqueue(
		ctx, sessionstore.Input{
			SessionID:  grandchild.ChildID,
			Source:     sessionstore.InputSourceSubagent,
			Content:    "complete",
			Attributes: nil,
		},
	)
	require.NoError(t, err)

	// Capture WARN audit lines emitted during the cascade kill.
	core, logs := observer.New(zap.WarnLevel)
	killCtx := logger.ToContext(ctx, zap.New(core))
	require.NoError(t, h.mgr.sendToSession(killCtx, root, "/kill"))
	h.waitUntil("both descendants gone", func() bool {
		return !h.mgr.HasActiveLoop(child.ChildID) && !h.mgr.HasActiveLoop(grandchild.ChildID)
	})
	for _, id := range []int64{child.ChildID, grandchild.ChildID} {
		rec := h.session(id)
		assert.NotNil(t, rec.KilledAt, "descendant %d is killed with its tree", id)
	}
	for _, inputID := range []int64{childInput.Input.ID, grandchildInput.Input.ID} {
		var state string
		require.NoError(t, h.db.QueryRowContext(ctx,
			`SELECT state FROM session_inbox WHERE id = ?`, inputID,
		).Scan(&state))
		assert.Equal(t, string(sessionstore.InputStateCancelled), state)
	}
	assert.Len(t, logs.FilterMessage("cascade_killed_descendant").All(), 2, "one WARN per killed unfinished descendant")
}

// Cascade kill removes a background child's schedules just as direct kill removes root schedules.
func TestCascadeKill_RemovesChildSchedules(t *testing.T) {
	release := make(chan struct{})
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			<-release // hang until kill cancels the loop ctx

			return textReply("unreached")
		}
		return textReply("idle")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()
	ctx := h.ctx
	root := h.createRoot(nil)
	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })
	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = h.schedules.AddSchedule(ctx, child.ChildID, "", &oneShot, "child one-shot", false)
	require.NoError(t, err)
	_, err = h.schedules.AddSchedule(ctx, child.ChildID, "0 9 * * *", nil, "child cron", false)
	require.NoError(t, err)
	require.NoError(t, h.mgr.sendToSession(ctx, root, "/kill"))
	h.waitUntil("child gone", func() bool { return !h.mgr.HasActiveLoop(child.ChildID) })
	remaining, err := h.schedules.ListSchedules(ctx, child.ChildID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cascade-killed child's schedules are removed")
}

func TestCascadeKill_KilledTreeSuppressesTerminalBackgroundCompletion(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	var err error
	parent := h.createRoot(nil)
	childID := h.createChild(parent, subagent.Link{TaskCallID: "background"})
	require.NoError(
		t, seedTerminalChild(h.ctx, h.store, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted),
	)
	require.NoError(
		t,
		h.store.WithTx(
			h.ctx,
			func(tx *sql.Tx) error { return sessionstore.MarkSessionKilledTx(h.ctx, tx, parent) },
		),
	)
	h.mgr.killDescendants(h.ctx, parent, 0)
	link := h.link(childID)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, "done", link.Result)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Positive(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredInputID)
	assert.Zero(t, link.DeliveredMsgID)
	_, err = h.store.PeekPending(h.ctx, parent)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

// An in-flight stop commits its fence/start, settles the call and releases the root through one terminal completion.
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
		return textReply("never reached")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		close(release)
		h.shutdown()
	}()

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")
	service := installScenarioProcessService(t, h)
	process := startScenarioProcess(t, service, root, root, "sleep 30")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.store.GetSession(h.ctx, root)
		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})
	stoppedProcess := func() backgroundprocess.Process {
		var record backgroundprocess.Process
		h.waitUntil("process state", func() bool {
			var err error
			record, err = h.mgr.processStore.GetProcess(context.Background(), process.ID)
			return err == nil && record.State == backgroundprocess.StateCancelled
		})
		return record
	}()
	require.Equal(t, backgroundprocess.IntentSessionStopped, stoppedProcess.HostIntent)
	controller := newChainController(t, h)
	drainScenarioClaims(t, "stop_live_chain.json", controller)
	collector.waitFor(t, "stopped readiness", func(events []controllerapi.SessionNotification) bool {
		return containsStateWithReason(events, root, sessionevent.StateIdle, "stopped")
	})
	assertHarnessTrace(t, "stop_live_chain.json", collector.snapshot(), root)
}

// Restart finishes a committed stop fence and terminal completion without executing more model or tool work.
func TestHarnessScenario_InterruptedStopChain(t *testing.T) {
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		return textReply("must not run")
	}
	dbPath := filepath.Join(t.TempDir(), "interrupted-stop.db")
	h1 := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond})
	root := h1.createRoot(managerAttrs(scenarioManagerID))
	require.NoError(t, h1.store.BindManager(h1.ctx, scenarioManagerID, "telegram", map[string]any{
		"bot_user_id": int64(1), "chat_id": int64(2), "topology": "group",
	}))
	input, err := h1.store.Enqueue(
		h1.ctx, sessionstore.Input{SessionID: root, Source: sessionstore.InputSourceUser, Content: "/stop"},
	)
	require.NoError(t, err)
	_, err = h1.store.BeginLifecycleInput(h1.ctx, input.Input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	h1.shutdown()
	var modelCalls int
	respond2 := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls++
		return textReply("must not run")
	}
	h2 := newHarness(t, harnessOptions{dbPath: dbPath, respond: respond2})
	require.NoError(t, h2.mgr.Start(h2.ctx))
	record := h2.session(root)
	require.Equal(t, sessionstore.SessionStatusStopped, record.Status)
	require.Zero(t, modelCalls, "stop recovery must never run the model")
	collector := collectEvents(t, h2.mgr.bus.SubscribeAll())
	defer collector.stop()
	controller := newChainController(t, h2)
	drainScenarioClaims(t, "stop_interrupted_chain.json", controller)
	collector.waitFor(t, "stopped readiness", func(events []controllerapi.SessionNotification) bool {
		return containsStateWithReason(events, root, sessionevent.StateIdle, "stopped")
	})
	assertHarnessTrace(t, "stop_interrupted_chain.json", collector.snapshot(), root)
}

// A later ordinary input on a stopped root is a fresh generation on preserved
// history: the pre-stop progress chain is never edited again.
func TestHarnessScenario_LaterFreshTurnAfterStop(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "continue please") {
			return textReply("Resumed and done.")
		}
		if hasToolResultFor(msgs, "ls") {
			return textReply("Working on it, done for now.")
		}
		return &llmwire.Response{
			Text:      "Working on it",
			ToolCalls: []llmwire.ToolCall{{ID: "fresh-ls", Name: "ls", Arguments: []byte(`{"path":"."}`)}},
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitMessage(root, "Working on it, done for now.")
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.store.GetSession(h.ctx, root)
		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "continue please"))
	collector.waitMessage(root, "Resumed and done.")
	controller := newChainController(t, h)
	drainScenarioClaims(t, "stop_then_fresh_turn.json", controller)
	collector.waitIdleAfter(root, "Resumed and done.")
	assertHarnessTrace(t, "stop_then_fresh_turn.json", collector.snapshot(), root)
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
