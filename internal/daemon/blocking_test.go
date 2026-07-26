package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
)

func (h *subagentHarness) waitUntil(label string, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if cond() {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	h.t.Fatalf("timed out waiting for: %s", label)
}

func TestIntegration_BlockingTaskSuspendsAndResumes(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-release // hold the child so we can observe the suspended parent
			return &llmwire.Response{Text: "blocking child done: 7"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent got the child result"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}

		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	assert.True(t, link.Blocking, "blocking task must create a blocking link")

	// The suspended parent must hold NO run-slot: its loop goroutine exits while
	// it waits for the child (kills the priority-inversion deadlock).
	h.waitUntil("parent suspended (no live runner)", func() bool {
		return !h.mgr.HasActiveLoop(parentID)
	})

	// Release the child — it completes and its result fills the pending task call.
	close(release)

	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"), "the task tool_use is filled by the child result")

	// The revived parent must make a FRESH Chat call that consumes the result and
	// produces new text — proving it did not re-suspend on the still-pending task
	// (the silent-hang the atomic delivery + in-memory append guard against).
	assert.Equal(t, "parent got the child result", lastAssistantTextDTO(msgs),
		"parent consumed the completion instead of re-suspending")

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, LinkStateCompleted, res.State)
	assert.Equal(t, LinkOutcomeCompleted, res.Outcome, "clean finish → completed outcome")
	assert.Contains(t, res.Output, "blocking child done")
}

func TestIntegration_CompletedForegroundChildAcceptsFollowUpInSameSession(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_INITIAL") {
			if hasUserContaining(msgs, "FOLLOW_UP") {
				return &llmwire.Response{Text: "child continuation answer"}
			}

			return &llmwire.Response{Text: "child initial answer"}
		}

		if hasToolResultFor(msgs, "subagent_event") {
			return &llmwire.Response{Text: "parent received continuation"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent received initial answer"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task",
			Arguments: []byte(
				`{"prompt":"CHILD_INITIAL","description":"c","subagent_type":"general"}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "FOLLOW_UP answer one more thing"))
	h.waitUntil("foreground continuation delivered", func() bool {
		current, linkErr := h.links.GetLink(h.ctx, link.ChildID)
		return linkErr == nil && current != nil && current.Terminal() &&
			current.DeliveredAt != 0 && current.ActivationSeq == 2
	})
	h.mgr.waitIdle(parentID)

	continued, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, continued)
	assert.False(t, continued.Blocking, "a resolved foreground task continues via async completion")
	assert.Equal(t, int64(2), continued.ActivationSeq)

	messages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(messages, "task"))
	assert.Equal(t, 1, countToolResultsFor(messages, "subagent_event"))
	assert.Equal(t, "parent received continuation", lastAssistantTextDTO(messages))
}

func TestIntegration_ScatterGatherBlockingTasks(t *testing.T) {
	callIDs := []string{"sg-1", "sg-2", "sg-3"}

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			return &llmwire.Response{Text: "child done"}
		}

		// Once the 3 tasks have been emitted, only return final text — never
		// re-emit (would double-fork).
		if hasAssistantToolCall(msgs, "task") {
			return &llmwire.Response{Text: "all three children done"}
		}

		calls := make([]llmwire.ToolCall, len(callIDs))
		for i, id := range callIDs {
			calls[i] = llmwire.ToolCall{
				ID:        id,
				Name:      "task",
				Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "scatter gather", "fake-model", nil)
	require.NoError(t, err)

	// All three children are spawned, each bound to its own task call id.
	childIDs := make([]int64, 0, len(callIDs))

	for _, cid := range callIDs {
		callID := cid
		h.waitUntil("child link for "+callID, func() bool {
			link, lerr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
			return lerr == nil && link != nil
		})

		link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		require.NoError(t, err)
		assert.True(t, link.Blocking)
		childIDs = append(childIDs, link.ChildID)
	}

	for _, childID := range childIDs {
		h.waitForDelivery(childID)
	}

	h.mgr.waitIdle(parentID)

	// Each task tool_use is filled by its own child — exactly three results, and
	// the parent proceeds to the LLM only once all are resolved (transcript valid).
	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 3, countToolResultsFor(msgs, "task"), "each of the 3 task calls gets its own result")
}
