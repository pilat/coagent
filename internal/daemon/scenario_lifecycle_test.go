package daemon

import (
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

// TestCascadeKill_BackgroundDescendant: killing a parent stops every non-terminal
// descendant — blocking AND background — across the depth bound, and each killed
// unfinished descendant produces exactly one WARN audit line.
func TestCascadeKill_BackgroundDescendant(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 2)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			entered <- struct{}{}
			<-release // hang until kill cancels the loop ctx

			return &llmwire.Response{Text: "unreached"}
		}

		return &llmwire.Response{Text: "idle"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	ctx := h.ctx

	root, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })
	h.waitUntil("background child entered model call", func() bool { return len(entered) >= 1 })

	h.startInboxWake()
	grandchild, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: child.ChildID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background grandchild running", func() bool { return h.mgr.HasActiveLoop(grandchild.ChildID) })
	h.waitUntil("background grandchild entered model call", func() bool { return len(entered) >= 2 })
	childInput, err := h.sessStore.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  child.ChildID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "pending",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	grandchildInput, err := h.sessStore.Enqueue(
		ctx,
		sessionstore.Input{
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

	require.NoError(t, h.mgr.sendToSession(killCtx, root.ID, "/kill"))

	h.waitUntil("both descendants gone", func() bool {
		return !h.mgr.HasActiveLoop(child.ChildID) && !h.mgr.HasActiveLoop(grandchild.ChildID)
	})

	for _, id := range []int64{child.ChildID, grandchild.ChildID} {
		rec, gerr := h.sessStore.GetSession(ctx, id)
		require.NoError(t, gerr)
		assert.NotNil(t, rec.KilledAt, "descendant %d is killed with its tree", id)
	}
	for _, inputID := range []int64{childInput.Input.ID, grandchildInput.Input.ID} {
		var state string
		require.NoError(t, h.db.QueryRowContext(ctx,
			`SELECT state FROM session_inbox WHERE id = ?`, inputID,
		).Scan(&state))
		assert.Equal(t, string(sessionstore.InputStateCancelled), state)
	}

	assert.Len(t, logs.FilterMessage("cascade_killed_descendant").All(), 2,
		"one WARN per killed unfinished descendant")
}

// TestCascadeKill_RemovesChildSchedules: cascade-killing a background child
// deletes its schedules too — killSubagent owns schedule teardown, same as the
// direct Kill path.
func TestCascadeKill_RemovesChildSchedules(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "HANG") {
			<-release // hang until kill cancels the loop ctx

			return &llmwire.Response{Text: "unreached"}
		}

		return &llmwire.Response{Text: "idle"}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	ctx := h.ctx

	root, err := h.sessStore.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	h.startInboxWake()
	child, err := h.mgr.Spawn(ctx, subagent.SpawnRequest{
		ParentID: root.ID, AgentType: "general", Prompt: "HANG", Blocking: false,
	})
	require.NoError(t, err)
	h.waitUntil("background child running", func() bool { return h.mgr.HasActiveLoop(child.ChildID) })

	oneShot := time.Now().Add(time.Hour).UTC()
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "", &oneShot, "child one-shot", false)
	require.NoError(t, err)
	_, err = h.schedStore.AddSchedule(ctx, child.ChildID, "0 9 * * *", nil, "child cron", false)
	require.NoError(t, err)

	require.NoError(t, h.mgr.sendToSession(ctx, root.ID, "/kill"))
	h.waitUntil("child gone", func() bool { return !h.mgr.HasActiveLoop(child.ChildID) })

	remaining, err := h.schedStore.ListSchedules(ctx, child.ChildID)
	require.NoError(t, err)
	assert.Empty(t, remaining, "cascade-killed child's schedules are removed")
}

func TestCascadeKill_KilledTreeSuppressesTerminalBackgroundCompletion(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(h.ctx, h.sessStore, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "background",
	}))
	require.NoError(
		t,
		seedTerminalChild(h.ctx, h.sessStore, childID, subagent.StateCompleted, "done", subagent.OutcomeCompleted),
	)
	require.NoError(
		t,
		h.sessStore.WithTx(
			h.ctx,
			func(tx *sql.Tx) error { return sessionstore.MarkSessionKilledTx(h.ctx, tx, parent.ID) },
		),
	)

	h.mgr.killDescendants(h.ctx, parent.ID, 0)

	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.StateCompleted, link.State)
	assert.Equal(t, "done", link.Result)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Positive(t, link.DeliveredAt)
	assert.Zero(t, link.DeliveredInputID)
	assert.Zero(t, link.DeliveredMsgID)
	_, err = h.sessStore.PeekPending(h.ctx, parent.ID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
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

	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer collector.stop()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")
	service := installScenarioProcessService(t, h)
	process := startScenarioProcess(t, service, root, root, "sleep 30")

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})
	stoppedProcess := waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCancelled)
	require.Equal(t, backgroundprocess.IntentSessionStopped, stoppedProcess.HostIntent)

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
	input, err := h1.sessStore.Enqueue(
		h1.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/stop"},
	)
	require.NoError(t, err)
	_, err = h1.sessStore.BeginLifecycleInput(h1.ctx, input.Input.ID, "stop", "⏳ Stopping…")
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

	collector := collectEvents(h2.mgr.bus.SubscribeAll())
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

	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer collector.stop()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "long work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, root, "Working on it, done for now.")
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/stop"))
	h.waitUntil("root stopped", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, root)

		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "continue please"))
	waitForVisibleMessage(t, collector, root, "Resumed and done.")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "stop_then_fresh_turn.json", controller)
	waitForIdleAfterMessage(t, collector, root, "Resumed and done.")

	assertHarnessTrace(t, "stop_then_fresh_turn.json", collector.snapshot(), root)
}
