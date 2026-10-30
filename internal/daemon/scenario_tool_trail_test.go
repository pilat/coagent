package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

func TestHarnessScenario_WorkToolStepsPublishOneMessageEach(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if toolResultForCallID(messages, "text-ls") != nil {
			return textReply("Work complete.")
		}
		if toolResultForCallID(messages, "silent-ls") != nil {
			return assistantCalls("Checking files", llmwire.ToolCall{
				ID: "text-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
			})
		}
		return assistantCalls(" \n", llmwire.ToolCall{
			ID: "silent-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
		}, llmwire.ToolCall{ID: "silent-glob", Name: "glob", Arguments: []byte(`{"pattern":"*.go"}`)})
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "inspect files", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitIdleAfter(root, "Work complete.")
	assertNarratedOutputOrder(t, h, root, "→ ls: `.`\n→ glob: `*.go`", "Checking files\n\n→ ls: `.`", "Work complete.")
	for _, message := range h.messages(root) {
		assert.NotContains(t, message.Content, "→ ls:", "the trail belongs only to the output log")
	}
	assert.Equal(t, 1, countPublishedMessage(collector.snapshot(), root, "→ ls: `.`\n→ glob: `*.go`"))
	assertHarnessTrace(t, h, "tool_trail_work_steps.json", collector.snapshot(), root)
}

func TestHarnessScenario_AssistantToolStepsKeepTextOnly(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if toolResultForCallID(messages, "text-models") != nil {
			return textReply("Assistant complete.")
		}
		if toolResultForCallID(messages, "silent-models") != nil {
			return assistantCalls("Checking models", llmwire.ToolCall{
				ID: "text-models", Name: "models", Arguments: []byte(`{}`),
			})
		}
		return callReply("silent-models", "models", `{}`)
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "check the models"))
	h.waitUntil("assistant finished", func() bool {
		return assistantSayCount(t, h, root.ID, "Assistant complete.") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assertNarratedOutputOrder(t, h, root.ID, "Checking models", "Assistant complete.")
	assert.Equal(t, 2, countToolResultsFor(h.messages(root.ID), "models"))
}

func TestHarnessScenario_SubagentToolStepsHaveNoOutbox(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "TRAIL_CHILD") {
			if hasToolResultFor(messages, "ls") {
				return textReply("child complete")
			}
			return assistantCalls("Child inspecting", llmwire.ToolCall{
				ID: "child-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
			})
		}
		if hasToolResultFor(messages, "task") {
			return textReply("Parent complete.")
		}
		return callReply(
			"trail-child",
			"task",
			`{"prompt":"TRAIL_CHILD","description":"inspect child","subagent_type":"general"}`,
		)
	}})
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "inspect with a child", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	h.waitUntil("parent finished", func() bool {
		return lastAssistantTextDTO(h.messages(root)) == "Parent complete." && !h.mgr.HasActiveLoop(root)
	})
	link := h.linkByCall(root, "trail-child")
	require.NotNil(t, link)
	assert.Empty(t, h.outbox(link.ChildID))
	assert.Equal(t, 1, countToolResultsFor(h.messages(link.ChildID), "ls"))
	for _, row := range h.outbox(root) {
		assert.NotContains(t, row.Content, "Child inspecting")
	}
}
