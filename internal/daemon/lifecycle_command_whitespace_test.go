package daemon

import (
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestLifecycleCommandTrimsSurroundingWhitespaceBeforeDispatch(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "prepare", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)
	before := modelCalls.Load()

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, " \t/stop \n"))
	h.waitUntil("whitespace-padded stop parks the session", func() bool {
		record, loadErr := h.sessStore.GetSession(h.ctx, sessionID)
		return loadErr == nil && record.Status == sessionstore.SessionStatusStopped
	})
	require.Equal(t, before, modelCalls.Load(), "a lifecycle command must not reach the model")
}

func TestInitialLifecycleCommandRequiresExistingSession(t *testing.T) {
	for _, command := range []string{"/stop", " /clear ", "\t/kill\n"} {
		t.Run(command, func(t *testing.T) {
			var modelCalls atomic.Int64
			h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
				modelCalls.Add(1)
				return &llmwire.Response{Text: "unexpected"}
			})
			defer h.shutdown()

			_, err := h.mgr.Send(h.ctx, h.projectID, command, "fake-model", map[string]any{
				"manager_id": scenarioManagerID,
			})
			require.ErrorContains(t, err, "requires an existing session")
			require.Zero(t, modelCalls.Load())
			var sessions int
			require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM sessions`).Scan(&sessions))
			require.Zero(t, sessions, "rejected lifecycle command must not create a session")
		})
	}
}

func TestInitialStopPrefixRemainsOrdinaryInput(t *testing.T) {
	var modelCalls atomic.Int64
	h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
		modelCalls.Add(1)
		return &llmwire.Response{Text: "ready"}
	})
	defer h.shutdown()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/stop now", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)
	require.Positive(t, modelCalls.Load(), "only the exact lifecycle command is host-handled")
}
