package daemon

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_SentMessageWakesStoppedRootAtRuntime(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "please continue") {
			return textReply("woken")
		}
		return textReply("ok")
	}})
	h.startInboxWake()
	id := h.send("init", nil)
	h.waitAnswer(id, "ok")
	h.say(id, "/stop")
	h.waitUntil("stopped", func() bool { return h.session(id).Status == sessionstore.SessionStatusStopped })
	_, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: id, Source: sessionstore.InputSourceAgent,
		Content: "please continue", Attributes: map[string]any{"sender_session_id": 999, "sender_name": "x"},
	})
	require.NoError(t, err)
	h.waitAnswer(id, "woken")
	assert.Equal(t, 1, h.countOutbox(id, func(s string) bool { return s == "↪ from #999 x\n\nplease continue" }))
}

func TestCommandOnlyStoppedRootRequiresHumanSource(t *testing.T) {
	var calls atomic.Int64
	h := newHarness(t, harnessOptions{respond: func(string, []llmwire.Message) *llmwire.Response {
		calls.Add(1)
		return textReply("must stay parked")
	}})
	id := h.createRoot(nil)
	limit := 1.0
	armed, err := h.store.Arm(
		h.ctx,
		budget.Mutation{RootSessionID: id, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "armed"},
	)
	require.NoError(t, err)
	_, _, err = h.store.FireBudget(h.ctx, id, armed.Generation, "cost", 2, "checkpoint")
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, id, sessionstore.SessionStatusStopped))
	_, err = h.store.Enqueue(
		h.ctx,
		sessionstore.Input{SessionID: id, Source: sessionstore.InputSourceAgent, Content: "/status"},
	)
	require.NoError(t, err)
	commandOnly, err := h.mgr.commandOnlyStoppedRoot(h.ctx, h.session(id))
	require.NoError(t, err)
	assert.False(t, commandOnly)
	deferStart, err := h.mgr.prepareStoppedSessionInput(h.ctx, h.session(id), "/status")
	require.NoError(t, err)
	assert.True(t, deferStart)
	err = h.mgr.startLocked(h.ctx, id)
	require.EqualError(t, err, fmt.Sprintf("session %d is stopped", id))
	resumed, err := h.mgr.resumeRecoverableRoot(h.ctx, id)
	require.NoError(t, err)
	assert.True(t, resumed)
	h.waitUntil("recovered budget remains parked", func() bool {
		return !h.mgr.HasActiveLoop(id) && !h.mgr.parks.active(id) &&
			h.session(id).Status == sessionstore.SessionStatusStopped
	})
	assert.Zero(t, calls.Load())
	current, err := h.store.Get(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, current.State)
}
