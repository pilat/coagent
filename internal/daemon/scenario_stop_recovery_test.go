package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestScenario_BootFinishedStopIsNotAppliedToLaterWork(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "continue please") {
			return textReply("Resumed after recovery.")
		}
		return textReply("must not run")
	}
	h1 := newHarness(t, harnessOptions{dbPath: filepath.Join(t.TempDir(), "boot-finished-stop.db"), respond: respond})
	root := h1.createRoot(nil)
	stop, err := h1.store.Enqueue(h1.ctx, sessionstore.Input{
		SessionID: root, Source: sessionstore.InputSourceUser, Content: "/stop",
	})
	require.NoError(t, err)
	_, err = h1.store.BeginLifecycleInput(h1.ctx, stop.Input.ID, "stop", "⏳ Stopping…")
	require.NoError(t, err)
	h2 := h1.restart(nil)
	require.NoError(t, h2.mgr.Start(h2.ctx))
	require.Equal(t, sessionstore.SessionStatusStopped, h2.session(root).Status)
	require.Equal(t, 1, countStopOutput(h2, root, stop.Input.ID, "completed"))
	h2.say(root, "continue please")
	h2.waitAnswer(root, "Resumed after recovery.")
	afterWork := h2.session(root).Status
	require.NotEqual(t, sessionstore.SessionStatusStopping, afterWork)
	require.NotEqual(t, sessionstore.SessionStatusStopped, afterWork)
	var laterCalls atomic.Int32
	h3 := h2.restart(func(_ string, _ []llmwire.Message) *llmwire.Response {
		laterCalls.Add(1)
		return textReply("must not run")
	})
	require.NoError(t, h3.mgr.Start(h3.ctx))
	h3.resume()
	assert.Equal(t, afterWork, h3.session(root).Status)
	assert.Equal(t, "Resumed after recovery.", lastAssistantTextDTO(h3.messages(root)))
	assert.True(t, hasUserContaining(h3.messages(root), "continue please"))
	assert.Equal(t, 1, countStopOutput(h3, root, stop.Input.ID, "completed"))
	isStopped := func(c string) bool { return strings.HasPrefix(c, sessionstore.StopTerminalContent) }
	assert.Equal(t, 1, h3.countOutbox(root, isStopped))
	require.False(t, h3.mgr.HasActiveLoop(root))
	assert.Zero(t, laterCalls.Load())
	assert.Equal(t, sessionstore.InputStateHandled, stopRecoveryInputState(h3, stop.Input.ID))
}

func TestScenario_PendingStopIsDispatchedOnceAtBoot(t *testing.T) {
	var modelCalls atomic.Int32
	respond := func(_ string, _ []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return textReply("must not run")
	}
	h1 := newHarness(t, harnessOptions{dbPath: filepath.Join(t.TempDir(), "pending-stop.db"), respond: respond})
	root := h1.createRoot(nil)
	var inputIDs []int64
	for _, content := range []string{"/stop", "queued after stop"} {
		input, err := h1.store.Enqueue(h1.ctx, sessionstore.Input{
			SessionID: root, Source: sessionstore.InputSourceUser, Content: content,
		})
		require.NoError(t, err)
		inputIDs = append(inputIDs, input.Input.ID)
		require.Equal(t, sessionstore.InputStatePending, stopRecoveryInputState(h1, input.Input.ID))
	}
	require.Empty(t, h1.outbox(root))
	h2 := h1.restart(nil)
	require.NoError(t, h2.mgr.Start(h2.ctx))
	h2.waitUntil("boot dispatched the pending stop", func() bool {
		return h2.session(root).Status == sessionstore.SessionStatusStopped &&
			countStopOutput(h2, root, inputIDs[0], "completed") == 1
	})
	h2.resume()
	assert.Equal(t, 1, countStopOutput(h2, root, inputIDs[0], "started"))
	assert.Equal(t, 1, countStopOutput(h2, root, inputIDs[0], "completed"))
	assert.Zero(t, countStopOutput(h2, root, inputIDs[0], "already_stopped"))
	assert.Equal(t, sessionstore.InputStateHandled, stopRecoveryInputState(h2, inputIDs[0]))
	assert.Equal(t, sessionstore.InputStateCancelled, stopRecoveryInputState(h2, inputIDs[1]))
	require.False(t, h2.mgr.HasActiveLoop(root))
	assert.Zero(t, modelCalls.Load())
	assert.Equal(t, sessionstore.SessionStatusStopped, h2.session(root).Status)
}

func countStopOutput(h *harness, root, inputID int64, suffix string) int {
	count := 0
	for _, row := range h.outbox(root) {
		if row.SourceKey == fmt.Sprintf("input:%d:stop:%s", inputID, suffix) {
			count++
		}
	}
	return count
}

func stopRecoveryInputState(h *harness, id int64) sessionstore.InputState {
	h.t.Helper()
	var state sessionstore.InputState
	err := h.db.QueryRowContext(h.ctx, "SELECT state FROM session_inbox WHERE id = ?", id).Scan(&state)
	require.NoError(h.t, err)
	return state
}
