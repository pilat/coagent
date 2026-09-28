package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const budgetCrossingToolID = "budget_crossing_probe"

type budgetCrossingFactory struct {
	session.Factory
	executions *atomic.Int64
}

type budgetCrossingTool struct {
	executions *atomic.Int64
}

func TestScenario_BudgetCrossingSkipsReturnedToolsAndResumesOnUserInput(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var enterOnce, releaseOnce sync.Once
	releaseResponse := func() { releaseOnce.Do(func() { close(release) }) }
	var executions atomic.Int64
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "resume after checkpoint") {
			for _, message := range messages {
				if message.Role == llmwire.RoleTool && message.ToolCallID == "resumed-probe" {
					return &llmwire.Response{Text: "continued after checkpoint"}
				}
			}
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "resumed-probe", Name: budgetCrossingToolID, Arguments: []byte(`{}`),
			}}}
		}
		enterOnce.Do(func() { close(entered) })
		<-release
		return &llmwire.Response{CostUSD: 1.25, ToolCalls: []llmwire.ToolCall{
			{ID: "crossing-probe-a", Name: budgetCrossingToolID, Arguments: []byte(`{}`)},
			{ID: "crossing-probe-b", Name: budgetCrossingToolID, Arguments: []byte(`{}`)},
		}}
	})
	h.mgr.factory = &budgetCrossingFactory{Factory: h.mgr.factory, executions: &executions}
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		releaseResponse()
		h.shutdown()
		collector.stop()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "perform the probe", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "paid model response waiting for budget arm")
	// Arm after model admission so the paid response itself must enforce crossing.
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)
	releaseResponse()
	h.waitUntil("crossing root parked", func() bool {
		budget, loadErr := h.sessStore.GetBudget(h.ctx, sessionID)
		if loadErr != nil || budget.State != sessionstore.BudgetFired || budget.ParkPhase != "parked" {
			return false
		}
		record, loadErr := h.sessStore.GetSession(h.ctx, sessionID)
		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped && !h.mgr.HasActiveLoop(sessionID)
	})

	assert.Zero(t, executions.Load(), "tools returned by the crossing response must never execute")
	for _, callID := range []string{"crossing-probe-a", "crossing-probe-b"} {
		var count int
		var content string
		require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*), COALESCE(MAX(content), '') FROM messages
			WHERE session_id = ? AND role = 'tool' AND tool_call_id = ?`, sessionID, callID).Scan(&count, &content))
		assert.Equal(t, 1, count)
		assert.Equal(t, "Not executed because the budget checkpoint fired.", content)
	}
	_, _, paidCost, err := h.sessStore.GetSessionTreeUsage(h.ctx, sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, paidCost, 0.000001)
	assertBudgetCrossingCheckpoint(t, h, sessionID)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "resume after checkpoint"))
	waitForVisibleMessage(t, collector, sessionID, "continued after checkpoint")
	h.waitUntil("resumed root finished", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	assert.EqualValues(t, 1, executions.Load(), "only the newly requested probe may execute")
	budget, err := h.sessStore.GetBudget(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.BudgetReleased, budget.State)
	assert.Equal(t, "resumed", budget.ReleasedReason)
	assert.EqualValues(t, 1, budget.Generation)
	_, _, paidCost, err = h.sessStore.GetSessionTreeUsage(h.ctx, sessionID)
	require.NoError(t, err)
	assert.InDelta(t, 1.25, paidCost, 0.000001)
	assertBudgetCrossingCheckpoint(t, h, sessionID)
}

func (f *budgetCrossingFactory) Create(ctx context.Context, options session.CreateOptions) (session.Service, error) {
	sess, err := f.Factory.Create(ctx, options)
	if err != nil {
		return nil, err
	}
	if !sess.RegisterGatedTool(&budgetCrossingTool{executions: f.executions}) {
		sess.Close()
		return nil, errors.New("budget crossing probe was not registered")
	}
	return sess, nil
}

func (t *budgetCrossingTool) ID() string          { return budgetCrossingToolID }
func (t *budgetCrossingTool) Description() string { return "Record a probe execution." }
func (t *budgetCrossingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object"}`)
}
func (t *budgetCrossingTool) ParallelSafe() bool { return false }

func (t *budgetCrossingTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	t.executions.Add(1)
	return &tool.Result{Output: "probe executed"}, nil
}

func assertBudgetCrossingCheckpoint(t *testing.T, h *subagentHarness, sessionID int64) {
	t.Helper()
	var count int
	var content, owner string
	var releases int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*), COALESCE(MAX(content), ''),
		COALESCE(MAX(json_extract(attributes, '$.manager_id')), ''), COALESCE(MAX(releases_input), 0)
		FROM session_outbox WHERE session_id = ? AND source_key = 'budget:1:checkpoint'`, sessionID).
		Scan(&count, &content, &owner, &releases))
	assert.Equal(t, 1, count, "crossing and resume must retain exactly one host checkpoint")
	assert.Equal(
		t,
		"Budget checkpoint reached (cost). Persisted cost: $1.250000. The limiter is no longer armed.",
		content,
	)
	assert.Equal(t, scenarioManagerID, owner)
	assert.Equal(t, 1, releases)
}
