package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	agentregistry "github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// Post-construction tool registration must reach the live prompt, without advertising child-forbidden subagent tools.
func TestHarnessScenario_SystemPromptMatchesTheDaemonRegisteredToolset(t *testing.T) {
	const exploreCallID = "task-explore-prompt"
	prompts := newPromptRecorder()
	respond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_PROMPT") {
			prompts.record("child", system)
			return textReply("child done")
		}
		prompts.record("root", system)
		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return textReply("parent done")
		}
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{spawnTaskCall(exploreCallID, "explore", "CHILD_PROMPT")}}
	}
	h := newGatingHarness(t, map[string]string{"reviewer.md": promptReviewerAgentFile}, respond)
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn an explore child", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, exploreCallID) != nil })
	link := *h.linkByCall(parentID, exploreCallID)
	h.waitUntil(
		"child delivery",
		func() bool { link := h.link(link.ChildID); return link != nil && link.DeliveredAt != 0 },
	)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
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
			return textReply("background process polling rejected")
		}
		return callReply("poll-process", tool.IDSleep, `{"duration":"1h","reason":"poll background process"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	var err error
	root := h.createRoot(managerAttrs(scenarioManagerID))
	now := time.Now().UTC()
	require.NoError(t, h.mgr.processStore.InsertProcess(context.Background(), backgroundprocess.Process{
		ID: "bgp_prompt_guard", SessionID: root, RootSessionID: root,
		ToolCallID: "background-bash", OutputPath: filepath.Join(t.TempDir(), "process.out"),
		Deadline: now.Add(time.Hour), CreatedAt: now, AdvertisedAt: &now, State: backgroundprocess.StateRunning,
	}))
	t.Cleanup(func() {
		_, _, _ = h.mgr.processStore.FinalizeWithIntent(
			context.Background(), "bgp_prompt_guard", backgroundprocess.IntentSessionKilled, 0,
		)
	})
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "wait for the process"))
	collector.waitMessage(root, "background process polling rejected")
	_, _, err = h.mgr.processStore.FinalizeWithIntent(
		context.Background(), "bgp_prompt_guard", backgroundprocess.IntentSessionKilled, 0,
	)
	require.NoError(t, err)
	prompt := prompts.first(t, "root")
	assert.NotContains(t, prompt, "# Active background work")
	requestMu.Lock()
	firstRequest := append([]llmwire.Message(nil), requestMessages...)
	requestMu.Unlock()
	matchingMessages := 0
	for _, message := range firstRequest {
		if strings.Contains(message.Content, "# Active background work") {
			matchingMessages++
		}
	}
	require.Equal(t, 1, matchingMessages)
	assert.True(t, hasUserContaining(firstRequest, "process bgp_prompt_guard (running)"))
	assert.True(t, hasUserContaining(firstRequest, "Snapshot from activation start"))
	messages := h.messages(root)
	require.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, promptLastToolResultContent(messages, tool.IDSleep), "sleep is unavailable")
	assert.Contains(t, promptLastToolResultContent(messages, tool.IDSleep), "end the response")
	assert.Equal(t, llmwire.RoleAssistant, messages[len(messages)-1].Role)
	schedules, err := h.schedules.ListSchedules(h.ctx, root)
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
		return textReply("done")
	}
	h := newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "ordinary task", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(root) })
	requestMu.Lock()
	recorded := append([]llmwire.Message(nil), requestMessages...)
	requestMu.Unlock()
	require.NotEmpty(t, recorded)
	matchingMessages := 0
	for _, message := range recorded {
		if strings.Contains(message.Content, "# Active background work") {
			matchingMessages++
		}
	}
	assert.Zero(t, matchingMessages)
}

func TestHarnessScenario_DynamicRegistryPromptMatchesEachActivation(t *testing.T) {
	fake := newFakeMCPServer(t, "pong from registry", false)
	wrapped := newRegistryPromptHarness(t, registryPromptRespond(fake))
	h, schemas, prompts := wrapped.harness, wrapped.schemas, wrapped.prompts
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "exercise dynamic registry", "fake-model", map[string]any{
		"channel": "cli",
	})
	require.NoError(t, err)
	h.waitUntil("child link", func() bool { return h.linkByCall(parentID, "task-registry") != nil })
	link := *h.linkByCall(parentID, "task-registry")
	h.waitUntil(
		"child delivery",
		func() bool { link := h.link(link.ChildID); return link != nil && link.DeliveredAt != 0 },
	)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	assertInitialRegistryProjection(t, h, schemas, prompts, parentID, link.ChildID)
	assert.Contains(t, promptLastToolResultContent(h.messages(parentID), "mcp__fake__ping"), "unknown tool")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, registryUseMarker))
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	assertNextRegistryProjection(t, h, schemas, prompts, parentID)
}

// A root must remain the primary build agent; schema defaults cannot turn it into a general child without todo tools.
func TestHarnessScenario_RootSessionRunsAsTheBuildAgent(t *testing.T) {
	prompts := newPromptRecorder()
	respond := func(system string, _ []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)
		return textReply("done")
	}
	h := newGatingHarness(t, nil, respond)
	h.startInboxWake()
	rootID, err := h.mgr.Send(h.ctx, h.projectID, "do the thing", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(rootID) })
	system := prompts.first(t, "root")
	assert.True(t, strings.HasPrefix(system, agentregistry.BuildAgentPrompt),
		"root must open with the primary build prompt, got: %s", firstLine(system))
	assert.NotContains(t, system, "You are a subagent", "root is nobody's subagent")
	offered := h.schemas.offered(rootID)
	assert.Contains(t, offered, "todoread", "todo tools belong to the primary agent")
	assert.Contains(t, offered, "todowrite")
	rec := h.session(rootID)
	assert.Equal(t, string(agentregistry.AgentTypeBuild), rec.AgentType, "the root row names the agent it runs")
}

// The real registry-to-transcript-to-provider path preserves webfetch guidance; a local HTTP fixture keeps it hermetic.
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
			return textReply("untrusted probe done")
		}
		args, _ := json.Marshal(map[string]string{"url": page.URL})
		return callReply("fetch-1", "webfetch", string(args))
	}
	h := newGatingHarness(t, nil, respond)
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "probe untrusted output", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("webfetch result stored", func() bool {
		return strings.Contains(promptLastToolResultContent(h.messages(parentID), "webfetch"), untrustedProbeMarker)
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	msgs := h.messages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "transcript must stay provider-valid")
	content := promptLastToolResultContent(msgs, "webfetch")
	markers := regexp.MustCompile(`<<<BEGIN_UNTRUSTED_EXTERNAL_DATA id="([0-9a-f]{16})">>>`).FindStringSubmatch(content)
	require.Len(t, markers, 2)
	assert.True(t, strings.HasSuffix(content, `<<<END_UNTRUSTED_EXTERNAL_DATA id="`+markers[1]+`">>>`))
	assert.Contains(t, content, untrustedProbeMarker)

	// The next model request re-derives its messages from the durable
	// transcript: the persisted wrapper must survive into that request too.
	requestMsgs := requests.lastMessages(t)
	requestContent := promptLastToolResultContent(requestMsgs, "webfetch")
	assert.Equal(t, content, requestContent)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue the probe"))
	h.waitUntil("next model activation", func() bool {
		return hasUserContaining(requests.lastMessages(t), "continue the probe")
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	assert.Equal(t, content, promptLastToolResultContent(requests.lastMessages(t), "webfetch"))
	assert.Equal(t, content, promptLastToolResultContent(h.messages(parentID), "webfetch"))

	// The system prompt carries the matching dynamic guidance for the registered toolset.
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
		return textReply("session ready")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "open session", "fake-model", map[string]any{
		controllerapi.SessionAttributeManagerID: scenarioManagerID,
		"channel":                               "telegram",
	})
	require.NoError(t, err)
	collector.waitMessage(sessionID, "session ready")
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/help"))
	collector.waitMessage(sessionID, helpWithGWT)
	controller := newChainController(t, h)
	drainScenarioClaims(t, "help_includes_gwt.json", controller)
	collector.waitIdleAfter(sessionID, helpWithGWT)

	// The opener completes its candidate/confirmation pair; help itself must never invoke the model.
	assert.Equal(t, int64(2), modelCalls.Load(), "/help must not invoke the model")
	assertHarnessTrace(t, "help_includes_gwt.json", collector.snapshot(), sessionID)
}

func promptLastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

const promptReviewerAgentFile = `---
name: reviewer
description: Reviews changes before they ship
---
You are the project reviewer.
`

// promptRecorder keeps the system prompts each role's session was handed, in order.
type promptRecorder struct {
	mu     sync.Mutex
	byRole map[string][]string
}

func newPromptRecorder() *promptRecorder {
	return &promptRecorder{byRole: make(map[string][]string)}
}

func (r *promptRecorder) record(role, system string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byRole[role] = append(r.byRole[role], system)
}

func (r *promptRecorder) first(t *testing.T, role string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byRole[role], "no %s request was recorded", role)
	return r.byRole[role][0]
}

const (
	registryChildMarker = "CHILD_REGISTRY"
	registryUseMarker   = "USE_REGISTRY_MCP"
)

func registryPromptRespond(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, registryChildMarker) {
			return textReply("child complete")
		}
		if hasUserContaining(messages, registryUseMarker) {
			if toolResultForCallID(messages, "ping-next-activation") != nil {
				return textReply("mcp complete")
			}
			return mcpPingCall("ping-next-activation")
		}
		if hasToolResultFor(messages, tool.IDMCPAdd) {
			if toolResultForCallID(messages, "ping-same-activation") != nil {
				return textReply("registered")
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

func assertInitialRegistryProjection(
	t *testing.T,
	h *harness,
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
	for _, id := range []string{tool.IDTask, tool.IDSchedule, tool.IDSleep, tool.IDMCPAdd, tool.IDConfigEdit} {
		assert.NotContains(t, childSchemas, id, "child registry must gate %q", id)
	}
	childPrompt := prompts.first(t, strconv.FormatInt(childID, 10))
	assert.NotContains(t, childPrompt, "Sub-agents: task")
	assert.NotContains(t, childPrompt, "# SCHEDULING")
}

func assertNextRegistryProjection(
	t *testing.T,
	h *harness,
	schemas *activationSchemas,
	prompts *promptRecorder,
	parentID int64,
) {
	t.Helper()
	lastSchemas := schemas.last(t, parentID)
	assert.Contains(t, lastSchemas, "mcp__fake__ping")
	assert.Contains(t, lastSchemas, tool.IDTask)
	assert.Contains(t, lastSchemas, tool.IDConfigEdit)
	assert.Contains(t, toolResultForCallID(h.messages(parentID), "ping-next-activation").Content, "pong from registry")
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
	return []string{tool.IDConfigEdit}
}

// activationSchemas stores each request's inventory separately. A union would
// hide a stale registry that survived into a later activation.
type activationSchemas struct {
	mu   sync.Mutex
	byID map[int64][][]string
}

func (r *activationSchemas) record(sessionID int64, schemas []llmwire.ToolSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		ids = append(ids, schema.Name)
	}
	r.byID[sessionID] = append(r.byID[sessionID], ids)
}

func (r *activationSchemas) first(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)
	return append([]string(nil), r.byID[sessionID][0]...)
}

func (r *activationSchemas) last(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)
	requests := r.byID[sessionID]
	return append([]string(nil), requests[len(requests)-1]...)
}

func (r *promptRecorder) last(t *testing.T, role string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byRole[role], "no %s request was recorded", role)
	requests := r.byRole[role]
	return requests[len(requests)-1]
}

type registryPromptHarness struct {
	*harness
	schemas *activationSchemas
	prompts *promptRecorder
}

func newRegistryPromptHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *registryPromptHarness {
	t.Helper()
	recorder := &activationSchemas{byID: make(map[int64][][]string)}
	prompts := newPromptRecorder()
	h := newHarness(t, harnessOptions{clientFor: func(*config.Config) (llm.Client, error) {
		return &registryPromptLLM{respond: respond, recorder: recorder, prompts: prompts}, nil
	}})
	h.mgr.applier = configapply.New(newTestConfigOps(t, t.TempDir()), h.store)
	return &registryPromptHarness{harness: h, schemas: recorder, prompts: prompts}
}

func firstLine(s string) string {
	head, _, _ := strings.Cut(s, "\n")
	return head
}

const untrustedProbeMarker = "PROBE_UNTRUSTED"

// requestRecorder keeps each request's full message transcript, in order.
type requestRecorder struct {
	mu   sync.Mutex
	msgs [][]llmwire.Message
}

func (r *requestRecorder) record(msgs []llmwire.Message) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, msgs)
}

func (r *requestRecorder) lastMessages(t *testing.T) []llmwire.Message {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.msgs, "no scripted model request was recorded")
	return r.msgs[len(r.msgs)-1]
}

const helpWithGWT = "## Session commands\n" +
	"`/status` — show session status\n" +
	"`/stop` — stop the current run\n" +
	"`/clear` — start a fresh session\n" +
	"`/kill` — close this session\n" +
	"`/compact [focus]` — compact the context\n" +
	"`/schedules` — list schedules\n" +
	"`/budget <request>` — arm, replace, inspect, or clear a one-shot cost/wall-time checkpoint\n" +
	"`/gwt <name>` — fork into a worktree (Telegram session topics only)"

type registryPromptLLM struct {
	scriptedLLM
	recorder *activationSchemas
	prompts  *promptRecorder

	mu        sync.Mutex
	sessionID int64
}

func (c *registryPromptLLM) SetSessionID(id string) {
	if index := strings.LastIndex(id, ":"); index >= 0 {
		id = id[index+1:]
	}
	parsed, _ := strconv.ParseInt(id, 10, 64)
	c.mu.Lock()
	c.sessionID = parsed
	c.mu.Unlock()
}

func (c *registryPromptLLM) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()
	c.prompts.record(strconv.FormatInt(sessionID, 10), system)
	c.recorder.record(sessionID, tools)
	return c.scriptedLLM.Chat(ctx, system, messages, tools, opts...)
}
