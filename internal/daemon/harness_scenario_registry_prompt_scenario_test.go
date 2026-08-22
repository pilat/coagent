package daemon

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	registryChildMarker = "CHILD_REGISTRY"
	registryUseMarker   = "USE_REGISTRY_MCP"
)

func registryPromptRespond(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, registryChildMarker) {
			return &llmwire.Response{Text: "child complete"}
		}

		if hasUserContaining(messages, registryUseMarker) {
			if hasToolResultForCallID(messages, "ping-next-activation") {
				return &llmwire.Response{Text: "mcp complete"}
			}

			return mcpPingCall("ping-next-activation")
		}

		if hasToolResultFor(messages, tool.IDMCPAdd) {
			if hasToolResultForCallID(messages, "ping-same-activation") {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpPingCall("ping-same-activation")
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return mcpToolCall("add-registry", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall("task-registry", "explore", registryChildMarker),
		}}
	}
}

func TestHarnessScenario_DynamicRegistryPromptMatchesEachActivation(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from registry", false)
	h, schemas, prompts, _ := newRegistryPromptHarness(t, registryPromptRespond(fake))
	defer h.shutdown()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "exercise dynamic registry", "fake-model", map[string]any{
		"channel": "cli",
	})
	require.NoError(t, err)

	link := h.waitForLinkByCall(parentID, "task-registry")
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	assertInitialRegistryProjection(t, h, schemas, prompts, parentID, link.ChildID)
	assert.Contains(t, lastToolResultContent(h.parentMessages(parentID), "mcp__fake__ping"), "unknown tool")
	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, registryUseMarker))
	h.mgr.waitIdle(parentID)

	assertNextRegistryProjection(t, h, schemas, prompts, parentID)
}

func assertInitialRegistryProjection(
	t *testing.T,
	h *subagentHarness,
	schemas *activationSchemas,
	prompts *promptRecorder,
	parentID, childID int64,
) {
	t.Helper()
	firstSchemas := schemas.first(t, parentID)
	for _, id := range append(dynamicRootTools(), configToolsForPromptScenario()...) {
		assert.Contains(t, firstSchemas, id, "root activation must expose daemon-registered %q", id)
	}
	assert.NotContains(t, firstSchemas, "mcp__fake__ping")

	firstPrompt := prompts.first(t, strconv.FormatInt(parentID, 10))
	assert.Contains(t, firstPrompt, "Sub-agents: task")
	assert.Contains(t, firstPrompt, "Scheduling: schedule")
	assert.Contains(t, firstPrompt, "# SCHEDULING")
	assert.NotContains(t, firstPrompt, "mcp__fake__ping")

	childSchemas := schemas.first(t, childID)
	for _, id := range []string{tool.IDTask, tool.IDSchedule, tool.IDSleep, tool.IDMCPAdd, tool.IDSetProvider} {
		assert.NotContains(t, childSchemas, id, "child registry must gate %q", id)
	}
	childPrompt := prompts.first(t, strconv.FormatInt(childID, 10))
	assert.NotContains(t, childPrompt, "Sub-agents: task")
	assert.NotContains(t, childPrompt, "# SCHEDULING")
}

func assertNextRegistryProjection(
	t *testing.T,
	h *subagentHarness,
	schemas *activationSchemas,
	prompts *promptRecorder,
	parentID int64,
) {
	t.Helper()
	lastSchemas := schemas.last(t, parentID)
	assert.Contains(t, lastSchemas, "mcp__fake__ping")
	assert.Contains(t, lastSchemas, tool.IDTask)
	assert.Contains(t, lastSchemas, tool.IDSetProvider)
	assert.Contains(t, toolResultForCallID(h.parentMessages(parentID), "ping-next-activation"), "pong from registry")

	lastPrompt := prompts.last(t, strconv.FormatInt(parentID, 10))
	assert.Contains(t, lastPrompt, "Sub-agents: task")
	assert.Contains(t, lastPrompt, "Scheduling: schedule")
}

func dynamicRootTools() []string {
	return []string{
		tool.IDTask, tool.IDSendToSubagent, tool.IDSleep, tool.IDSchedule,
		tool.IDMCPAdd, tool.IDMCPRemove, tool.IDMCPEnable, tool.IDMCPDisable, tool.IDMCPList,
	}
}

func configToolsForPromptScenario() []string {
	return []string{
		tool.IDSetProvider, tool.IDRemoveProvider, tool.IDSetManager, tool.IDRemoveManager,
		tool.IDAddModel, tool.IDRemoveModel, tool.IDSetDefaultModel, tool.IDSetModelTags,
		tool.IDRequestSecret,
	}
}
