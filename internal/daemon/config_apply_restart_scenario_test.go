package daemon

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/tool"
)

// The verdict for a session-owned apply is produced by a different process image
// than the one that suspended the call. It must still reach that exact call.
func TestScenario_ConfigApplyVerdictReachesTheSessionAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err, "the verdict must reach the session that suspended")
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)

	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSetDefaultModel), "exactly one result for the call")
	assert.Contains(t, lastToolResultContent(msgs, tool.IDSetDefaultModel), "Config applied")
	assert.Equal(t, 0, second.restartCount(), "answering a verdict must not stage another apply")

	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName),
		"the marker is cleared once the verdict is durably delivered")
}

// A process that resolves the marker and dies before delivering must leave the
// verdict deliverable: the next boot is the only thing that can still answer.
func TestScenario_ConfigApplyVerdictSurvivesADaemonThatDiesBeforeDelivering(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	// Dies between resolving the marker and telling the session.
	second.shutdown()

	third := newApplyDaemon(t, dbPath, configDir)
	defer third.shutdown()

	require.NoError(t, third.mgr.Start(third.ctx))

	_, err = third.bootVerdict(t)
	require.NoError(t, err)

	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSetDefaultModel))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel))
}

// A session woken for another reason before its verdict arrives still owes the
// config call: re-executing it would apply the same change — and restart — twice.
func TestScenario_ConfigApplyCallIsNotReExecutedBeforeItsVerdict(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	events := collectEvents(second.mgr.PubSub().SubscribeAll())
	defer events.stop()

	require.NoError(t, second.mgr.SendToSession(second.ctx, sessionID, "are you done yet?"))

	events.waitFor(t, "the woken session ran", func(ns []controllerapi.SessionNotification) bool {
		for _, n := range ns {
			if n.SessionID == sessionID && n.Notification.Status == controllerapi.StateRunning {
				return true
			}
		}

		return false
	})
	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel), "the apply was not repeated")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDSetDefaultModel), "the call is still out with the world")
	assert.Zero(t, second.restartCount(), "a wake-up must not stage a second apply")

	// The queued message waited behind the call; the verdict still lands first.
	_, err := second.bootVerdict(t)
	require.NoError(t, err)

	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDSetDefaultModel))
	assert.True(t, hasUserContaining(final, "are you done yet?"), "the queued message runs after the verdict")
}

// failingCommitOps is an ops layer whose write fails, so the apply is rejected
// with no restart and no marker — the verdict has to come back inline.
type failingCommitOps struct {
	configops.Service
}

func (failingCommitOps) Commit(*configops.Staged, configops.Pending) configops.Verdict {
	return configops.Reject("", errors.New("no space left on device"))
}

// A commit that never landed is rejected in-process. The session is owed that
// answer just as much as it is owed a verdict from across a restart.
func TestScenario_ConfigApplyRejectionReachesTheSessionInProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	d := newApplyDaemon(t, dbPath, configDir)
	defer d.shutdown()

	d.mgr.applier = configapply.New(failingCommitOps{d.ops}, func() { d.restarts <- struct{}{} })

	sessionID, err := d.mgr.Send(
		d.ctx, d.projectID, "switch the default model", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	d.waitUntil("the rejection reached the transcript", func() bool {
		return countToolResultsFor(d.parentMessages(sessionID), tool.IDSetDefaultModel) == 1
	})

	d.mgr.waitIdle(sessionID)

	msgs := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDSetDefaultModel), "rejected")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel))
	assert.Zero(t, d.restartCount(), "a rejected commit never restarts")
	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName))
}

// A crash between injecting the verdict and clearing the marker replays the
// delivery on the next boot. The transcript already carries the answer, so the
// replay inserts nothing rather than a duplicate result.
func TestScenario_ConfigApplyVerdictRedeliveryIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	applied, err := second.mgr.DeliverPendingCallResult(
		second.ctx, sessionID, applyCallID, tool.IDSetDefaultModel, "Config applied: default model",
	)
	require.NoError(t, err)
	require.True(t, applied)

	// Dies after the injection, before the acknowledgement.
	second.shutdown()

	third := newApplyDaemon(t, dbPath, configDir)
	defer third.shutdown()

	require.NoError(t, third.mgr.Start(third.ctx))

	replay, err := third.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay, "an unacknowledged verdict is replayed")

	_, err = third.ops.ResolvePending(*replay, nil)
	require.NoError(t, err)

	applied, err = third.mgr.DeliverPendingCallResult(
		third.ctx, sessionID, applyCallID, tool.IDSetDefaultModel, "Config applied: default model",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")

	require.NoError(t, third.ops.ClearPending(*replay))

	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSetDefaultModel))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel))
}
