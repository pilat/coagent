package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	agentregistry "github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// The daemon registers task/schedule/sleep after the session object exists, so a
// prompt frozen at construction advertises a toolset the session does not have —
// and advertises subagents to a child that cannot spawn any.
func TestHarnessScenario_SystemPromptMatchesTheDaemonRegisteredToolset(t *testing.T) {
	const exploreCallID = "task-explore-prompt"

	prompts := newPromptRecorder()

	respond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_PROMPT") {
			prompts.record("child", system)

			return &llmwire.Response{Text: "child done"}
		}

		prompts.record("root", system)

		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{
			ToolCalls: []llmwire.ToolCall{spawnTaskCall(exploreCallID, "explore", "CHILD_PROMPT")},
		}
	}

	h := newGatingHarness(t, map[string]string{"reviewer.md": promptReviewerAgentFile}, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn an explore child", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForLink(parentID, exploreCallID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	root := prompts.first(t, "root")
	assert.Contains(t, root, "Sub-agents: task", "the inventory names the daemon-registered task tool")
	assert.Contains(t, root, "Scheduling: schedule")
	assert.Contains(t, root, "Never use sleep, schedule, or get_subagent_result polling to wait for subagents")
	assert.Contains(t, root, "## Available Subagents")
	assert.Contains(t, root, "**reviewer**")

	child := prompts.first(t, "child")
	assert.NotContains(t, child, "## Available Subagents", "explore has no task tool to spawn them with")
	assert.NotContains(t, child, "# SCHEDULING")
	assert.NotContains(t, child, "Sub-agents: task")
}

func TestHarnessScenario_ActiveProcessPromptAndSleepGuard(t *testing.T) {
	prompts := newPromptRecorder()
	var requestMessages []llmwire.Message
	var requestMu sync.Mutex
	respond := func(system string, messages []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)
		requestMu.Lock()
		if requestMessages == nil {
			requestMessages = append([]llmwire.Message(nil), messages...)
		}
		requestMu.Unlock()
		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "background process polling rejected"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "poll-process", Name: tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"poll background process"}`),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
	})
	require.NoError(t, err)
	now := time.Now().UTC()
	require.NoError(t, h.mgr.processStore.InsertProcess(context.Background(), backgroundprocess.Process{
		ID: "bgp_prompt_guard", SessionID: root.ID, RootSessionID: root.ID,
		ToolCallID: "background-bash", OutputPath: filepath.Join(t.TempDir(), "process.out"),
		Deadline: now.Add(time.Hour), CreatedAt: now, AdvertisedAt: &now, State: backgroundprocess.StateRunning,
	}))
	t.Cleanup(func() {
		_, _, _ = h.mgr.processStore.FinalizeWithIntent(
			context.Background(), "bgp_prompt_guard", backgroundprocess.IntentSessionKilled, 0,
		)
	})

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "wait for the process"))
	waitForVisibleMessage(t, collector, root.ID, "background process polling rejected")
	_, _, err = h.mgr.processStore.FinalizeWithIntent(
		context.Background(), "bgp_prompt_guard", backgroundprocess.IntentSessionKilled, 0,
	)
	require.NoError(t, err)

	prompt := prompts.first(t, "root")
	assert.NotContains(t, prompt, "# Active background work")
	requestMu.Lock()
	firstRequest := append([]llmwire.Message(nil), requestMessages...)
	requestMu.Unlock()
	require.Equal(t, 1, countMessageContentContaining(firstRequest, "# Active background work"))
	assert.True(t, hasUserContaining(firstRequest, "process bgp_prompt_guard (running)"))
	assert.True(t, hasUserContaining(firstRequest, "Snapshot from activation start"))

	messages := h.parentMessages(root.ID)
	require.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "sleep is unavailable")
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "end the response")
	assert.Equal(t, llmwire.RoleAssistant, messages[len(messages)-1].Role)

	schedules, err := h.schedules.ListSchedules(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}

func TestHarnessScenario_EmptyActiveBackgroundAddsNoProviderRow(t *testing.T) {
	var requestMessages []llmwire.Message
	var requestMu sync.Mutex
	respond := func(system string, messages []llmwire.Message) *llmwire.Response {
		assert.NotContains(t, system, "# Active background work")
		requestMu.Lock()
		requestMessages = append([]llmwire.Message(nil), messages...)
		requestMu.Unlock()

		return &llmwire.Response{Text: "done"}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "ordinary task", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(root)

	requestMu.Lock()
	recorded := append([]llmwire.Message(nil), requestMessages...)
	requestMu.Unlock()
	require.NotEmpty(t, recorded)
	assert.Zero(t, countMessageContentContaining(recorded, "# Active background work"))
}

func TestHarnessScenario_DynamicRegistryPromptMatchesEachActivation(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from registry", false)
	wrapped := newRegistryPromptHarness(t, registryPromptRespond(fake))
	h, schemas, prompts := wrapped.harness, wrapped.schemas, wrapped.prompts
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "exercise dynamic registry", "fake-model", map[string]any{
		"channel": "cli",
	})
	require.NoError(t, err)

	link := h.waitForLinkByCall(parentID, "task-registry")
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	assertInitialRegistryProjection(t, h, schemas, prompts, parentID, link.ChildID)
	assert.Contains(t, lastToolResultContent(h.parentMessages(parentID), "mcp__fake__ping"), "unknown tool")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, registryUseMarker))
	h.mgr.waitIdle(parentID)

	assertNextRegistryProjection(t, h, schemas, prompts, parentID)
}

// A root session is the primary build agent. When the store lets the schema
// default decide the agent type, the root silently runs as the "general"
// subagent: it is told it is a subagent and loses the todo tools.
func TestHarnessScenario_RootSessionRunsAsTheBuildAgent(t *testing.T) {
	prompts := newPromptRecorder()

	respond := func(system string, _ []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)

		return &llmwire.Response{Text: "done"}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "do the thing", "fake-model", nil)
	require.NoError(t, err)

	h.mgr.waitIdle(rootID)

	system := prompts.first(t, "root")
	assert.True(t, strings.HasPrefix(system, agentregistry.BuildAgentPrompt),
		"root must open with the primary build prompt, got: %s", firstLine(system))
	assert.NotContains(t, system, "You are a subagent", "root is nobody's subagent")

	offered := h.schemas.offered(rootID)
	assert.Contains(t, offered, "todoread", "todo tools belong to the primary agent")
	assert.Contains(t, offered, "todowrite")

	rec, err := h.store.GetSession(h.ctx, rootID)
	require.NoError(t, err)
	assert.Equal(t, string(agentregistry.AgentTypeBuild), rec.AgentType,
		"the root row names the agent it runs")
}

// The wrapper and the guidance are checked against the real daemon/session
// stack: registry → session formatting → durable transcript → next model
// input → next request messages. webfetch on a local httptest server keeps the
// scenario hermetic; the scripted model fetches the local page once, then
// finishes.
func TestHarnessScenario_UntrustedToolOutputCarriesWrapperAndGuidance(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(untrustedProbeMarker + " page body"))
	}))
	t.Cleanup(page.Close)

	prompts := newPromptRecorder()
	requests := &requestRecorder{}

	respond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)
		requests.record(msgs)

		if hasToolResultFor(msgs, "webfetch") {
			return &llmwire.Response{Text: "untrusted probe done"}
		}

		args, _ := json.Marshal(map[string]string{"url": page.URL})

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "fetch-1", Name: "webfetch", Arguments: args,
		}}}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "probe untrusted output", "fake-model", nil)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		return strings.Contains(lastToolResultContent(h.parentMessages(parentID), "webfetch"),
			untrustedProbeMarker)
	}, 10*time.Second, 20*time.Millisecond, "the webfetch result must reach the transcript")
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "transcript must stay provider-valid")

	content := lastToolResultContent(msgs, "webfetch")
	markers := regexp.MustCompile(`<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="([0-9a-f]{16})">>>`).FindStringSubmatch(content)
	require.Len(t, markers, 2)
	assert.True(t, strings.HasSuffix(content, `<<<END_UNTRUSTED_EXTERNAL_DATA id="`+markers[1]+`">>>`))
	assert.Contains(t, content, untrustedProbeMarker)

	// The next model request re-derives its messages from the durable
	// transcript: the persisted wrapper must survive into that request too.
	requestMsgs := requests.lastMessages(t)
	requestContent := lastToolResultContent(requestMsgs, "webfetch")
	assert.Equal(t, content, requestContent)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue the probe"))
	require.Eventually(t, func() bool {
		return hasUserContaining(requests.lastMessages(t), "continue the probe")
	}, 10*time.Second, 20*time.Millisecond, "the next activation must reach the model")
	h.mgr.waitIdle(parentID)
	assert.Equal(t, content, lastToolResultContent(requests.lastMessages(t), "webfetch"))
	assert.Equal(t, content, lastToolResultContent(h.parentMessages(parentID), "webfetch"))

	// The system prompt carries the matching dynamic guidance for the
	// registered toolset.
	prompt := prompts.last(t, "root")
	assert.Contains(t, prompt, "# UNTRUSTED CONTENT")
	assert.Contains(t, prompt, "curl")
	assert.Contains(t, prompt, "wget")
	assert.Contains(t, prompt, `<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="...">>>`)
	assert.Contains(t, prompt, "same ID")
}

func TestHarnessScenario_HelpIncludesGWT(t *testing.T) {
	var modelCalls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)

		return &llmwire.Response{Text: "session ready"}
	}})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "open session", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
		"channel":                               "telegram",
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, sessionID, "session ready")
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/help"))
	waitForVisibleMessage(t, collector, sessionID, helpWithGWT)

	controller := newChainController(t, h)
	drainScenarioClaims(t, "help_includes_gwt.json", controller)
	waitForIdleAfterMessage(t, collector, sessionID, helpWithGWT)

	// The opening turn runs the two-phase check: candidate response, then the
	// confirmation call after the host nudge. /help itself must not invoke
	// the model.
	assert.Equal(t, int64(2), modelCalls.Load(), "/help must not invoke the model")
	assertHarnessTrace(t, "help_includes_gwt.json", collector.snapshot(), sessionID)
}
