package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// The plan's command admission matrix accepts read-only boundary commands on a
// stopped root and processes them while preserving the stopped status. A
// stopped root must therefore answer /status without being reactivated.
func TestScenario_StoppedRootAnswersStatusWithoutReactivating(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "what happened?") {
			return &llmwire.Response{Text: "fresh response"}
		}
		if hasAssistantToolCall(msgs, "bash") {
			return &llmwire.Response{Text: "old work was interrupted"}
		}
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: "long-bash", Name: "bash", Arguments: []byte(`{"command":"sleep 30"}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "watch checks", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("ordinary bash started", func() bool {
		return countAssistantToolCallsFor(h.parentMessages(sessionID), "bash") == 1 &&
			h.mgr.HasActiveLoop(sessionID)
	})

	require.NoError(t, h.mgr.Stop(h.ctx, sessionID))
	h.waitUntil("stop completed", func() bool {
		rec, getErr := h.sessStore.GetSession(h.ctx, sessionID)
		return getErr == nil && rec.Status == sessionstore.SessionStatusStopped
	})

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/status"))

	collector.waitFor(t, "status report reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, sessionID)) == 1
	})
	h.mgr.waitIdle(sessionID)

	assert.Len(t, statusReports(collector.snapshot(), sessionID), 1, "the stopped root answers /status")

	rec, err := h.sessStore.GetSession(h.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, rec.Status,
		"a read-only command must not reactivate the stopped root")

	idle := lastIdleStatus(collector.snapshot(), sessionID)
	if idle != nil {
		assert.Empty(t, idle.Reason, "read-only processing must not be mistaken for a lifecycle stop")
	}
}

func lastIdleStatus(events []controllerapi.SessionNotification, sessionID int64) *sessionevent.Notification {
	for _, event := range events {
		if event.SessionID == sessionID && event.Notification.Type == sessionevent.NotifyStateChanged &&
			event.Notification.Status == controllerapi.StateIdle {
			n := event.Notification
			return &n
		}
	}

	return nil
}
