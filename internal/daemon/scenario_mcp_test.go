package daemon

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// Catalog refresh follows the same live client across activation boundaries.
func TestScenario_NextMCPStackReusesProcessAndRefreshesTools(t *testing.T) {
	fake := newShiftingMCPServer(t, "pong from fake")
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch last := lastUserText(msgs); {
		case strings.Contains(last, "AFTER_CRASH"):
			if hasToolResultForCallID(msgs, "after-crash") {
				return &llmwire.Response{Text: "recovered"}
			}
			return mcpPingCall("after-crash")
		case strings.Contains(last, "USE_SECOND"):
			if hasToolResultForCallID(msgs, "ping2-new") {
				return &llmwire.Response{Text: "used second"}
			}
			return mcpToolCall("ping2-new", "mcp__fake__ping2", "{}")
		case strings.Contains(last, "USE_FIRST"):
			if hasToolResultForCallID(msgs, "ping-first") {
				return &llmwire.Response{Text: "used first"}
			}
			return mcpPingCall("ping-first")
		default:
			if hasToolResultFor(msgs, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}
			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}
	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_FIRST now"))
	h.waitUntil("first call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used first"
	})
	h.mgr.waitIdle(sessionID)
	firstSpawns := fake.count(t, "spawn")
	assert.GreaterOrEqual(t, firstSpawns, 1)
	assert.Equal(t, 1, fake.count(t, "call"))
	require.NoError(t, os.WriteFile(fake.log+".extra", nil, 0o600))

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_SECOND now"))
	h.waitUntil("second call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used second"
	})
	h.mgr.waitIdle(sessionID)
	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping2-new"), "pong from fake")
	assert.Equal(t, firstSpawns, fake.count(t, "spawn"), "catalog refresh must reuse the session's live process")
	assert.Equal(t, 2, fake.count(t, "call"))
	pidText, err := os.ReadFile(fake.log + ".pid")
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	require.NoError(t, err)
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, process.Kill())
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "AFTER_CRASH now"))
	h.waitUntil("replacement client answers", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "recovered"
	})
	h.mgr.waitIdle(sessionID)
	assert.Greater(t, fake.count(t, "spawn"), firstSpawns)
	assert.Contains(t, toolResultForCallID(h.parentMessages(sessionID), "after-crash"), "pong from fake")

	spawnsBeforeStop := fake.count(t, "spawn")
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/stop"))
	assert.Equal(t, spawnsBeforeStop, fake.count(t, "spawn"),
		"transcript settlement must not launch project MCP code")
	require.Eventually(t, func() bool {
		return fake.countNoFail("exit") >= fake.countNoFail("spawn")
	}, 5*time.Second, 10*time.Millisecond, "stop must close retained MCP processes")
}

// Disabling a server changes the next stack without interrupting an active call.
func TestScenario_MCPDisableDoesNotBreakAnInFlightStack(t *testing.T) {
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

	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	holder, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "registered"
	})
	h.mgr.waitIdle(holder)

	// The holder's next run parks inside tools/call.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, holder, "HOLD_IT while I reconfigure"))
	h.waitUntil("the held call reaches the server", func() bool { return fake.count(t, "call") == 1 })
	require.GreaterOrEqual(t, fake.count(t, "spawn"), 1)

	// A second session disables the server while the holder is mid-call.
	h.startInboxWake()
	disabler, err := h.mgr.Send(h.ctx, h.projectID, "DISABLE_IT now", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("the disabling run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(disabler)) == "disabled"
	})

	assert.True(t, h.mgr.HasActiveLoop(holder), "the registry change must not tear down the active call")

	fake.unblock(t)
	h.waitUntil("the held run completes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "held run done"
	})

	held := h.parentMessages(holder)
	require.NoError(t, llm.ValidateToolPairing(held))
	assert.Contains(t, toolResultForCallID(held, "ping-held"), "pong from held run",
		"the in-flight call answers normally despite the registry change")

	require.NoError(t, llm.ValidateToolPairing(h.parentMessages(disabler)))

	spawnsBeforeResume := fake.count(t, "spawn")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, holder, "ENABLE_IT again"))
	h.waitUntil("re-enable lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "enabled"
	})
	h.mgr.waitIdle(holder)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, holder, "USE_AGAIN please"))
	h.waitUntil("the server answers again", func() bool {
		return lastAssistantTextDTO(h.parentMessages(holder)) == "used again"
	})
	h.mgr.waitIdle(holder)

	msgs := h.parentMessages(holder)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-again"), "pong from held run")
	assert.Greater(t, fake.count(t, "spawn"), spawnsBeforeResume,
		"the next stack starts a newly enabled server")
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

	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "DISABLE_IT now"))
	h.waitUntil("the disable lands", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "disabled"
	})
	h.mgr.waitIdle(sessionID)

	// The disabling run still had the server, so it spawned one; the next one must not.
	spawnsBefore := fake.count(t, "spawn")

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_IT anyway"))
	h.waitUntil("the run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "tried it"
	})
	h.mgr.waitIdle(sessionID)

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-after-disable"), "unknown tool",
		"a disabled server is gone from the next run's tools")
	assert.Equal(t, spawnsBefore, fake.count(t, "spawn"), "a disabled server is not spawned again")
}

func TestHarnessModel_MCPRegistryProjectionAndStackProtocol(t *testing.T) {
	h := newRegistryModelHarness(t)
	t.Cleanup(h.close)

	h.apply(t, registryAdd)
	h.assertVisible(t, false, "add is deferred until a stack rebuild")
	h.apply(t, registryRebuild)
	h.assertVisible(t, true, "rebuild acquires the enabled registry row")

	h.apply(t, registryDisable)
	h.assertVisible(t, true, "disable does not interrupt the current stack")
	h.apply(t, registryRelease)
	h.fake.waitForExit(t)
	h.assertVisible(t, false, "release closes the evicted disabled process")
	h.apply(t, registryRebuild)
	h.assertVisible(t, false, "disabled rows are absent from the next stack")

	h.apply(t, registryRestart)
	h.assertVisible(t, false, "restart does not resurrect disabled availability")
	h.apply(t, registryEnable)
	h.assertVisible(t, false, "enable is deferred until a later rebuild")
	h.apply(t, registryRebuild)
	h.assertVisible(t, true, "enabled availability appears on the next rebuild")

	h.apply(t, registryRemove)
	h.assertVisible(t, true, "remove retires but does not interrupt the current stack")
	h.apply(t, registryRelease)
	h.fake.waitForExitCount(t, 2)
	h.assertVisible(t, false, "release closes the evicted removed process")
	h.apply(t, registryRebuild)
	h.assertVisible(t, false, "removed availability cannot return")
	assert.Equal(t, 2, h.fake.count(t, "spawn"), "only the two enabled rebuilds spawn a process")
}

// The propagation contract at the real boundary: a server registered by the
// mcp_add tool is absent from the run that registered it and present in the next
// one, spawned by the next stack and answering for real.
func TestScenario_MCPAddReachesTheNextRunOnly(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "USE_IT") {
			if hasToolResultForCallID(msgs, "ping-next-run") {
				return &llmwire.Response{Text: "used it"}
			}

			return mcpPingCall("ping-next-run")
		}

		if hasToolResultForCallID(msgs, "ping-same-run") {
			return &llmwire.Response{Text: "registered"}
		}

		if hasToolResultFor(msgs, tool.IDMCPAdd) {
			return mcpPingCall("ping-same-run")
		}

		return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
	}

	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake mcp server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registering run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	h.mgr.waitIdle(sessionID)

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDMCPAdd), "next run")
	assert.Contains(t, toolResultForCallID(msgs, "ping-same-run"), "unknown tool",
		"the run that registered the server must not gain its tools")
	assert.Equal(t, 0, fake.count(t, "call"), "a mid-run registration executes nothing")

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("next run uses the server", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used it"
	})
	h.mgr.waitIdle(sessionID)

	msgs = h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-next-run"), "pong from fake",
		"the next run offers mcp__fake__ping and it answers from the real server")
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1, "the next stack started the server")
}

// The scope override is an mcpstore contract, but what a session may call is the
// user-visible half: a project row of the same name wins in the offered tools.
func TestScenario_ProjectMCPServerOverridesTheGlobalOfTheSameName(t *testing.T) {
	global := newFakeMCPServer(t, "pong from global", false)
	project := newFakeMCPServer(t, "pong from project", false)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "USE_IT") {
			if hasToolResultForCallID(msgs, "ping-override") {
				return &llmwire.Response{Text: "used it"}
			}

			return mcpPingCall("ping-override")
		}

		if hasToolResultForCallID(msgs, "add-project") {
			return &llmwire.Response{Text: "registered both"}
		}

		if hasToolResultForCallID(msgs, "add-global") {
			return mcpToolCall("add-project", tool.IDMCPAdd, project.addParams("fake", "project"))
		}

		return mcpToolCall("add-global", tool.IDMCPAdd, global.addParams("fake", "global"))
	}

	h, _ := newMCPHarness(t, respond)
	defer h.shutdown()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register both scopes", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("both registrations land", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered both"
	})
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("next run uses the server", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used it"
	})
	h.mgr.waitIdle(sessionID)

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-override"), "pong from project")
	assert.GreaterOrEqual(t, project.count(t, "spawn"), 1, "the project row is the one that runs")
	assert.Equal(t, 0, global.count(t, "spawn"), "the shadowed global is never spawned")
}

func TestScenario_MCPDisablePersistsAcrossDaemonRestart(t *testing.T) {
	fake := newFakeMCPServer(t, "pong before restart", false)
	dbPath := filepath.Join(t.TempDir(), "mcp-restart.db")
	workDir := t.TempDir()
	respond := disableScenarioResponder(fake)

	first := newMCPRestartHarness(t, dbPath, workDir, respond)
	first.startInboxWake()
	sessionID, err := first.mgr.Send(first.ctx, first.projectID, "register the fake server", "fake-model", nil)
	require.NoError(t, err)
	// waitIdle, not just the text: the candidate stop carries the same text as
	// the confirmation, and the next input must not race the pending check.
	first.waitUntil("registration finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "registered"
	})
	first.mgr.waitIdle(sessionID)

	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionID, "USE_IT now"))
	first.waitUntil("MCP call finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "used before restart"
	})
	first.mgr.waitIdle(sessionID)
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1)

	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionID, "DISABLE_IT now"))
	first.waitUntil("disable finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "disabled"
	})
	first.mgr.waitIdle(sessionID)
	defs, err := first.registry.ListForProject(first.ctx, first.projectID)
	require.NoError(t, err)
	assert.Empty(t, defs, "the disabled row is absent from the session's enabled projection")
	spawnsBeforeRestart := fake.count(t, "spawn")
	first.close()

	second := newMCPRestartHarness(t, dbPath, workDir, respond)
	require.NoError(t, second.mgr.Start(second.ctx))
	second.startInboxWake()
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "USE_AFTER_RESTART now"))
	second.waitUntil("post-restart run finishes", func() bool {
		return lastAssistantTextDTO(second.parentMessages(sessionID)) == "used after restart"
	})
	second.mgr.waitIdle(sessionID)

	messages := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-restart"), "unknown tool",
		"a disabled registry row must not return in a fresh daemon activation")
	assert.Equal(t, spawnsBeforeRestart, fake.count(t, "spawn"), "restart must not spawn stale MCP availability")
}

func TestScenario_MCPRemoveClosesStackProcessBeforeTheNextRun(t *testing.T) {
	fake := newExitTrackingMCPServer(t, "pong before removal")
	dbPath := filepath.Join(t.TempDir(), "mcp-remove.db")
	workDir := t.TempDir()
	respond := removeScenarioResponder(t, fake)
	h := newMCPRestartHarness(t, dbPath, workDir, respond)

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	h.mgr.waitIdle(sessionID)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("MCP call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used before remove"
	})
	h.mgr.waitIdle(sessionID)
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "REMOVE_IT now"))
	h.waitUntil("removal finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "removed"
	})
	h.mgr.waitIdle(sessionID)
	fake.waitForExit(t)
	spawnsAfterRemove := fake.count(t, "spawn")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "USE_AFTER_REMOVE now"))
	h.waitUntil("post-removal run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used after remove"
	})
	h.mgr.waitIdle(sessionID)

	messages := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-remove"), "unknown tool",
		"a removed row must be absent from the next stack")
	assert.Equal(t, spawnsAfterRemove, fake.count(t, "spawn"), "removal must not restart the deleted server")
}
