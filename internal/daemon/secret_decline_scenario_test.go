package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/tool"
)

// secretResolvedIDs lists the dismissals published for a session, in order. Every
// terminal that may still be showing the prompt learns from these alone.
func secretResolvedIDs(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string

	for _, e := range events {
		if e.SessionID == sessionID && e.Notification.Type == sessionevent.NotifySecretResolved {
			out = append(out, e.Notification.RequestID)
		}
	}

	return out
}

// The live-process half of an unanswered masked prompt: the push went out once,
// so the request has to stay queryable for whichever terminal shows up next, and
// declining it must free the session exactly once.
func TestScenario_OutstandingSecretRequestIsQueryableAndDeclinable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "decline.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	d := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	events := collectEvents(d.mgr.PubSub().SubscribeAll())

	defer func() {
		events.stop()
		d.shutdown()
	}()

	sessionID, err := d.mgr.Send(
		d.ctx, d.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	events.waitFor(t, "the terminal was asked for the token",
		func(e []controllerapi.SessionNotification) bool {
			return countSecretRequests(e, sessionID) == 1
		})
	d.waitUntil("the session suspended on the prompt", func() bool {
		return d.mgr.staged.has(sessionID) && !d.mgr.HasActiveLoop(sessionID)
	})

	outstanding := d.mgr.PendingSecretRequests(sessionID)
	require.Len(t, outstanding, 1, "a prompt nobody answered is still open")
	assert.Equal(t, "MANAGER_TG_BOT_TOKEN", outstanding[0].SecretName)
	assert.Equal(t, "the bot token from BotFather", outstanding[0].Message)

	requestID := outstanding[0].RequestID
	require.NotEmpty(t, requestID)

	require.NoError(t, d.mgr.CancelSecretRequest(d.ctx, requestID))
	d.mgr.waitIdle(sessionID)

	final := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	require.Equal(t, 1, countToolResultsFor(final, tool.IDRequestSecret),
		"the declined prompt is closed exactly once")
	assert.Contains(t, lastToolResultContent(final, tool.IDRequestSecret),
		"declined to provide MANAGER_TG_BOT_TOKEN")
	assert.Contains(t, lastAssistantTextDTO(final), "declined", "the model reacts to the refusal")
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDRequestSecret),
		"the suspended call is answered, never re-executed")

	assert.Empty(t, d.mgr.PendingSecretRequests(sessionID), "nothing is left for a terminal to prompt")
	assert.False(t, d.mgr.staged.has(sessionID))

	// Another terminal may still be parked at that prompt, and the push that
	// opened it is the only thing it ever heard about the request.
	events.waitFor(t, "the prompt was dismissed everywhere else",
		func(e []controllerapi.SessionNotification) bool {
			return len(secretResolvedIDs(e, sessionID)) == 1
		})
	assert.Equal(t, []string{requestID}, secretResolvedIDs(events.snapshot(), sessionID))

	// Two terminals may both hold the prompt: the second answer finds nothing.
	require.Error(t, d.mgr.CancelSecretRequest(d.ctx, requestID))
	require.Error(t, d.mgr.ResolveSecretRequest(d.ctx, requestID, "MANAGER_TG_BOT_TOKEN"))
	assert.Equal(t, 1, countToolResultsFor(d.parentMessages(sessionID), tool.IDRequestSecret))
	assert.Equal(t, []string{requestID}, secretResolvedIDs(events.snapshot(), sessionID),
		"a refused answer dismisses nothing a second time")

	seen.assertAllPaired(t)
}

// The answer path across the same gap: the prompt outlives the runner that asked,
// so the credential typed later has to reach a session rebuilt from scratch.
func TestScenario_SecretTypedAfterTheSessionSuspendedResolvesTheCall(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "answer.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	d := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	events := collectEvents(d.mgr.PubSub().SubscribeAll())

	defer func() {
		events.stop()
		d.shutdown()
	}()

	sessionID, err := d.mgr.Send(
		d.ctx, d.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	events.waitFor(t, "the terminal was asked for the token",
		func(e []controllerapi.SessionNotification) bool {
			return countSecretRequests(e, sessionID) == 1
		})
	d.waitUntil("the session suspended on the prompt", func() bool {
		return d.mgr.staged.has(sessionID) && !d.mgr.HasActiveLoop(sessionID)
	})

	open := d.mgr.PendingSecretRequests(sessionID)
	require.Len(t, open, 1)

	require.NoError(t, d.mgr.ResolveSecretRequest(d.ctx, open[0].RequestID, open[0].SecretName))
	d.mgr.waitIdle(sessionID)

	final := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDRequestSecret))
	assert.Contains(t, lastToolResultContent(final, tool.IDRequestSecret), "MANAGER_TG_BOT_TOKEN set")
	assert.False(t, d.mgr.staged.has(sessionID), "the ledger entry goes with the answer")

	events.waitFor(t, "the prompt was dismissed everywhere else",
		func(e []controllerapi.SessionNotification) bool {
			return len(secretResolvedIDs(e, sessionID)) == 1
		})
	assert.Equal(t, []string{open[0].RequestID}, secretResolvedIDs(events.snapshot(), sessionID))

	seen.assertAllPaired(t)
}
