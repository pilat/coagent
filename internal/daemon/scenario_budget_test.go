package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

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
	activation, err := h.store.PendingActivation(h.ctx, sessionID)
	require.ErrorIs(t, err, sessionstore.ErrActivationNotFound)
	assert.Nil(t, activation)
	var receipts int
	for _, row := range h.outbox(sessionID) {
		if strings.HasPrefix(strings.ToLower(row.Content), "budget armed:") {
			receipts++
		}
	}
	assert.Equal(t, 1, receipts)
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

func TestHarnessScenario_AgentInputCannotActivateBudget(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	record := h.createRoot(managerAttrs("telegram:main"))
	input, err := h.store.Enqueue(
		h.ctx, sessionstore.Input{SessionID: record, Source: sessionstore.InputSourceAgent, Content: "/budget 1m"},
	)
	require.NoError(t, err)
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{
		SessionID: record,
		Accept: []sessionstore.Accept{
			{InputID: input.Input.ID, State: sessionstore.InputStateAccepted, Content: "/budget 1m", LinkRef: -1},
		},
		Activation: &sessionstore.ActivationChange{InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget"},
	})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)
}

func TestHarnessScenario_FinalIncludesNonEmptyTodoAndBudget(t *testing.T) {
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
	// Confirmation is the second model call; the compact footer preserves model, metrics, TODO and budget ordering.
	want := "task answer\n\n" +
		"🤖 `fake-model` · iteration 2\n" +
		"📋 TODO · 1 active · 1 remaining · 0 done\n" +
		"ℹ️ /status shows the full TODO list\n" +
		"💸 Budget: armed (generation 1) · $0.000000 / $1.000000 · $1.000000 remaining"
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
	record, err := h.store.Get(h.ctx, root)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
}

// A user turn racing the park drain must be rejected with an actionable
// explanation, not the raw store conflict — and leave nothing in the inbox.
func TestSendToSessionDuringBudgetDrainExplainsParking(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	sessions, store := h.store, h.store
	projectID := testProject(t, store, "/tmp/park-race")
	root, err := sessions.CreateSession(ctx, projectID, "priced", "", managerAttrs("manager-park"))
	require.NoError(t, err)
	input, err := sessions.Enqueue(
		ctx, sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceUser, Content: "/budget"},
	)
	require.NoError(t, err)
	_, err = sessions.Commit(ctx, sessionstore.Commit{
		SessionID: root.ID,
		Accept: []sessionstore.Accept{
			{
				InputID: input.Input.ID,
				State:   sessionstore.InputStateAccepted,
				Content: "/budget\n\nactivate",
				LinkRef: -1,
			},
		},
		Activation: &sessionstore.ActivationChange{InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget"},
	})
	require.NoError(t, err)
	limit := 1.0
	_, err = sessions.Arm(ctx, budget.Mutation{
		RootSessionID: root.ID, InputID: input.Input.ID, ToolID: "set_budget", Command: "/budget",
		ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "Budget armed",
	})
	require.NoError(t, err)
	fired, _, err := sessions.FireBudget(ctx, root.ID, 1, "cost", 1.5, "Budget checkpoint reached (cost).")
	require.NoError(t, err)
	_, err = sessions.BeginBudgetDrain(ctx, root.ID, fired.Generation, fired.ParkOwner)
	require.NoError(t, err)
	mgr := h.mgr
	err = mgr.sendToSession(ctx, root.ID, "resume the work")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "budget conflict", "the raw store conflict must not reach the user")
	assert.Contains(t, err.Error(), "park", "the error must explain the parking state, got: %s", err.Error())
	_, pendingErr := sessions.PeekPending(ctx, root.ID)
	require.ErrorIs(t, pendingErr, sessionstore.ErrNoPendingInput)
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
		return loadErr == nil && record.State == budget.Fired && record.ParkPhase == "parked"
	})
	var recoveryRows, toolRows, rejectedRows int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		COUNT(*) FILTER (WHERE retry_of_message_id IS NOT NULL),
		COUNT(*) FILTER (WHERE role = 'tool'),
		COUNT(*) FILTER (WHERE rejected_reason = 'output_length')
		FROM messages WHERE session_id = ?`, sessionID).Scan(
		&recoveryRows, &toolRows, &rejectedRows,
	))
	assert.Zero(t, recoveryRows)
	assert.Zero(t, toolRows)
	assert.Equal(t, 1, rejectedRows)
}
