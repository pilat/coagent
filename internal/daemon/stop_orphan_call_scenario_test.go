package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// /stop is a durable two-phase park, so its second phase may run in a process
// that never saw the call staged. The stopped session is still resumable, so its
// transcript must be provider-valid when it comes back.
func TestScenario_StopThatDidNotSurviveItsRestartStillSettlesTheMaskedPrompt(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "stopped-secret.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.waitUntil("the session suspended on the masked prompt", func() bool {
		return first.mgr.staged.has(sessionID) && !first.mgr.HasActiveLoop(sessionID)
	})

	// Phase one of /stop lands; the process dies before phase two settles anything.
	require.NoError(t, first.sessStore.UpdateSessionStatus(
		first.ctx, sessionID, sessionstore.SessionStatusStopping,
	))
	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	second.mgr.sweep(second.ctx)

	rec, err := second.sessStore.GetSession(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopped, rec.Status, "the recovery finished the park")

	recovered := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(recovered),
		"a resumable stopped session must carry a transcript a provider accepts")
	require.Equal(t, 1, countToolResultsFor(recovered, tool.IDRequestSecret),
		"the abandoned prompt is settled exactly once")

	require.NoError(t, second.mgr.SendToSession(second.ctx, sessionID, "any progress?"))
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDRequestSecret),
		"the suspended call is answered, never re-executed")
	assert.True(t, hasUserContaining(final, "any progress?"))

	seen.assertAllPaired(t)
}
