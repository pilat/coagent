package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
)

func TestHarnessScenario_OwnedRootBecomesIdleWithoutOutputAcknowledgement(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		return textReply("finished without delivery")
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "finish", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	collector.waitMessage(root, "finished without delivery")
	collector.waitIdleAfter(root, "finished without delivery")
	assert.False(t, h.mgr.HasActiveLoop(root))
	var pending int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message'`, root).Scan(&pending))
	assert.Equal(t, 1, pending)
	var idleCount int
	for _, event := range collector.snapshot() {
		if event.SessionID == root && event.Notification.Type == sessionevent.NotifyStateChanged &&
			event.Notification.Status == controllerapi.StateIdle {
			idleCount++
		}
	}
	assert.Equal(t, 1, idleCount)
}
