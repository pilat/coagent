package daemon

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_BudgetGrantDoesNotOutliveItsProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "budget.db")
	entered := make(chan struct{})
	release := make(chan struct{})
	var enterOnce sync.Once
	first := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
			enterOnce.Do(func() { close(entered) })
			<-release
			return textReply("discarded before restart")
		}},
	)
	first.startInboxWake()
	root, err := first.mgr.Send(
		first.ctx,
		first.projectID,
		"/budget stop after one minute",
		"fake-model",
		managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "budget model call before restart")
	second := newHarness(
		t,
		harnessOptions{dbPath: dbPath, respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, "set_budget") {
				return textReply("budget refused")
			}
			return callReply("budget-after-crash", "set_budget", `{"action":"set","duration":"1m"}`)
		}},
	)
	defer func() {
		second.shutdown()
		first.shutdown()
		close(release)
	}()
	second.startInboxWake()
	second.mgr.resumeAfterRestart(second.ctx)
	second.waitUntil("budget result after restart", func() bool {
		return hasToolResultFor(second.messages(root), "set_budget") && !second.mgr.HasActiveLoop(root)
	})
	assert.Contains(t, lastToolResultContent(second.messages(root), "set_budget"),
		"This change requires a current user message beginning with /budget.")
	_, err = second.store.Get(second.ctx, root)
	require.ErrorIs(t, err, budget.ErrNotFound)
	for _, row := range second.outbox(root) {
		assert.NotContains(t, row.Content, "Budget armed")
	}
}

func TestHarnessScenario_BudgetMutationRequiresAndConsumesUserGrant(t *testing.T) {
	release := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "set_budget") {
			<-release
			return textReply("budget configured")
		}
		return callReply("budget-call", "set_budget", `{"action":"set","duration":"1m"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		close(release)
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(
		h.ctx, h.projectID, "/budget stop after one minute", "fake-model", managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	h.waitUntil("budget is armed", func() bool {
		record, loadErr := h.store.Get(h.ctx, sessionID)
		return loadErr == nil && record.State == budget.Armed
	})
	budgetRecord, err := h.store.Get(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, budgetRecord.State)
	require.NotNil(t, budgetRecord.DurationSeconds)
	assert.Equal(t, int64(60), *budgetRecord.DurationSeconds)
	var receipts int
	for _, row := range h.outbox(sessionID) {
		if strings.HasPrefix(strings.ToLower(row.Content), "budget armed:") {
			receipts++
		}
	}
	assert.Equal(t, 1, receipts)
}

func TestHarnessScenario_UnusedBudgetGrantSaysNothingChanged(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("no budget change")
	}})
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx,
		h.projectID,
		"/budget stop after one minute",
		"fake-model",
		managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	h.waitUntil("unused grant released", func() bool {
		return len(h.outbox(root)) > 0 && !h.mgr.HasActiveLoop(root)
	})
	var notices int
	for _, row := range h.outbox(root) {
		if row.Content == "/budget was not changed" {
			notices++
		}
	}
	assert.Equal(t, 1, notices)
	_, err = h.store.Get(h.ctx, root)
	require.ErrorIs(t, err, budget.ErrNotFound)
	pending, err := h.store.ListPending(h.ctx, root)
	require.NoError(t, err)
	assert.Empty(t, pending)
}

func TestHarnessScenario_InputWaitsWhileAGrantIsOpen(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		once.Do(func() {
			close(entered)
			<-release
		})
		if hasUserContaining(messages, "hello") {
			return textReply("hi back")
		}
		return textReply("no budget change")
	}})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx,
		h.projectID,
		"/budget stop after one minute",
		"fake-model",
		managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "command model call")
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "hello"))
	closeOnce(release)
	h.waitUntil("followup answered", func() bool {
		for _, row := range h.outbox(root) {
			if row.Content == "hi back" {
				return !h.mgr.HasActiveLoop(root)
			}
		}
		return false
	})
	ids := make(map[string]int64)
	for _, row := range h.outbox(root) {
		ids[row.Content] = row.ID
	}
	require.Positive(t, ids["no budget change"])
	assert.Less(t, ids["no budget change"], ids["/budget was not changed"])
	assert.Less(t, ids["/budget was not changed"], ids["hi back"])
}

func TestHarnessScenario_BudgetGrantIsSpentByTheFirstReceipt(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		switch countToolResultsFor(messages, "set_budget") {
		case 0:
			return callReply("budget-1", "set_budget", `{"action":"set","duration":"1m"}`)
		case 1:
			return callReply("budget-2", "set_budget", `{"action":"set","duration":"2m"}`)
		default:
			return textReply("budget configured")
		}
	}})
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx,
		h.projectID,
		"/budget stop after one minute",
		"fake-model",
		managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	h.waitUntil("two budget calls answered", func() bool {
		return countToolResultsFor(h.messages(root), "set_budget") == 2 && !h.mgr.HasActiveLoop(root)
	})
	assert.Contains(t, lastToolResultContent(h.messages(root), "set_budget"),
		"This change requires a current user message beginning with /budget.")
	var receipts int
	for _, row := range h.outbox(root) {
		if strings.HasPrefix(row.Content, "Budget armed") {
			receipts++
		}
	}
	assert.Equal(t, 1, receipts)
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	require.NotNil(t, record.DurationSeconds)
	assert.Equal(t, int64(60), *record.DurationSeconds)
}

func TestHarnessScenario_BackgroundChildRetainsBudgetUntilCompletion(t *testing.T) {
	childRelease := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BUDGET_CHILD") {
			<-childRelease
			return textReply("budget child complete")
		}
		if hasUserContaining(messages, "<subagent_completion>") {
			return textReply("budget completion handled")
		}
		if hasToolResultFor(messages, "task") {
			return textReply("budget child still running")
		}
		if hasToolResultFor(messages, "set_budget") {
			return callReply(
				"budget-child-call", "task",
				`{"prompt":"BUDGET_CHILD","description":"budget child","subagent_type":"general","background":true}`,
			)
		}
		return callReply("budget-arm", "set_budget", `{"action":"set","duration":"1m"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(
		h.ctx, h.projectID, "/budget run a background child", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	collector.waitMessage(sessionID, "budget child still running")
	record, err := h.store.Get(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	generation := record.Generation
	close(childRelease)
	collector.waitMessage(sessionID, "budget completion handled")
	h.waitUntil("budget released after completion", func() bool {
		current, loadErr := h.store.Get(h.ctx, sessionID)
		return loadErr == nil && current.State == budget.Released
	})
	record, err = h.store.Get(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, generation, record.Generation)
}

func TestHarnessScenario_FinalOmitsTodoAndBudget(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		enterOnce.Do(func() { close(entered) })
		<-release
		return textReply("task answer")
	}})
	defer func() {
		releaseModel()
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "do work", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")
	todos := json.RawMessage(`[{"id":"todo-1","content":"ship change","status":"in_progress","priority":"high"}]`)
	require.NoError(t, func() error {
		raw := todos
		_, err := h.store.Commit(
			h.ctx, sessionstore.Commit{SessionID: sessionID, State: sessionstore.StatePatch{TodoItems: &raw}},
		)
		return err
	}())
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)
	releaseModel()
	want := "task answer"
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	var finals []string
	for _, row := range h.outbox(sessionID) {
		if strings.HasPrefix(row.SourceKey, "message:") && strings.HasSuffix(row.SourceKey, ":final") {
			finals = append(finals, row.Content)
		}
	}
	require.Len(t, finals, 1, "the manager sees exactly the confirmed answer")
	assert.Equal(t, want, finals[0])
}

// A hidden candidate that crosses budget cannot publish or request confirmation; only the host checkpoint appears.
func TestHarnessScenario_CompletionCheckBudgetCrossingOnCandidateHidesText(t *testing.T) {
	var calls atomic.Int64
	budgetArmed := make(chan struct{})
	respond := func(string, []llmwire.Message) *llmwire.Response {
		n := calls.Add(1)
		if n == 1 {
			// The budget row must exist before the first disposition commit;
			// admission has already passed with a zero tree cost.
			<-budgetArmed
		}

		// The cost lands with the candidate message, so the crossing is
		// observed by the disposition transaction, not by an earlier admission.
		return &llmwire.Response{Text: "unconfirmed candidate under budget", CostUSD: 0.01}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx, h.projectID, "do work under a tight budget", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)

	// The limit sits below the candidate's cost: admission sees a zero tree,
	// and only the disposition commit crosses it.
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.000001)`, root, time.Now().UTC())
	require.NoError(t, err)
	close(budgetArmed)

	// The checkpoint publishes through the durable outbox; the park scheduler
	// owns its delivery, so assert the committed row rather than a push event.
	h.waitUntil("budget checkpoint committed", func() bool {
		var checkpoints int
		for _, row := range h.outbox(root) {
			if strings.HasPrefix(strings.ToLower(row.Content), "budget checkpoint reached") {
				checkpoints++
			}
		}
		return checkpoints == 1
	})
	assert.Equal(t, int64(1), calls.Load(),
		"the crossing fires on the candidate disposition, before any confirmation call")
	var leaks int
	for _, row := range h.outbox(root) {
		if strings.Contains(strings.ToLower(row.Content), "unconfirmed candidate under budget") {
			leaks++
		}
	}
	assert.Zero(t, leaks, "the unconfirmed candidate text never publishes")
	collector.waitFor(
		t,
		"budget park becomes idle without output acknowledgement",
		func(events []controllerapi.SessionNotification) bool {
			return hasStateEvent(events, root, controllerapi.StateIdle)
		},
	)
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(root).Status)
}

func TestSendToSessionDuringBudgetDrainExplainsParking(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return &llmwire.Response{Text: "over budget", CostUSD: 0.5}
		}
		return textReply("continued")
	}})
	t.Cleanup(func() { closeOnce(release); h.shutdown() })
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "work under a budget", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "budgeted model call")
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.1)`, root, time.Now().UTC())
	require.NoError(t, err)
	unlockTree, err := h.mgr.lockSessionTree(h.ctx, root)
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(unlockTree) }
	t.Cleanup(unlock)
	require.NoError(t, err)
	close(release)
	h.waitUntil("park in progress", func() bool { return h.mgr.parks.active(root) })
	err = h.mgr.sendToSession(h.ctx, root, "resume the work")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "budget conflict", "the raw store conflict must not reach the user")
	assert.Contains(t, err.Error(), "park", "the error must explain the parking state, got: %s", err.Error())
	_, pendingErr := h.store.PeekPending(h.ctx, root)
	require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	unlock()
	h.waitUntil("park stopped", func() bool {
		return !h.mgr.parks.active(root) && h.session(root).Status == sessionstore.SessionStatusStopped
	})
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "resume the work"))
	record, err = h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Released, record.State)
	assert.Equal(t, "resumed", record.ReleasedReason)
	h.waitUntil("continued answer", func() bool {
		for _, row := range h.outbox(root) {
			if row.Content == "continued" {
				return !h.mgr.HasActiveLoop(root)
			}
		}
		return false
	})
	require.NoError(t, llmtest.ValidateToolPairing(h.messages(root)))
}

func TestHarnessScenario_BudgetChangeDuringParkRearmsAndSkipsThePark(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return &llmwire.Response{Text: "over budget", CostUSD: 0.5}
		}
		return textReply("continued")
	}})
	t.Cleanup(func() { closeOnce(release); h.shutdown() })
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "work under a budget", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "budgeted model call")
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.1)`, root, time.Now().UTC())
	require.NoError(t, err)
	unlockTree, err := h.mgr.lockSessionTree(h.ctx, root)
	var unlockOnce sync.Once
	unlock := func() { unlockOnce.Do(unlockTree) }
	t.Cleanup(unlock)
	require.NoError(t, err)
	close(release)
	h.waitUntil("budget fired", func() bool {
		record, loadErr := h.store.Get(h.ctx, root)
		return loadErr == nil && record.State == budget.Fired
	})
	h.waitUntil("park in progress", func() bool { return h.mgr.parks.active(root) })
	ten := 10.0
	rearmed, err := h.store.Arm(h.ctx, budget.Mutation{
		RootSessionID: root, ToolCallID: "rearm-during-park", CostLimitUSD: &ten, Receipt: "Budget armed during park",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(2), rearmed.Generation)
	assert.Equal(t, budget.Armed, rearmed.State)
	unlock()
	h.waitUntil("run idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	assert.Never(t,
		func() bool { return h.session(root).Status == sessionstore.SessionStatusStopped },
		500*time.Millisecond, 25*time.Millisecond,
	)
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	assert.Equal(t, int64(2), record.Generation)
	var receipts int
	for _, row := range h.outbox(root) {
		if row.Content == "Budget armed during park" {
			receipts++
		}
	}
	assert.Equal(t, 1, receipts)
	h.waitUntil("continue accepted", func() bool { return h.mgr.sendToSession(h.ctx, root, "continue") == nil })
	h.waitUntil("continued answer", func() bool {
		for _, row := range h.outbox(root) {
			if row.Content == "continued" {
				return !h.mgr.HasActiveLoop(root)
			}
		}
		return false
	})
	record, err = h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Released, record.State)
	assert.Equal(t, "completed", record.ReleasedReason)
	assert.Equal(t, int64(2), record.Generation)
	require.NoError(t, llmtest.ValidateToolPairing(h.messages(root)))
}

func TestResponseIntegrity_BudgetCrossingSuppressesRecoveryAndCallStubs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		once.Do(func() { close(entered) })
		<-release
		return &llmwire.Response{
			FinishType: llmwire.FinishLength, CostUSD: 0.5,
			ToolCalls: []llmwire.ToolCall{{ID: "rejected", Name: tool.IDTask, Arguments: []byte(`{}`)}},
		}
	}})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		h.shutdown()
	}()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "cross the budget", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "budgeted model call")
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)
	close(release)
	h.waitUntil("budget park completes", func() bool {
		record, loadErr := h.store.Get(h.ctx, sessionID)
		return loadErr == nil && record.State == budget.Fired &&
			h.session(sessionID).Status == sessionstore.SessionStatusStopped
	})
	var recoveryRows, toolRows, rejectedRows int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		COUNT(*) FILTER (WHERE role = 'user' AND content = ?),
		COUNT(*) FILTER (WHERE role = 'tool'),
		COUNT(*) FILTER (WHERE rejected_reason = 'output_length')
		FROM messages WHERE session_id = ?`, sessionstore.OutputLengthRecoveryPrompt, sessionID).Scan(
		&recoveryRows, &toolRows, &rejectedRows,
	))
	assert.Zero(t, recoveryRows)
	assert.Zero(t, toolRows)
	assert.Equal(t, 1, rejectedRows)
}

func TestBudgetParkStopsACompletedRoot(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	root := h.createRoot(managerAttrs(scenarioManagerID))
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, root, sessionstore.SessionStatusCompleted))
	limit := 1.0
	armed, err := h.store.Arm(h.ctx, budget.Mutation{
		RootSessionID: root, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	_, _, err = h.store.FireBudget(h.ctx, root, armed.Generation, "cost", 1.5, "checkpoint")
	require.NoError(t, err)
	h.mgr.startPark(root)
	h.waitUntil("completed root stopped", func() bool {
		return h.session(root).Status == sessionstore.SessionStatusStopped && !h.mgr.parks.active(root)
	})
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
}
