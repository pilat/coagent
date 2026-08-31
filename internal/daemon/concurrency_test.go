package daemon

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/subagent"
)

// trivialRespond: every session finishes immediately. Used when the test drives
// Spawn directly and does not want children to spawn further.
func trivialRespond(_ string, _ []llmwire.Message) *llmwire.Response {
	return &llmwire.Response{Text: "done"}
}

// blockingParentRespond builds a respond that spawns one blocking child, then
// finishes once the child's result lands. childBody runs as the child's turn.
func blockingParentRespond(childBody func() *llmwire.Response) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			return childBody()
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK","description":"c","subagent_type":"general"}`),
		}}}
	}
}

func lastAssistantTextDTO(msgs []llmwire.Message) string {
	for _, v := range slices.Backward(msgs) {
		m := v
		if m.Role == llmwire.RoleAssistant && len(m.ToolCalls) == 0 && m.Content != "" {
			return m.Content
		}
	}

	return ""
}

// TestAdmissionCaps_ChildrenCappedBelowTotal guards the load-bearing
// deadlock-freedom invariant: children are capped strictly below the total, so at
// least one slot is always reservable by a parent. A completing child can
// therefore always re-admit its suspended (slot-free) parent, killing the
// priority-inversion deadlock. If this relationship ever flips, durable blocking
// fan-in can deadlock — fail loudly at compile/test time instead.
func TestAdmissionCaps_ChildrenCappedBelowTotal(t *testing.T) {
	assert.Less(t, maxChildSlots, maxTotalSlots, "maxChildSlots must stay strictly below maxTotalSlots")
	assert.LessOrEqual(t, maxInFlightPerParent, maxChildSlots, "per-parent cap cannot exceed the child cap")
}

func TestIntegration_DepthCapRejected(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	// depth 1: root → child A
	a, err := h.mgr.Spawn(
		h.ctx,
		spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"},
	)
	require.NoError(t, err)

	// depth 2: A → grandchild B (allowed — root → child → grandchild)
	b, err := h.mgr.Spawn(
		h.ctx,
		spawnRequest{ParentID: a.ChildID, AgentType: "general", Prompt: "x"},
	)
	require.NoError(t, err)

	// depth 3: B → great-grandchild — rejected as a tool error.
	_, err = h.mgr.Spawn(
		h.ctx,
		spawnRequest{ParentID: b.ChildID, AgentType: "general", Prompt: "x"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nesting limit")
}

func TestIntegration_SuspendedParentHoldsNoSlot(t *testing.T) {
	release := make(chan struct{})

	h := newSubagentHarnessWith(t, blockingParentRespond(func() *llmwire.Response {
		<-release

		return &llmwire.Response{Text: "child done"}
	}))
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)

	// The parent suspends (loop exits, slot released); only the in-flight child
	// holds a slot. The suspended parent holds ZERO.
	h.waitUntil("parent suspended, only child holds a slot", func() bool {
		return !h.mgr.HasActiveLoop(parentID) && h.mgr.admit.liveTotal() == 1
	})
	assert.Equal(t, int64(1), h.mgr.admit.liveChildren())

	closeOnce(release)
	h.waitForDelivery(link.ChildID)
}

func TestIntegration_CascadeKillsBlockingChild(t *testing.T) {
	release := make(chan struct{})

	h := newSubagentHarnessWith(t, blockingParentRespond(func() *llmwire.Response {
		<-release

		return &llmwire.Response{Text: "child done"}
	}))
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	// Killing the parent must cascade-kill its in-flight blocking child.
	require.NoError(t, h.mgr.Kill(h.ctx, parentID))

	h.waitUntil("blocking child killed", func() bool {
		rec, gerr := h.sessStore.GetSession(h.ctx, link.ChildID)
		return gerr == nil && rec.KilledAt != nil
	})

	childRec, err := h.sessStore.GetSession(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.NotNil(t, childRec.KilledAt, "blocking descendant is killed with its parent")

	childLink, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateKilled, childLink.State)
	assert.Equal(t, subagent.OutcomeKilled, childLink.Outcome, "a killed child reports the killed outcome")
}

func TestIntegration_ChildPanicMarksError(t *testing.T) {
	h := newSubagentHarnessWith(t, blockingParentRespond(func() *llmwire.Response {
		panic("boom in child")
	}))
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)

	// The panicked child is marked error and its parent is unblocked.
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateError, res.State)
	assert.Equal(t, subagent.OutcomeError, res.Outcome, "a panicked child reports the error outcome")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"), "parent's task call is resolved with the error")
}

func TestIntegration_BlockingChildTimeout(t *testing.T) {
	hang := make(chan struct{}) // closed only in cleanup — the child hangs until its timeout fires

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-hang

			return &llmwire.Response{Text: "unreached"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK","description":"c","subagent_type":"general","timeout":1}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(hang)
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	assert.Equal(t, 1, link.TimeoutSec, "task timeout param propagates to the link")

	// The child hangs; its 1s wall-clock timeout fires, marking it terminal and
	// unblocking the parent (which would otherwise wait forever).
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateError, res.State, "a timed-out child is terminal-error")
	assert.Equal(t, subagent.OutcomeError, res.Outcome, "a timed-out child reports the error outcome")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"), "timed-out child still resolves the parent's task call")
}

func TestIntegration_StressBlockingNoDeadlock(t *testing.T) {
	h := newSubagentHarnessWith(t, blockingParentRespond(func() *llmwire.Response {
		return &llmwire.Response{Text: "child done"}
	}))
	defer h.shutdown()

	const parents = 6

	ids := make([]int64, 0, parents)

	for range parents {
		id, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
		require.NoError(t, err)
		ids = append(ids, id)
	}

	// Each parent spawns its child, suspends, the child completes, the parent
	// resumes — under saturation, with no deadlock.
	for _, pid := range ids {
		link := h.waitForChildLink(pid)
		h.waitForDelivery(link.ChildID)
	}

	for _, pid := range ids {
		h.mgr.waitIdle(pid)
		msgs := h.parentMessages(pid)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, "parent done", lastAssistantTextDTO(msgs), "parent %d resumed to completion", pid)
	}

	// Caps were never exceeded; everything drained back to idle.
	assert.LessOrEqual(t, h.mgr.admit.liveChildren(), int64(maxChildSlots))
	assert.LessOrEqual(t, h.mgr.admit.liveTotal(), int64(maxTotalSlots))
}

func TestIntegration_BackgroundQueueDrains(t *testing.T) {
	release := make(chan struct{})

	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("bg-%d", i)
	}

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-release

			return &llmwire.Response{Text: "child done"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent done"}
		}

		calls := make([]llmwire.ToolCall, len(ids))
		for i, id := range ids {
			calls[i] = llmwire.ToolCall{
				ID: id, Name: "task",
				Arguments: []byte(
					`{"prompt":"CHILD_TASK","description":"c","subagent_type":"general","background":true}`,
				),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn many", "fake-model", nil)
	require.NoError(t, err)

	// Per-parent cap is 8: 8 children run (blocked on release), the other 2 are
	// parked in the in-memory FIFO. Every link is persisted regardless.
	h.waitUntil("8 admitted, 2 queued", func() bool {
		return h.mgr.admit.liveChildren() == int64(maxInFlightPerParent) && h.queueLen() == 2
	})

	// Release: the 8 finish, freeing slots; drainQueue starts the 2 parked ones.
	// All 10 must eventually complete and deliver — none dropped.
	closeOnce(release)

	for _, id := range ids {
		link := h.waitForLinkByCall(parentID, id)
		h.waitForDelivery(link.ChildID)
	}

	h.waitUntil("queue drained", func() bool { return h.queueLen() == 0 })
	assert.LessOrEqual(t, h.mgr.admit.liveChildren(), int64(maxChildSlots))
}

// closeOnce closes ch unless it is already closed (cleanup helper for hold channels).
func closeOnce(ch chan struct{}) {
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (h *subagentHarness) queueLen() int {
	h.mgr.queueMu.Lock()
	defer h.mgr.queueMu.Unlock()

	return len(h.mgr.queue)
}

func (h *subagentHarness) waitForLinkByCall(parentID int64, callID string) subagent.Link {
	h.t.Helper()
	h.waitUntil("link for "+callID, func() bool {
		link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		return err == nil && link != nil
	})

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
	require.NoError(h.t, err)

	return *link
}
