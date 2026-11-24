package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
)

func TestOrchestratorWaitIsAWakeSource(t *testing.T) {
	release := make(chan struct{})
	var target int64
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(messages, "[Signal from coagent:"):
			return textReply("signal handled")
		case hasUserContaining(messages, "TARGET_BLOCK"):
			<-release
			return textReply("target done")
		case hasToolResultFor(messages, orchestrator.IDSendToSession):
			return textReply("sent, waiting")
		case hasToolResultFor(messages, "set_budget"):
			return callReply("wait-send", orchestrator.IDSendToSession,
				fmt.Sprintf(`{"session_id":%d,"message":"TARGET_BLOCK"}`, target))
		default:
			return callReply("wait-budget", "set_budget", `{"action":"set","duration":"1m"}`)
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	defer closeOnce(release)
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target = h.createRoot(nil)
	h.say(watcher, "/budget send and wait")
	h.waitAnswer(watcher, "sent, waiting")
	assert.Zero(t, countUserCompletions(h.messages(watcher), completionNudgeMarker))
	record, err := h.store.Get(h.ctx, watcher)
	require.NoError(t, err)
	assert.Equal(t, budget.Armed, record.State)
	close(release)
	h.waitAnswer(target, "target done")
	assertAnswerSignal(t, h, watcher, target)
	h.waitAnswer(watcher, "signal handled")
	h.waitUntil("waiting budget released", func() bool {
		current, err := h.store.Get(h.ctx, watcher)
		return err == nil && current.State == budget.Released
	})
}

func TestOrchestratorMutualWaitsAreSignalled(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if !hasUserContaining(messages, "[Signal from coagent:") {
			<-release
		}
		return textReply("done")
	}})
	defer closeOnce(release)
	h.startInboxWake()
	a, b := modeRoot(t, h), modeRoot(t, h)
	host := orchestratorHost{s: h.mgr}
	_, _, err := host.SendToSession(h.ctx, a, b, "work for B")
	require.NoError(t, err)
	_, _, err = host.SendToSession(h.ctx, b, a, "work for A")
	require.NoError(t, err)
	close(release)
	h.waitUntil("both mutual wait outcomes", func() bool {
		return signalRows(t, h, a, b) == 1 && signalRows(t, h, b, a) == 1
	})
	assertAnswerSignal(t, h, a, b)
	assertAnswerSignal(t, h, b, a)
}
