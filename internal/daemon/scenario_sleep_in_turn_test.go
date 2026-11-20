package daemon

import (
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestScenario_ShortSleepRunsInsideTheTurn(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if toolResultForCallID(msgs, "short-sleep") != nil && toolResultForCallID(msgs, "after-sleep") != nil {
			return textReply("slept in turn")
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			{ID: "short-sleep", Name: tool.IDSleep, Arguments: []byte(`{"duration":"1s"}`)},
			{ID: "after-sleep", Name: "ls", Arguments: []byte(`{"path":"."}`)},
		}}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := h.collect()
	start := time.Now()
	root := h.send("sleep briefly then list files", nil)
	collector.waitMessage(root, "slept in turn")
	assert.GreaterOrEqual(t, time.Since(start), time.Second)
	collector.waitIdleAfter(root, "slept in turn")
	msgs := h.messages(root)
	result := toolResultForCallID(msgs, "short-sleep")
	require.NotNil(t, result)
	assert.Equal(t, "Sleep completed (1s).", result.Content)
	assert.False(t, result.ToolError)
	after := toolResultForCallID(msgs, "after-sleep")
	require.NotNil(t, after)
	assert.False(t, after.ToolError)
	schedules, err := h.schedules.ListSchedules(h.ctx, root)
	require.NoError(t, err)
	assert.Empty(t, schedules)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	for _, event := range collector.snapshot() {
		if event.SessionID != root || event.Notification.Type != sessionevent.NotifyWaiting {
			continue
		}
		for _, item := range event.Notification.Waiting {
			assert.NotEqual(t, sessionevent.WaitSleep, item.Kind)
		}
	}
}

func TestScenario_MessageDuringShortSleepFollowsItsResult(t *testing.T) {
	sleeping := make(chan struct{})
	var once sync.Once
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "message during sleep") {
			return textReply("saw message")
		}
		if toolResultForCallID(msgs, "short-sleep") != nil {
			return textReply("no message")
		}
		once.Do(func() { close(sleeping) })

		return callReply("short-sleep", tool.IDSleep, `{"duration":"2s"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	root := h.send("sleep briefly", nil)
	select {
	case <-sleeping:
	case <-time.After(5 * time.Second):
		t.Fatal("model did not request sleep")
	}
	h.say(root, "message during sleep")
	h.waitAnswer(root, "saw message")
	msgs := h.messages(root)
	result := toolResultForCallID(msgs, "short-sleep")
	require.NotNil(t, result)
	assert.Equal(t, "Sleep completed (2s).", result.Content)
	resultIndex, inputIndex, inputs := -1, -1, 0
	for i, msg := range msgs {
		if msg.Role == llmwire.RoleTool && msg.ToolCallID == "short-sleep" {
			resultIndex = i
		}
		if msg.Role == llmwire.RoleUser && strings.Contains(msg.Content, "message during sleep") {
			inputIndex = i
			inputs++
		}
	}
	assert.Equal(t, 1, inputs)
	require.GreaterOrEqual(t, resultIndex, 0)
	assert.Greater(t, inputIndex, resultIndex)
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSleep))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep))
}

func TestScenario_StopEndsShortSleepAtOnce(t *testing.T) {
	var modelCalls atomic.Int32
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)

		return callReply("short-sleep", tool.IDSleep, `{"duration":"10s"}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	root := h.send("sleep until stopped", nil)
	h.waitUntil("in-turn sleep running", func() bool {
		schedules, err := h.schedules.ListSchedules(h.ctx, root)

		return err == nil && len(schedules) == 0 && h.mgr.HasActiveLoop(root) &&
			countAssistantToolCallsFor(h.messages(root), tool.IDSleep) == 1
	})
	h.say(root, "/stop")
	h.waitUntil("root stopped", func() bool { return h.session(root).Status == sessionstore.SessionStatusStopped })
	msgs := h.messages(root)
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep))
	result := toolResultForCallID(msgs, "short-sleep")
	require.NotNil(t, result)
	assert.NotContains(t, result.Content, "Sleep completed")
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	schedules, err := h.schedules.ListSchedules(h.ctx, root)
	require.NoError(t, err)
	assert.Empty(t, schedules)
	assert.EqualValues(t, 1, modelCalls.Load())
}
