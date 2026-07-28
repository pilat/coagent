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

func countSecretRequests(events []controllerapi.SessionNotification, sessionID int64) int {
	count := 0

	for _, e := range events {
		if e.SessionID == sessionID && e.Notification.Type == sessionevent.NotifySecretRequest {
			count++
		}
	}

	return count
}

// The whole observable story of a masked prompt nobody answered: the terminal is
// asked, the daemon restarts, and the next thing the user types must reach the
// model on a transcript a provider accepts — not a dangling tool_use forever.
func TestScenario_SecretRequestOrphanedByARestartIsClosedOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "secret.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	firstEvents := collectEvents(first.mgr.PubSub().SubscribeAll())

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	firstEvents.waitFor(t, "the terminal was asked for the token",
		func(events []controllerapi.SessionNotification) bool {
			return countSecretRequests(events, sessionID) == 1
		})
	first.waitUntil("the session suspended on the prompt", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	firstEvents.stop()
	first.shutdown() // nobody ever typed the token

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	secondEvents := collectEvents(second.mgr.PubSub().SubscribeAll())

	defer func() {
		secondEvents.stop()
		second.shutdown()
	}()

	second.mgr.sweep(second.ctx)

	recovered := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(recovered), "boot must leave a transcript a provider accepts")
	require.Equal(t, 1, countToolResultsFor(recovered, tool.IDRequestSecret),
		"the abandoned prompt is closed exactly once")
	assert.Contains(t, lastToolResultContent(recovered, tool.IDRequestSecret), "restarted")
	assert.False(t, second.mgr.HasActiveLoop(sessionID), "closing the call must not wake the session by itself")
	assert.Zero(t, countSecretRequests(secondEvents.snapshot(), sessionID),
		"the recovery must not re-open a prompt no terminal is waiting on")

	require.NoError(t, second.mgr.SendToSession(second.ctx, sessionID, "any progress?"))
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDRequestSecret),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDRequestSecret))
	assert.True(t, hasUserContaining(final, "any progress?"), "the user's message reaches the conversation")
	assert.Contains(t, lastAssistantTextDTO(final), "restarted", "the model reacts to the cancelled prompt")

	seen.assertAllPaired(t)
}

// Managers and the schedule executor start once Start returns, and a runner
// either of them opens makes PASS 0 skip that session for the rest of the boot.
// So PASS 0 has to be finished by then, not merely spawned.
func TestScenario_OrphanedCallsAreClosedBeforeStartReturns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boot.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	sessionID := stageSecretRequestAndStop(t, dbPath, configDir, &seen)

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs),
		"a controller may open a runner the instant Start returns")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDRequestSecret),
		"the orphaned prompt is closed by the time Start returns")
}

// A message that waited behind the masked prompt is the user's turn, not the
// daemon's: the restart that lost the prompt must not swallow it either.
func TestScenario_MessageQueuedBehindAnOrphanedSecretRequestRunsAfterRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queued.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.waitUntil("the session suspended on the prompt", func() bool {
		return first.mgr.staged.has(sessionID) && !first.mgr.HasActiveLoop(sessionID)
	})

	require.NoError(t, first.mgr.SendToSession(first.ctx, sessionID, "any progress?"))
	first.waitUntil("the message waits behind the prompt", func() bool {
		_, pendErr := first.sessStore.PeekPending(first.ctx, sessionID)

		return pendErr == nil && !first.mgr.HasActiveLoop(sessionID)
	})
	require.False(t, hasUserContaining(first.parentMessages(sessionID), "any progress?"),
		"a user turn may not split a tool_use from its result")

	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
	defer second.shutdown()

	second.mgr.sweep(second.ctx)
	second.waitUntil("the queued message finally ran", func() bool {
		return hasUserContaining(second.parentMessages(sessionID), "any progress?")
	})
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDRequestSecret))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDRequestSecret))
	second.requireInboxDrained(sessionID)

	seen.assertAllPaired(t)
}
