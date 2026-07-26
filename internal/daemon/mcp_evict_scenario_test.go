package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

func lastUserText(msgs []llmwire.Message) string {
	text := ""

	for _, m := range msgs {
		if m.Role == llmwire.RoleUser {
			text = m.Content
		}
	}

	return text
}

// mcp_disable retires the pooled subprocess, but a session already holding the
// client keeps it: the entry dies on the last release, never under an active call.
func TestScenario_MCPDisableEvictsThePoolWithoutBreakingAnInFlightSession(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from held run", true)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch last := lastUserText(msgs); {
		case strings.Contains(last, "DISABLE_IT"):
			if hasToolResultFor(msgs, tool.IDMCPDisable) {
				return &llmwire.Response{Text: "disabled"}
			}

			return mcpToolCall("disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`)
		case strings.Contains(last, "HOLD_IT"):
			if hasToolResultForCallID(msgs, "ping-held") {
				return &llmwire.Response{Text: "held run done"}
			}

			return mcpPingCall("ping-held")
		case strings.Contains(last, "ENABLE_IT"):
			if hasToolResultFor(msgs, tool.IDMCPEnable) {
				return &llmwire.Response{Text: "enabled"}
			}

			return mcpToolCall("enable-1", tool.IDMCPEnable, `{"name":"fake","scope":"project"}`)
		case strings.Contains(last, "USE_AGAIN"):
			if hasToolResultForCallID(msgs, "ping-again") {
				return &llmwire.Response{Text: "used again"}
			}

			return mcpPingCall("ping-again")
		default:
			if hasToolResultFor(msgs, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}

	h, _, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	holder, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "registered"
	})

	// The holder's next run acquires the pooled client and parks inside tools/call.
	require.NoError(t, h.mgr.SendToSession(h.ctx, holder, "HOLD_IT while I reconfigure"))
	h.waitUntil("the held call reaches the server", func() bool { return fake.count(t, "call") == 1 })
	require.Equal(t, 1, fake.count(t, "spawn"))

	// A second session disables the server — Evict fires while the holder is mid-call.
	disabler, err := h.mgr.Send(h.ctx, h.projectID, "DISABLE_IT now", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("the disabling run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(disabler)) == "disabled"
	})

	assert.True(t, h.mgr.HasActiveLoop(holder), "eviction must not tear down the session holding the client")

	fake.unblock(t)
	h.waitUntil("the held run completes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "held run done"
	})

	held := h.parentMessages(holder)
	require.NoError(t, llm.ValidateToolPairing(held))
	assert.Contains(t, toolResultForCallID(held, "ping-held"), "pong from held run",
		"the in-flight call answers normally despite the eviction")

	require.NoError(t, llm.ValidateToolPairing(h.parentMessages(disabler)))

	// The retired subprocess is gone rather than idling in the pool: re-enabling and
	// using the server again has to spawn a second one.
	require.NoError(t, h.mgr.SendToSession(h.ctx, holder, "ENABLE_IT again"))
	h.waitUntil("re-enable lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "enabled"
	})

	require.NoError(t, h.mgr.SendToSession(h.ctx, holder, "USE_AGAIN please"))
	h.waitUntil("the server answers again", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "used again"
	})

	msgs := h.parentMessages(holder)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-again"), "pong from held run")
	assert.Equal(t, 2, fake.count(t, "spawn"), "the evicted subprocess was retired, not reused")
}

// A disabled server is absent from the next run's tool inventory — the mutation
// reaches the offered tools, not just the registry table.
func TestScenario_MCPDisableRemovesTheToolFromTheNextRun(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch last := lastUserText(msgs); {
		case strings.Contains(last, "DISABLE_IT"):
			if hasToolResultFor(msgs, tool.IDMCPDisable) {
				return &llmwire.Response{Text: "disabled"}
			}

			return mcpToolCall("disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`)
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(msgs, "ping-after-disable") {
				return &llmwire.Response{Text: "tried it"}
			}

			return mcpPingCall("ping-after-disable")
		default:
			if hasToolResultFor(msgs, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}

	h, _, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "DISABLE_IT now"))
	h.waitUntil("the disable lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "disabled"
	})

	// The disabling run still had the server, so it spawned one; the next one must not.
	spawnsBefore := fake.count(t, "spawn")

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_IT anyway"))
	h.waitUntil("the run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "tried it"
	})

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-after-disable"), "unknown tool",
		"a disabled server is gone from the next run's tools")
	assert.Equal(t, spawnsBefore, fake.count(t, "spawn"), "a disabled server is not spawned again")
}
