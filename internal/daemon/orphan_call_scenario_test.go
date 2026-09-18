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

// parkedOnChild reports the durable park, not the in-memory loop flag: between
// two model iterations the loop briefly counts as inactive, and a wait keyed on
// that flag races the next iteration on a fast machine.
func (d *applyDaemon) parkedOnChild(sessionID int64) bool {
	rec, err := d.sessStore.GetSession(d.ctx, sessionID)
	if err != nil || rec.Status != sessionstore.SessionStatusSuspended {
		return false
	}

	return countAssistantToolCallsFor(d.parentMessages(sessionID), tool.IDTask) == 1
}

// The whole observable story of a blocking child lost to a restart: the parent
// parks on the task, the daemon restarts, and the next thing the user types must
// reach the model on a transcript a provider accepts — not a dangling tool_use.
func TestScenario_BlockingTaskOrphanedByARestartIsClosedOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "task.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return first.parkedOnChild(sessionID)
	})

	// The child never finishes; the restart must not resurrect it as a producer.
	link, err := first.links.GetLinkByTaskCallID(first.ctx, sessionID, orphanTaskCallID)
	require.NoError(t, err)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")
	_, err = first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, err)

	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	second.mgr.sweep(second.ctx)

	recovered := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(recovered), "boot must leave a transcript a provider accepts")
	require.Equal(t, 1, countToolResultsFor(recovered, tool.IDTask),
		"the abandoned task is closed exactly once")
	assert.Contains(t, lastToolResultContent(recovered, tool.IDTask), "restarted")
	assert.False(t, second.mgr.HasActiveLoop(sessionID), "closing the call must not wake the session by itself")

	require.NoError(t, second.mgr.SendToSession(second.ctx, sessionID, "any progress?"))
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask))
	assert.True(t, hasUserContaining(final, "any progress?"), "the user's message reaches the conversation")
	assert.Contains(t, lastAssistantTextDTO(final), "restarted", "the model reacts to the cancelled task")

	seen.assertAllPaired(t)
}

// Managers and the schedule executor start once Start returns, and a runner
// either of them opens makes PASS 0 skip that session for the rest of the boot.
// So PASS 0 has to be finished by then, not merely spawned.
func TestScenario_OrphanedCallsAreClosedBeforeStartReturns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "boot.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests

	sessionID := stageTaskAndStop(t, dbPath, configDir, &seen)

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs),
		"a controller may open a runner the instant Start returns")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDTask),
		"the orphaned task is closed by the time Start returns")
}

// A message that waited behind the task is the user's turn, not the
// daemon's: the restart that lost the child must not swallow it either.
func TestScenario_MessageQueuedBehindAnOrphanedTaskRunsAfterRecovery(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "queued.db")
	configDir := newApplyConfigDir(t)

	var seen modelRequests
	// The child must stay parked, or its completion wakes the parent before
	// the queued message is ever observed waiting behind the task.
	childHeld := make(chan struct{})
	heldRespond := func(system string, msgs []llmwire.Message) *llmwire.Response {
		if !hasToolResultFor(msgs, tool.IDTask) && hasUserContaining(msgs, "do the thing") {
			<-childHeld
		}

		return askForBlockingTaskRespond(system, msgs)
	}

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(heldRespond))
	defer func() {
		select {
		case <-childHeld:
		default:
			close(childHeld)
		}
	}()

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return first.parkedOnChild(sessionID)
	})

	require.NoError(t, first.mgr.SendToSession(first.ctx, sessionID, "any progress?"))
	first.waitUntil("the message waits behind the task", func() bool {
		_, pendErr := first.sessStore.PeekPending(first.ctx, sessionID)

		return pendErr == nil && !first.mgr.HasActiveLoop(sessionID)
	})
	require.False(t, hasUserContaining(first.parentMessages(sessionID), "any progress?"),
		"a user turn may not split a tool_use from its result")

	link, linkErr := first.links.GetLinkByTaskCallID(first.ctx, sessionID, orphanTaskCallID)
	require.NoError(t, linkErr)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")
	_, delErr := first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, delErr)

	first.shutdown()

	second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
	defer second.shutdown()

	second.mgr.sweep(second.ctx)
	second.waitUntil("the queued message finally ran", func() bool {
		return hasUserContaining(second.parentMessages(sessionID), "any progress?")
	})
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask))
	second.requireInboxDrained(sessionID)

	seen.assertAllPaired(t)
}
