package daemon

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

func (h *harness) waitParentAnswer(parentID int64) {
	h.t.Helper()
	h.waitUntil("parent answer", func() bool {
		return lastAssistantTextDTO(h.messages(parentID)) == "parent final" && !h.mgr.HasActiveLoop(parentID)
	})
}

func hasAssistantText(messages []llmwire.Message, text string) bool {
	return slices.ContainsFunc(messages, func(message llmwire.Message) bool {
		return message.Role == llmwire.RoleAssistant && message.Content == text
	})
}

func (h *harness) processInputs(processID string) int {
	h.t.Helper()
	var count int
	require.NoError(h.t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox
		WHERE source = 'process' AND json_extract(attributes, '$.process_id') = ?`, processID).Scan(&count))
	return count
}

// Foreground children are never queued, so a resumed one that finds no capacity
// fails at once and its parent's task call returns instead of hanging.
func TestHarnessScenario_ForegroundYieldFailsWhenCapacityIsFull(t *testing.T) {
	script := newBackgroundChildScript(t)
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	link := h.waitChildYield(parentID)

	held := 0
	for h.mgr.runners.tryAdmit(true, int64(40_000+held)) {
		held++
	}
	defer func() {
		for i := range held {
			h.mgr.runners.release(true, int64(40_000+i))
		}
	}()
	require.Equal(t, maxChildren, held, "every child slot is taken")

	script.releaseSearch(t)
	h.waitParentAnswer(parentID)

	failed := h.link(link.ChildID)
	require.NotNil(t, failed)
	assert.Equal(t, subagent.StateError, failed.State)
	assert.Equal(t, subagent.OutcomeError, failed.Outcome)
	assert.Contains(t, failed.Result, "session capacity is full")
	assert.Contains(t, failed.Result, "waiting for the search", "the child's last answer is kept")
	assert.Equal(t, 1, h.parentDeliveries(parentID, link.ChildID))
	assert.Contains(t, subagentLastToolResultContent(h.messages(parentID), tool.IDTask), "could not resume")
	assert.Equal(t, sessionstore.SessionStatusError, h.session(link.ChildID).Status)
}

// The process exits while the yielding child's runner is still registered; the
// runner-exit restart, not the wake, resumes the same activation.
func TestHarnessScenario_ProcessExitDuringYieldResumesAfterRunnerExit(t *testing.T) {
	script := newBackgroundChildScript(t)
	var h *harness
	script.beforeNudgeReply = func() { script.releaseAndAwaitExit(t, &h) }
	h = newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	h.waitParentAnswer(parentID)
	link := h.linkByCall(parentID, taskCallID)
	require.NotNil(t, link)
	assertOneChildResult(t, h, parentID, link, "final report with search")
	assert.True(t, hasAssistantText(h.messages(link.ChildID), "waiting for the search"), "the child did yield first")
}

// cancel_process loses to a natural exit: the completion still arrives once and
// the child finishes with one result.
func TestHarnessScenario_CancelAfterNaturalExitDeliversOneResult(t *testing.T) {
	script := newBackgroundChildScript(t)
	script.cancel = true
	var h *harness
	script.beforeNudgeReply = func() { script.releaseAndAwaitExit(t, &h) }
	h = newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)

	h.waitParentAnswer(parentID)
	link := h.linkByCall(parentID, taskCallID)
	require.NotNil(t, link)
	assertOneChildResult(t, h, parentID, link, "final report with search")
	id, _ := script.processID.Load().(string)
	process, err := h.mgr.processStore.GetProcess(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateCompleted, process.State)
	assert.Equal(t, 1, h.processInputs(id))
}

func TestHarnessScenario_StopOverYieldedChildSettlesTheTree(t *testing.T) {
	script := newBackgroundChildScript(t)
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	link := h.waitChildYield(parentID)

	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "/stop"))
	h.waitUntil("root stopped", func() bool {
		return h.session(parentID).Status == sessionstore.SessionStatusStopped
	})
	id, _ := script.processID.Load().(string)
	h.waitUntil("process cancelled", func() bool {
		process, err := h.mgr.processStore.GetProcess(h.ctx, id)
		return err == nil && process.State == backgroundprocess.StateCancelled
	})
	assert.Equal(t, subagent.StateStopped, h.link(link.ChildID).State)
	assert.Zero(t, h.processInputs(id), "a stop-caused cancellation wakes nobody")

	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue"))
	h.waitParentAnswer(parentID)
	assert.Equal(t, 1, countToolResultsFor(h.messages(parentID), tool.IDTask), "the task call is settled once")
	assert.Equal(t, subagent.StateStopped, h.link(link.ChildID).State)
	assert.Zero(t, h.parentDeliveries(parentID, link.ChildID), "a stopped child delivers no result")
}

func TestHarnessScenario_BudgetParkOverYieldedChildKeepsItsProcess(t *testing.T) {
	script := newBackgroundChildScript(t)
	h := newHarness(t, harnessOptions{respond: script.respond})
	installScenarioProcessService(t, h)
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "research", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	link := h.waitChildYield(parentID)

	unlock, err := h.mgr.lockSessionTree(h.ctx, parentID)
	require.NoError(t, err)
	require.NoError(t, h.mgr.stopTreeCleanup(h.ctx, parentID, stopTreeOptions{preserveBackgroundProcesses: true}))
	unlock()
	assert.Equal(t, subagent.StateStopped, h.link(link.ChildID).State)

	id, _ := script.processID.Load().(string)
	process, err := h.mgr.processStore.GetProcess(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateRunning, process.State, "parking preserves background work")

	script.releaseSearch(t)
	h.waitUntil("process completion recorded", func() bool { return h.processInputs(id) == 1 })
	assert.False(t, h.mgr.HasActiveLoop(link.ChildID), "a parked child does not wake")
	assert.Zero(t, h.parentDeliveries(parentID, link.ChildID))

	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue"))
	h.waitParentAnswer(parentID)
	assert.Equal(t, 1, countToolResultsFor(h.messages(parentID), tool.IDTask), "the task call is settled once")
	assert.Zero(t, h.parentDeliveries(parentID, link.ChildID))
}
