package daemon

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantFreshHeadIncludesPreviousExchange(t *testing.T) {
	var checked atomic.Bool
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if !strings.HasPrefix(system, registry.AssistantPrompt) {
			return textReply("work answer")
		}
		if hasUserContaining(messages, "second exchange") {
			if checked.CompareAndSwap(false, true) {
				assert.Len(t, messages, 2)
				assert.Contains(t, messages[0].Content, "first exchange")
				assert.Contains(t, messages[0].Content, "first answer")
				for _, message := range messages {
					assert.NotEqual(t, llmwire.RoleTool, message.Role)
					assert.Empty(t, message.ToolCalls)
				}
			}
			return textReply("second answer")
		}
		if !hasToolResultFor(messages, "projects") {
			return assistantCalls("", llmwire.ToolCall{ID: "projects-1", Name: "projects", Arguments: []byte(`{}`)})
		}
		return textReply("first answer")
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	assert.Equal(t, "fake-model", root.Model)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "first exchange"))
	h.waitUntil("first assistant activation", func() bool {
		return assistantSayCount(t, h, root.ID, "first answer") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "second exchange"))
	h.waitUntil("second assistant activation", func() bool {
		return assistantSayCount(t, h, root.ID, "second answer") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	assert.True(t, checked.Load())
	var compacted int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ? AND role = 'tool' AND compacted_at IS NOT NULL`, root.ID).
			Scan(&compacted),
	)
	assert.Equal(t, 1, compacted)
}

func TestAssistantErrorRootWakesForAgentInput(t *testing.T) {
	h := newHarness(
		t,
		harnessOptions{
			respond: func(string, []llmwire.Message) *llmwire.Response { return textReply("recovered answer") },
		},
	)
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, root.ID, sessionstore.SessionStatusError))
	_, err = h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: root.ID, Source: sessionstore.InputSourceAgent, Content: "work answered"},
	)
	require.NoError(t, err)
	require.NoError(t, h.mgr.inputReady(h.ctx, root.ID))
	h.waitUntil("errored assistant recovers", func() bool {
		return assistantSayCount(t, h, root.ID, "recovered answer") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	commandRoot, err := h.mgr.EnsureAssistantRoot(h.ctx, "command-only-owner")
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, commandRoot.ID, sessionstore.SessionStatusError))
	_, err = h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: commandRoot.ID, Source: sessionstore.InputSourceUser, Content: "/status"},
	)
	require.NoError(t, err)
	require.NoError(t, h.mgr.inputReady(h.ctx, commandRoot.ID))
	assert.False(t, h.mgr.HasActiveLoop(commandRoot.ID))
	record, err := h.store.GetSession(h.ctx, commandRoot.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusError, record.Status)
}
