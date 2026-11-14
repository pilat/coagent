package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/tool"
)

// Catalog refresh follows the same live client across activation boundaries.
func TestScenario_NextMCPStackReusesProcessAndRefreshesTools(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)
	respond := scriptTurns{
		{"AFTER_CRASH", []scriptCall{{"after-crash", "mcp__fake__ping", `{}`}}, "recovered"},
		{"USE_SECOND", []scriptCall{{"ping2-new", "mcp__fake__ping2", `{}`}}, "used second"},
		{"USE_FIRST", []scriptCall{{"ping-first", "mcp__fake__ping", `{}`}}, "used first"},
		{"", []scriptCall{{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")}}, "registered"},
	}.respond
	h := newHarness(t, harnessOptions{respond: respond})
	sessionID := h.ask("register the fake mcp server", "registered")
	h.reply(sessionID, "USE_FIRST now", "used first")
	firstSpawns := fake.count(t, "spawn")
	assert.GreaterOrEqual(t, firstSpawns, 1)
	assert.Equal(t, 1, fake.count(t, "call"))
	require.NoError(t, os.WriteFile(fake.log+".extra", nil, 0o600))
	h.reply(sessionID, "USE_SECOND now", "used second")
	msgs := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping2-new").Content, "pong from fake")
	assert.Equal(t, firstSpawns, fake.count(t, "spawn"), "catalog refresh must reuse the session's live process")
	assert.Equal(t, 2, fake.count(t, "call"))
	pidText, err := os.ReadFile(fake.log + ".pid")
	require.NoError(t, err)
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidText)))
	require.NoError(t, err)
	process, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, process.Kill())
	h.reply(sessionID, "AFTER_CRASH now", "recovered")
	assert.Greater(t, fake.count(t, "spawn"), firstSpawns)
	assert.Contains(t, toolResultForCallID(h.messages(sessionID), "after-crash").Content, "pong from fake")
	spawnsBeforeStop := fake.count(t, "spawn")
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/stop"))
	assert.Equal(t, spawnsBeforeStop, fake.count(t, "spawn"), "transcript settlement must not launch project MCP code")
	h.waitUntil("MCP processes stopped", func() bool {
		return fake.countNoFail("exit") >= fake.countNoFail("spawn")
	})
}

// Disabling a server changes the next stack without interrupting an active call.
func TestScenario_MCPDisableDoesNotBreakAnInFlightStack(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from held run", true)
	respond := scriptTurns{
		{"DISABLE_IT", []scriptCall{{"disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`}}, "disabled"},
		{"HOLD_IT", []scriptCall{{"ping-held", "mcp__fake__ping", `{}`}}, "held run done"},
		{"ENABLE_IT", []scriptCall{{"enable-1", tool.IDMCPEnable, `{"name":"fake","scope":"project"}`}}, "enabled"},
		{"USE_AGAIN", []scriptCall{{"ping-again", "mcp__fake__ping", `{}`}}, "used again"},
		{"", []scriptCall{{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")}}, "registered"},
	}.respond
	h := newHarness(t, harnessOptions{respond: respond})
	holder := h.ask("register the fake mcp server", "registered")

	// The holder's next run parks inside tools/call.
	h.say(holder, "HOLD_IT while I reconfigure")
	h.waitUntil("the held call reaches the server", func() bool { return fake.count(t, "call") == 1 })
	require.GreaterOrEqual(t, fake.count(t, "spawn"), 1)

	// A second session disables the server while the holder is mid-call.
	disabler := h.ask("DISABLE_IT now", "disabled")
	assert.True(t, h.mgr.HasActiveLoop(holder), "the registry change must not tear down the active call")
	fake.unblock(t)
	h.waitUntil("the held run completes", func() bool {
		return lastAssistantTextDTO(h.messages(holder)) == "held run done"
	})
	held := h.messages(holder)
	require.NoError(t, llmtest.ValidateToolPairing(held))
	assert.Contains(t, toolResultForCallID(held, "ping-held").Content, "pong from held run",
		"the in-flight call answers normally despite the registry change")
	require.NoError(t, llmtest.ValidateToolPairing(h.messages(disabler)))
	spawnsBeforeResume := fake.count(t, "spawn")
	h.reply(holder, "ENABLE_IT again", "enabled")
	h.reply(holder, "USE_AGAIN please", "used again")
	msgs := h.messages(holder)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-again").Content, "pong from held run")
	assert.Greater(t, fake.count(t, "spawn"), spawnsBeforeResume, "the next stack starts a newly enabled server")
}

// Disabling a server must change the next run's offered tools as well as its registry row.
func TestScenario_MCPDisableRemovesTheToolFromTheNextRun(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)
	respond := scriptTurns{
		{"DISABLE_IT", []scriptCall{{"disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`}}, "disabled"},
		{"USE_IT", []scriptCall{{"ping-after-disable", "mcp__fake__ping", `{}`}}, "tried it"},
		{"", []scriptCall{{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")}}, "registered"},
	}.respond
	h := newHarness(t, harnessOptions{respond: respond})
	sessionID := h.send("register the fake mcp server", nil)
	h.waitUntil("registration lands", func() bool {
		return lastAssistantTextDTO(h.messages(sessionID)) == "registered"
	})
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "DISABLE_IT now"))
	h.waitAnswer(sessionID, "disabled")

	// The disabling run still had the server, so it spawned one; the next one must not.
	spawnsBefore := fake.count(t, "spawn")
	h.reply(sessionID, "USE_IT anyway", "tried it")
	msgs := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-after-disable").Content, "unknown tool",
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
	h.waitUntil("MCP process exit", func() bool { return h.fake.count(t, "exit") >= 1 })
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
	h.waitUntil("MCP process exits", func() bool { return h.fake.count(t, "exit") >= 2 })
	h.assertVisible(t, false, "release closes the evicted removed process")
	h.apply(t, registryRebuild)
	h.assertVisible(t, false, "removed availability cannot return")
	assert.Equal(t, 2, h.fake.count(t, "spawn"), "only the two enabled rebuilds spawn a process")
}

// An MCP server registered in one run becomes available only in the next stack, which must spawn and call it.
func TestScenario_MCPAddReachesTheNextRunOnly(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from fake", false)
	respond := scriptTurns{
		{"USE_IT", []scriptCall{{"ping-next-run", "mcp__fake__ping", `{}`}}, "used it"},
		{
			"",
			[]scriptCall{
				{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")},
				{"ping-same-run", "mcp__fake__ping", `{}`},
			},
			"registered",
		},
	}.respond
	h := newHarness(t, harnessOptions{respond: respond})
	sessionID := h.ask("register the fake mcp server", "registered")
	msgs := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	toolOutput := lastToolResultContent(msgs, tool.IDMCPAdd)
	assert.Contains(t, toolOutput, "next run")
	assert.Contains(t, toolResultForCallID(msgs, "ping-same-run").Content, "unknown tool",
		"the run that registered the server must not gain its tools")
	assert.Equal(t, 0, fake.count(t, "call"), "a mid-run registration executes nothing")
	h.reply(sessionID, "USE_IT now", "used it")
	msgs = h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-next-run").Content, "pong from fake",
		"the next run offers mcp__fake__ping and it answers from the real server")
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1, "the next stack started the server")
}

// A project MCP row must override the global row in the tools offered to a session.
func TestScenario_ProjectMCPServerOverridesTheGlobalOfTheSameName(t *testing.T) {
	global := newFakeMCPServer(t, "pong from global", false)
	project := newFakeMCPServer(t, "pong from project", false)
	respond := scriptTurns{
		{"USE_IT", []scriptCall{{"ping-override", "mcp__fake__ping", `{}`}}, "used it"},
		{
			"",
			[]scriptCall{
				{"add-global", tool.IDMCPAdd, global.addParams("fake", "global")},
				{"add-project", tool.IDMCPAdd, project.addParams("fake", "project")},
			},
			"registered both",
		},
	}.respond
	h := newHarness(t, harnessOptions{respond: respond})
	sessionID := h.ask("register both scopes", "registered both")
	h.reply(sessionID, "USE_IT now", "used it")
	msgs := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, toolResultForCallID(msgs, "ping-override").Content, "pong from project")
	assert.GreaterOrEqual(t, project.count(t, "spawn"), 1, "the project row is the one that runs")
	assert.Equal(t, 0, global.count(t, "spawn"), "the shadowed global is never spawned")
}

func TestScenario_MCPDisablePersistsAcrossDaemonRestart(t *testing.T) {
	fake := newFakeMCPServer(t, "pong before restart", false)
	dbPath := filepath.Join(t.TempDir(), "mcp-restart.db")
	workDir := t.TempDir()
	respond := disableScenarioResponder(fake)
	first := newScheduleRestartHarness(t, dbPath, workDir, respond)
	sessionID := first.send("register the fake server", nil)
	// Wait for teardown: candidate and confirmation texts match, so visible text alone can race the check.
	first.waitAnswer(sessionID, "registered")
	first.reply(sessionID, "USE_IT now", "used before restart")
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1)
	first.reply(sessionID, "DISABLE_IT now", "disabled")
	defs, err := first.mgr.mcpStore.ListForProject(first.ctx, first.projectID)
	require.NoError(t, err)
	assert.Empty(t, defs, "the disabled row is absent from the session's enabled projection")
	spawnsBeforeRestart := fake.count(t, "spawn")
	require.NoError(t, first.close())
	second := newScheduleRestartHarness(t, dbPath, workDir, respond)
	require.NoError(t, second.mgr.Start(second.ctx))
	second.reply(sessionID, "USE_AFTER_RESTART now", "used after restart")
	messages := second.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-restart").Content, "unknown tool",
		"a disabled registry row must not return in a fresh daemon activation")
	assert.Equal(t, spawnsBeforeRestart, fake.count(t, "spawn"), "restart must not spawn stale MCP availability")
}

func TestScenario_MCPRemoveClosesStackProcessBeforeTheNextRun(t *testing.T) {
	fake := newFakeMCPServer(t, "pong before removal", false)
	dbPath := filepath.Join(t.TempDir(), "mcp-remove.db")
	workDir := t.TempDir()
	respond := removeScenarioResponder(t, fake)
	h := newScheduleRestartHarness(t, dbPath, workDir, respond)
	sessionID := h.ask("register the fake server", "registered")
	h.reply(sessionID, "USE_IT now", "used before remove")
	assert.GreaterOrEqual(t, fake.count(t, "spawn"), 1)
	h.reply(sessionID, "REMOVE_IT now", "removed")
	h.waitUntil("MCP process exit", func() bool { return fake.count(t, "exit") >= 1 })
	spawnsAfterRemove := fake.count(t, "spawn")
	h.reply(sessionID, "USE_AFTER_REMOVE now", "used after remove")
	messages := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-remove").Content, "unknown tool",
		"a removed row must be absent from the next stack")
	assert.Equal(t, spawnsAfterRemove, fake.count(t, "spawn"), "removal must not restart the deleted server")
}

type registryModelCommand string

const (
	registryAdd     registryModelCommand = "add"
	registryRebuild registryModelCommand = "rebuild"
	registryDisable registryModelCommand = "disable"
	registryRelease registryModelCommand = "release"
	registryEnable  registryModelCommand = "enable"
	registryRemove  registryModelCommand = "remove"
	registryRestart registryModelCommand = "restart"
)

type registryReference struct {
	registered bool
	enabled    bool
	visible    bool
}

type registryModelHarness struct {
	*harness
	registry  mcpstore.Store
	workDir   string
	fake      *fakeMCPServer
	service   mcp.Service
	reference registryReference
}

func newRegistryModelHarness(t *testing.T) *registryModelHarness {
	t.Helper()
	h := newHarness(t, harnessOptions{})
	fake := newFakeMCPServer(t, "pong from model", false)
	return &registryModelHarness{harness: h, registry: h.mgr.mcpStore, workDir: h.workDir(), fake: fake}
}

func (h *registryModelHarness) apply(t *testing.T, command registryModelCommand) {
	t.Helper()
	ctx := context.Background()
	switch command {
	case registryAdd:
		err := h.registry.Add(ctx, &h.projectID, mcpstore.ServerDef{
			Name: "fake", Command: h.fake.path, Args: h.fake.args(), Enabled: true,
		})
		require.NoError(t, err)
		h.reference.registered = true
		h.reference.enabled = true
	case registryRebuild:
		h.rebuild(t)
	case registryDisable:
		require.NoError(t, h.registry.SetEnabled(ctx, &h.projectID, "fake", false))
		h.reference.enabled = false
	case registryRelease, registryRestart:
		if h.service != nil {
			h.service.Stop()
			h.service = nil
		}
		h.reference.visible = false
	case registryEnable:
		require.NoError(t, h.registry.SetEnabled(ctx, &h.projectID, "fake", true))
		h.reference.enabled = true
	case registryRemove:
		require.NoError(t, h.registry.Remove(ctx, &h.projectID, "fake"))
		h.reference.registered = false
		h.reference.enabled = false
	default:
		t.Fatalf("unknown registry model command %q", command)
	}
}

func (h *registryModelHarness) rebuild(t *testing.T) {
	t.Helper()
	defs, err := h.registry.ListForProject(context.Background(), h.projectID)
	require.NoError(t, err)
	configs := make(map[string]mcp.ServerConfig, len(defs))
	for _, def := range defs {
		configs[def.Name] = mcp.ServerConfig{Command: def.Command, Args: def.Args, Env: def.Env}
	}
	service, err := mcp.AcquireForWorkDir(context.Background(), configs, h.workDir, nil, nil)
	require.NoError(t, err)
	h.service = service
	actual := service != nil && service.Stats().Started == 1
	expected := h.reference.registered && h.reference.enabled
	assert.Equal(t, expected, actual, "real registry projection and stack acquire")
	h.reference.visible = expected
}

func (h *registryModelHarness) assertVisible(t *testing.T, want bool, message string) {
	t.Helper()
	assert.Equal(t, want, h.reference.visible, "reference model: "+message)
	actual := h.service != nil && h.service.Stats().Started == 1
	assert.Equal(t, want, actual, "real registry/stack state: "+message)
}

func (h *registryModelHarness) close() {
	if h.service != nil {
		h.service.Stop()
	}
	_ = h.db.Close()
}

func disableScenarioResponder(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return scriptTurns{
		{"USE_AFTER_RESTART", []scriptCall{{"ping-after-restart", "mcp__fake__ping", `{}`}}, "used after restart"},
		{"USE_IT", []scriptCall{{"ping-before-restart", "mcp__fake__ping", `{}`}}, "used before restart"},
		{"DISABLE_IT", []scriptCall{{"disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`}}, "disabled"},
		{"", []scriptCall{{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")}}, "registered"},
	}.respond
}

func removeScenarioResponder(
	t *testing.T,
	fake *fakeMCPServer,
) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()
	return scriptTurns{
		{"USE_AFTER_REMOVE", []scriptCall{{"ping-after-remove", "mcp__fake__ping", `{}`}}, "used after remove"},
		{"USE_IT", []scriptCall{{"ping-before-remove", "mcp__fake__ping", `{}`}}, "used before remove"},
		{"REMOVE_IT", []scriptCall{{"remove-1", tool.IDMCPRemove, `{"name":"fake","scope":"project"}`}}, "removed"},
		{"", []scriptCall{{"add-1", tool.IDMCPAdd, fake.addParams("fake", "project")}}, "registered"},
	}.respond
}
