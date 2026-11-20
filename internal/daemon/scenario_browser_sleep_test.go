package daemon

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/tool"
)

func TestScenario_BrowserGrantedSleepWaitsInTurnAndSuspendsBeyondTheLimit(t *testing.T) {
	server, log := newBrowserMCPServer(t)
	var mu sync.Mutex
	var t0, lookAt time.Time
	childRespond := func(msgs []llmwire.Message) *llmwire.Response {
		if toolResultForCallID(msgs, "long-pause") != nil {
			return textReply("browser complete")
		}
		if toolResultForCallID(msgs, "second-surface") != nil {
			return callReply("long-pause", tool.IDSleep, `{"duration":"1h"}`)
		}
		if toolResultForCallID(msgs, "look-after-pause") != nil {
			info, err := os.Stat(log)
			if assert.NoError(t, err) {
				mu.Lock()
				lookAt = info.ModTime()
				mu.Unlock()
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
				{ID: "one-surface", Name: "mcp__remote__" + "view", Arguments: []byte(`{"task_state":"act"}`)},
				{ID: "between", Name: tool.IDSleep, Arguments: []byte(`{"duration":"1ms"}`)},
				{
					ID:        "second-surface",
					Name:      "mcp__remote__" + "view",
					Arguments: []byte(`{"task_state":"act again"}`),
				},
			}}
		}
		mu.Lock()
		t0 = time.Now()
		mu.Unlock()

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			{ID: "short-pause", Name: tool.IDSleep, Arguments: []byte(`{"duration":"1s"}`)},
			{
				ID:        "look-after-pause",
				Name:      "mcp__remote__" + "view",
				Arguments: []byte(`{"task_state":"look again after the pause"}`),
			},
		}}
	}
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "BROWSER_SLEEP") {
			return childRespond(msgs)
		}
		if toolResultForCallID(msgs, "browser-task") != nil {
			return textReply("parent received browser result")
		}

		return callReply("browser-task", tool.IDTask,
			`{"prompt":"BROWSER_SLEEP inspect example.test","description":"inspect site","subagent_type":"browser"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond, configure: func(cfg *config.Config) {
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Browser: &config.BrowserConfig{MCPName: "remote", Tools: []string{"sleep"}},
		}
	}})
	require.NoError(t, h.mgr.mcpStore.Add(h.ctx, &h.projectID, mcpstore.ServerDef{
		Name: "remote", Command: server, Args: []string{log}, Enabled: true,
	}))
	rootID := h.send("inspect site", nil)
	childID := h.waitLink(rootID, "browser-task").ChildID
	h.waitUntil("browser durable sleep", func() bool {
		schedules, err := h.schedules.ListSchedules(h.ctx, childID)

		return err == nil && len(schedules) == 1 && !h.mgr.HasActiveLoop(childID)
	})
	link := h.link(childID)
	require.NotNil(t, link)
	assert.True(t, link.Blocking)
	assert.False(t, link.Terminal())
	assert.Nil(t, toolResultForCallID(h.messages(rootID), "browser-task"))
	msgs := h.messages(childID)
	short := toolResultForCallID(msgs, "short-pause")
	require.NotNil(t, short)
	assert.Equal(t, "Sleep completed (1s).", short.Content)
	look := toolResultForCallID(msgs, "look-after-pause")
	require.NotNil(t, look)
	assert.False(t, look.ToolError)
	mu.Lock()
	elapsed := lookAt.Sub(t0)
	mu.Unlock()
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	one := toolResultForCallID(msgs, "one-surface")
	require.NotNil(t, one)
	assert.False(t, one.ToolError)
	between := toolResultForCallID(msgs, "between")
	require.NotNil(t, between)
	assert.Equal(t, "Sleep completed (1ms).", between.Content)
	second := toolResultForCallID(msgs, "second-surface")
	require.NotNil(t, second)
	assert.True(t, second.ToolError)
	assert.Contains(t, second.Content, "Only one surface action")
	data, err := os.ReadFile(log)
	require.NoError(t, err)
	assert.Equal(t, 2, strings.Count(string(data), "call\n"))
	_, err = h.db.ExecContext(h.ctx, "UPDATE schedules SET one_shot_at = ? WHERE session_id = ?",
		time.Now().Add(-time.Second).UTC(), childID)
	require.NoError(t, err)
	executor := schedule.NewExecutor(h.schedules, h.mgr)
	executor.Start(h.ctx)
	defer executor.Stop()
	h.waitAnswer(rootID, "parent received browser result")
	msgs = h.messages(childID)
	long := toolResultForCallID(msgs, "long-pause")
	require.NotNil(t, long)
	assert.Contains(t, long.Content, "Sleep completed (1h)")
	assert.Equal(t, "browser complete", lastAssistantTextDTO(msgs))
	parentResult := toolResultForCallID(h.messages(rootID), "browser-task")
	require.NotNil(t, parentResult)
	assert.Contains(t, parentResult.Content, "browser complete")
	schedules, err := h.schedules.ListSchedules(h.ctx, childID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	require.NoError(t, llmtest.ValidateToolPairing(h.messages(rootID)))
}
