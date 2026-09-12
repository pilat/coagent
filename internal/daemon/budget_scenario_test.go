package daemon

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_BudgetMutationRequiresAndConsumesUserGrant(t *testing.T) {
	release := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "set_budget") {
			<-release

			return &llmwire.Response{Text: "budget configured"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "budget-call", Name: "set_budget",
			Arguments: []byte(`{"action":"set","duration":"1m"}`),
		}}}
	}
	h := newSubagentHarnessWith(t, respond)
	defer func() {
		close(release)
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/budget stop after one minute", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	h.waitUntil("budget is armed", func() bool {
		record, loadErr := h.sessStore.GetBudget(h.ctx, sessionID)

		return loadErr == nil && record.State == sessionstore.BudgetArmed
	})

	budgetRecord, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetArmed, budgetRecord.State)
	require.NotNil(t, budgetRecord.DurationSeconds)
	assert.Equal(t, int64(60), *budgetRecord.DurationSeconds)

	activation, err := h.sessStore.PendingActivation(h.ctx, sessionID)
	require.ErrorIs(t, err, sessionstore.ErrActivationNotFound)
	assert.Nil(t, activation)

	var receipts int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND content LIKE 'Budget armed:%'`, sessionID).Scan(&receipts))
	assert.Equal(t, 1, receipts)
}

func TestHarnessScenario_BackgroundChildRetainsBudgetUntilCompletion(t *testing.T) {
	childRelease := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BUDGET_CHILD") {
			<-childRelease
			return &llmwire.Response{Text: "budget child complete"}
		}
		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "budget completion handled"}
		}
		if hasToolResultFor(messages, "task") {
			return &llmwire.Response{Text: "budget child still running"}
		}
		if hasToolResultFor(messages, "set_budget") {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
				{
					ID:   "budget-child-call",
					Name: "task",
					Arguments: []byte(
						`{"prompt":"BUDGET_CHILD","description":"budget child","subagent_type":"general","background":true}`,
					),
				},
			}}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "budget-arm", Name: "set_budget", Arguments: []byte(`{"action":"set","duration":"1m"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/budget run a background child", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "budget child still running")
	record, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetArmed, record.State)
	generation := record.Generation

	close(childRelease)
	waitForVisibleMessage(t, collector, sessionID, "budget completion handled")
	h.waitUntil("budget released after completion", func() bool {
		current, loadErr := h.sessStore.GetBudget(h.ctx, sessionID)
		return loadErr == nil && current.State == sessionstore.BudgetReleased
	})
	record, err = h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, generation, record.Generation)
}

func TestHarnessScenario_AgentInputCannotActivateBudget(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()
	record, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	input, err := h.sessStore.EnqueueInput(h.ctx, record.ID, sessionstore.InputSourceAgent, "/budget 1m")
	require.NoError(t, err)

	_, _, err = h.sessStore.PromoteInputWithActivation(h.ctx, input.ID, "/budget 1m",
		sessionstore.ActivationDraft{ToolID: "set_budget", Command: "/budget"})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)
}

func TestHarnessScenario_FinalIncludesNonEmptyTodoAndBudget(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		close(entered)
		<-release

		return &llmwire.Response{Text: "task answer"}
	})
	defer func() {
		releaseModel()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "do work", "fake-model", map[string]any{
		"manager_id": "telegram:main",
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "model call")

	todos := json.RawMessage(`[{"id":"todo-1","content":"ship change","status":"in_progress","priority":"high"}]`)
	require.NoError(t, h.sessStore.UpdateSessionTodoItems(h.ctx, sessionID, todos))
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)

	releaseModel()
	// The compact progress footer now leads the trailing block: model/iteration,
	// metrics, TODO counts with the hint on its own line, then the budget line.
	want := "task answer\n\n" +
		"🤖 `fake-model` · iteration 1\n" +
		"⌚ 0s · 💰 $0.0 total\n" +
		"📋 TODO · 1 active · 1 remaining · 0 done\n" +
		"ℹ️ /status shows the full TODO list\n" +
		"💸 Budget: armed (generation 1) · $0.000000 / $1.000000 · $1.000000 remaining"
	h.mgr.waitIdle(sessionID)

	var final string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'message:%:final' ORDER BY id DESC LIMIT 1`, sessionID).
		Scan(&final))
	assert.Equal(t, want, final)
}
