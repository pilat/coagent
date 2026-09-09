package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

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

	h := newGatingHarness(t, false, map[string]string{"reviewer.md": promptReviewerAgentFile}, respond)
	defer h.shutdown()

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
	respond := func(system string, messages []llmwire.Message) *llmwire.Response {
		prompts.record("root", system)
		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "background process polling rejected"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "poll-process", Name: tool.IDSleep,
			Arguments: []byte(`{"duration":"1h","reason":"poll background process"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
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

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "wait for the process"))
	waitForVisibleMessage(t, collector, root.ID, "background process polling rejected")
	_, _, err = h.mgr.processStore.FinalizeWithIntent(
		context.Background(), "bgp_prompt_guard", backgroundprocess.IntentSessionKilled, 0,
	)
	require.NoError(t, err)

	prompt := prompts.first(t, "root")
	assert.Contains(t, prompt, "# Active background work")
	assert.Contains(t, prompt, "process bgp_prompt_guard (running)")
	assert.Contains(t, prompt, "<WAITING/>")

	messages := h.parentMessages(root.ID)
	require.Equal(t, 1, countToolResultsFor(messages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "sleep is unavailable")
	assert.Contains(t, lastToolResultContent(messages, tool.IDSleep), "<WAITING/>")
	assert.Equal(t, llmwire.RoleAssistant, messages[len(messages)-1].Role)

	schedules, err := h.schedStore.ListSchedules(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}
