package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestOrchestratorSignalEmptyResponseError(t *testing.T) {
	t.Run("empty turn", testSignalEmptyTurn)
	t.Run("answer then empty follow-up", testSignalEmptyFollowUp)
}

func testSignalEmptyTurn(t *testing.T) {
	t.Helper()

	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "[Signal from coagent:") {
			return textReply("error observed")
		}

		return &llmwire.Response{FinishType: llmwire.FinishStop}
	}})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target := h.createRoot(nil)
	_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, watcher, target, "work")
	require.NoError(t, err)
	h.waitUntil("empty response termination", func() bool {
		return h.session(target).Status == sessionstore.SessionStatusError && !h.mgr.HasActiveLoop(target)
	})
	assertOutcomeSignal(t, h, watcher, target, target, "stopped on an error", "stopped on an error")
	states, err := h.store.WatchStates(h.ctx, watcher)
	require.NoError(t, err)
	assert.False(t, states[target].Armed)
	assert.True(t, states[target].Signalled)
}

func testSignalEmptyFollowUp(t *testing.T) {
	t.Helper()

	var h *harness
	var target int64

	queued := make(chan error, 1)
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		switch {
		case hasUserContaining(messages, "[Signal from coagent:"):
			return textReply("error observed")
		case hasUserContaining(messages, "EMPTY_FOLLOWUP"):
			return &llmwire.Response{FinishType: llmwire.FinishStop}
		case hasUserContaining(messages, completionNudgeMarker):
			_, err := h.store.Enqueue(h.ctx, sessionstore.Input{
				SessionID: target, Source: sessionstore.InputSourceUser, Content: "EMPTY_FOLLOWUP",
			})
			queued <- err
		}

		return textReply("first answer")
	}
	h = newHarness(t, harnessOptions{respond: respond})
	h.startInboxWake()
	watcher := modeRoot(t, h)
	target = h.createRoot(nil)
	_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, watcher, target, "work")
	require.NoError(t, err)
	h.waitUntil("follow-up queued before first answer", func() bool { return len(queued) == 1 })
	require.NoError(t, <-queued)
	h.waitUntil("follow-up empty response termination", func() bool {
		return h.session(target).Status == sessionstore.SessionStatusError && !h.mgr.HasActiveLoop(target)
	})
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool { return content == "first answer" }))
	assertOutcomeSignal(t, h, watcher, target, target, "stopped on an error", "stopped on an error")
}
