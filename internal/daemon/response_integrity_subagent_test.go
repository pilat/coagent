package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestResponseIntegrity_BudgetCrossingSuppressesRecoveryAndCallStubs(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		once.Do(func() { close(entered) })
		<-release

		return &llmwire.Response{
			FinishType: llmwire.FinishLength, CostUSD: 0.5,
			ToolCalls: []llmwire.ToolCall{{ID: "rejected", Name: tool.IDTask, Arguments: []byte(`{}`)}},
		}
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "cross the budget", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForScenarioSignal(t, entered, "budgeted model call")
	_, err = h.db.ExecContext(h.ctx, `INSERT INTO session_budgets
		(root_session_id, state, generation, armed_at, baseline_cost_usd, cost_limit_usd)
		VALUES (?, 'armed', 1, ?, 0, 0.1)`, sessionID, time.Now().UTC())
	require.NoError(t, err)
	close(release)

	h.waitUntil("budget park completes", func() bool {
		record, loadErr := h.sessStore.GetBudget(h.ctx, sessionID)
		return loadErr == nil && record.State == sessionstore.BudgetFired && record.ParkPhase == "parked"
	})

	var recoveryRows, toolRows, rejectedRows int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		COUNT(*) FILTER (WHERE retry_of_message_id IS NOT NULL),
		COUNT(*) FILTER (WHERE role = 'tool'),
		COUNT(*) FILTER (WHERE rejected_reason = 'output_length')
		FROM messages WHERE session_id = ?`, sessionID).Scan(&recoveryRows, &toolRows, &rejectedRows))
	assert.Zero(t, recoveryRows)
	assert.Zero(t, toolRows)
	assert.Equal(t, 1, rejectedRows)
}

func TestResponseIntegrity_ReusedChildReportsCurrentErrorInsteadOfPriorAnswer(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_INTEGRITY") {
			if hasUserContaining(messages, "FAIL_CURRENT_ROUND") {
				return &llmwire.Response{Text: "rejected child partial", FinishType: llmwire.FinishUnknown}
			}
			return &llmwire.Response{Text: "prior child answer"}
		}
		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent consumed child outcome"}
		}

		return taskResponse("CHILD_INTEGRITY", "integrity")
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start integrity child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "FAIL_CURRENT_ROUND"))
	h.waitUntil("integrity-error continuation delivered", func() bool {
		current, linkErr := h.links.GetLink(h.ctx, link.ChildID)
		return linkErr == nil && current != nil && current.Terminal() && current.DeliveredAt != 0 &&
			current.ActivationSeq == 2
	})
	current, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeError, current.Outcome)
	assert.Equal(t, sessionstore.UnknownFinishTerminalError, current.Result)
	assert.NotContains(t, current.Result, "prior child answer")
}

func TestResponseIntegrity_ChildEmptyFinishCannotLeakText(t *testing.T) {
	runIncompleteChildResponse(t, &llmwire.Response{
		Text: "hidden child text", FinishType: llmwire.FinishToolCalls,
	})
}

func TestResponseIntegrity_ChildWhitespaceStopCannotBecomeCompletion(t *testing.T) {
	runIncompleteChildResponse(t, &llmwire.Response{Text: " \n\t ", FinishType: llmwire.FinishStop})
}

func runIncompleteChildResponse(t *testing.T, childResponse *llmwire.Response) {
	t.Helper()
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_EMPTY_TOOL_FINISH") {
			return childResponse
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{Text: "parent handled incomplete child"}
		}

		return taskResponse("CHILD_EMPTY_TOOL_FINISH", "empty finish")
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start empty-finish child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)
	current, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeIncomplete, current.Outcome)
	assert.NotContains(t, current.Result, childResponse.Text)
}

func TestResponseIntegrity_MissingTerminalRejectionNeverReusesOlderAnswer(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()
	parent, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, parent.ID, parent.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	require.NoError(t, h.links.InsertSubagentLink(h.ctx, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "missing-rejection",
	}))
	_, err = h.sessStore.InsertMessage(h.ctx, childID, &transcript.Message{
		Role: llmwire.RoleAssistant, Content: "older accepted answer", FinishType: llmwire.FinishStop,
	})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionIteration(h.ctx, childID, 2, sessionstore.SessionStatusError))

	h.mgr.finalizeChild(h.ctx, childID)
	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeError, link.Outcome)
	assert.NotContains(t, link.Result, "older accepted answer")
	assert.Contains(t, link.Result, "crashed after 2 iterations")
}

func taskResponse(prompt, description string) *llmwire.Response {
	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID: taskCallID, Name: tool.IDTask,
		Arguments: []byte(`{"prompt":"` + prompt + `","description":"` + description +
			`","subagent_type":"general"}`),
	}}}
}
