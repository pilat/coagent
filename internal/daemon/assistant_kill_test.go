package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantKillsWorkOnRequest(t *testing.T) {
	var workID int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "kill") {
			assert.Equal(t, fmt.Sprintf("deleted session %d", workID), configLastToolResultContent(messages, "kill"))
			return textReply("Work session deleted.")
		}
		return callReply("delete-work", "kill", fmt.Sprintf(`{"session":%d}`, workID))
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	workID = work.ID
	require.NoError(t, h.store.BindChat(h.ctx, scenarioManagerID, "chat"))
	before, err := h.store.ChatSessions(h.ctx, scenarioManagerID, "chat")
	require.NoError(t, err)
	require.Len(t, before, 2)
	assert.Equal(t, work.ID, before[1].ID)
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, fmt.Sprintf("Delete work session #%d.", work.ID)))
	h.waitUntil("assistant confirms work deletion", func() bool {
		return assistantSayCount(t, h, root.ID, "Work session deleted.") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	deleted, err := h.store.GetSession(h.ctx, work.ID)
	require.NoError(t, err)
	assert.NotNil(t, deleted.KilledAt)
	assert.Equal(t, sessionstore.SessionStatusKilled, deleted.Status)
	after, err := h.store.ChatSessions(h.ctx, scenarioManagerID, "chat")
	require.NoError(t, err)
	require.Len(t, after, 1)
	assert.Zero(t, after[0].ID)
	alive, err := h.store.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Nil(t, alive.KilledAt)
	assert.Equal(t, 1, countAssistantToolCallsFor(h.messages(root.ID), "kill"))
	assert.Equal(t, 1, countToolResultsFor(h.messages(root.ID), "kill"))
}
