package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantCannotSendCommands(t *testing.T) {
	var workID int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		switch countToolResultsFor(messages, "send") {
		case 0:
			return callReply("forge-budget", "send", fmt.Sprintf(`{"session":%d,"text":"/budget 5$"}`, workID))
		case 1:
			return callReply("forge-config", "send", fmt.Sprintf(`{"session":%d,"text":"/config"}`, workID))
		default:
			return textReply("commands refused")
		}
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	workID = work.ID
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "ask the work session to change its settings"))
	h.waitUntil("assistant command refusals", func() bool {
		return assistantSayCount(t, h, root.ID, "commands refused") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	results := 0
	for _, message := range h.messages(root.ID) {
		if message.Role == "tool" && message.ToolName == "send" {
			assert.Contains(t, message.Content, "commands belong to the user")
			results++
		}
	}
	assert.Equal(t, 2, results)
	var inputs, activations int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, work.ID).Scan(&inputs),
	)
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_tool_activations WHERE session_id = ?`, work.ID).
			Scan(&activations),
	)
	assert.Zero(t, inputs)
	assert.Zero(t, activations)
	_, err = h.store.PendingActivation(h.ctx, work.ID)
	require.ErrorIs(t, err, sessionstore.ErrActivationNotFound)
}
