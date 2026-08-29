package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// This is the production incident shape: an ordinary in-loop bash call is
// cancelled, not an externally owned call. Stop must settle the transcript so a
// later input starts fresh instead of reconstructing and re-running bash.
func TestScenario_StopOrdinaryBashDoesNotReplayAfterNewInputOrRestart(t *testing.T) {
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

	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "same daemon", true: "after restart"}[restart], func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "ordinary-bash.db")
			first := newSubagentHarnessOnDB(t, dbPath, respond, nil)
			firstID, err := first.mgr.Send(first.ctx, first.projectID, "watch checks", "fake-model", nil)
			require.NoError(t, err)
			first.waitUntil("ordinary bash started", func() bool {
				return countAssistantToolCallsFor(first.parentMessages(firstID), "bash") == 1 &&
					first.mgr.HasActiveLoop(firstID)
			})

			require.NoError(t, first.mgr.Stop(first.ctx, firstID, 0))
			first.waitUntil("stop completed", func() bool {
				rec, getErr := first.sessStore.GetSession(first.ctx, firstID)
				return getErr == nil && rec.Status == sessionstore.SessionStatusStopped
			})

			active := first.parentMessages(firstID)
			require.NoError(t, llm.ValidateToolPairing(active))
			assert.Equal(t, 1, countToolResultsFor(active, "bash"))

			d := first
			if restart {
				first.shutdown()
				d = newSubagentHarnessOnDB(t, dbPath, respond, nil)
				require.NoError(t, d.mgr.Start(d.ctx))
				defer d.shutdown()
			}

			require.NoError(t, d.mgr.SendToSession(d.ctx, firstID, "what happened?"))
			d.mgr.waitIdle(firstID)
			final := d.parentMessages(firstID)
			assert.Equal(t, 1, countAssistantToolCallsFor(final, "bash"))
			assert.Equal(t, 1, countToolResultsFor(final, "bash"))
			assert.True(t, hasUserContaining(final, "what happened?"))
			assert.Contains(t, assistantTexts(final), "fresh response")
		})
	}
}

func assistantTexts(messages []llmwire.Message) []string {
	var texts []string
	for _, message := range messages {
		if message.Role == llmwire.RoleAssistant && message.Content != "" {
			texts = append(texts, message.Content)
		}
	}
	return texts
}

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
