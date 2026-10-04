package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// The marker, the config file and the restart are global, so the "one staged
// change at a time" guard has to be too. A second session's commit would
// overwrite the marker naming the first — losing that session's change and
// leaving its call with no producer that owes it a result.
func TestScenario_ASecondSessionCannotOverwriteAStagedApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)

	first.startInboxWake()
	sessionA, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_A switch the default model", "fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)
	first.waitUntil("A's opener settled", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	first.mgr.waitIdle(sessionA)
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionA, configapply.ConfigEditCommand))

	first.waitForRestart(t)
	first.waitUntil("A suspended on its config call", func() bool { return !first.mgr.HasActiveLoop(sessionA) })

	// B stages against the config A is already restarting into.
	first.startInboxWake()
	sessionB, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_B switch the default model", "fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)
	first.waitUntil("B's opener settled", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	first.mgr.waitIdle(sessionB)
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionB, configapply.ConfigEditCommand))

	first.mgr.waitIdle(sessionB)

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, sessionA, pending.SessionID, "the marker still names the session that is owed a verdict")
	assert.Equal(t, applyCallA, pending.ToolCallID)
	assert.Equal(t, "claude-opus-5", defaultModelInFile(t, configDir), "A's change is the one on disk")
	assert.Zero(t, first.restartCount(), "a refused stage never asks for a second restart")

	msgsB := first.parentMessages(sessionB)
	require.NoError(t, llm.ValidateToolPairing(msgsB))
	assert.Equal(t, 1, countToolResultsFor(msgsB, tool.IDConfigEdit), "B is answered in-process")
	assert.Contains(t, lastToolResultContent(msgsB, tool.IDConfigEdit), "config change")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())

	second.waitForConfigResult(sessionA)
	second.mgr.waitIdle(sessionA)

	msgsA := second.parentMessages(sessionA)
	require.NoError(t, llm.ValidateToolPairing(msgsA))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsA, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsA, tool.IDConfigEdit), "A's call is resolved exactly once")
	assert.Contains(t, lastToolResultContent(msgsA, tool.IDConfigEdit), "Config applied")
}

// The boot decides "this verdict can never be delivered" from the session
// record. This is the daemon-side half of that contract: the refusal, and the
// record that explains it.
func TestScenario_AVerdictOwedToAKilledSessionIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "/kill"))

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = enqueueCallResult(

		second.ctx, second.mgr.store,

		pending.SessionID,
		pending.ToolCallID,
		pending.ToolName,
		"Config applied",
	)
	require.Error(t, err, "a killed session can never take the verdict")

	rec, err := second.mgr.store.GetSession(second.ctx, sessionID)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "the record is what the boot reads to tell 'never' from 'not now'")
}

// Same invariant without the staging: whichever session wins the race owns the
// only marker, the loser is refused in-process, and neither transcript is left
// with a dangling tool call.
func TestScenario_ConcurrentAppliesResolveExactlyOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		sessions = map[string]int64{}
		sendErrs []error
	)

	for _, prompt := range []string{"APPLY_A switch the default model", "APPLY_B switch the default model"} {
		wg.Go(func() {
			first.startInboxWake()
			id, err := first.mgr.Send(
				first.ctx, first.projectID, prompt, "fake-model",
				map[string]any{"manager_id": "telegram:main"},
			)

			mu.Lock()
			sessions[prompt[:7]] = id
			sendErrs = append(sendErrs, err)
			mu.Unlock()
		})
	}

	wg.Wait()
	require.NoError(t, errors.Join(sendErrs...))

	sessionA, sessionB := sessions["APPLY_A"], sessions["APPLY_B"]

	first.waitUntil("both openers settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionA) && !first.mgr.HasActiveLoop(sessionB)
	})
	first.mgr.waitIdle(sessionA)
	first.mgr.waitIdle(sessionB)

	for _, session := range []int64{sessionA, sessionB} {
		first.startInboxWake()
		require.NoError(t, first.mgr.sendToSession(first.ctx, session, configapply.ConfigEditCommand))
	}

	first.waitForRestart(t)
	first.waitUntil("both sessions settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionA) && !first.mgr.HasActiveLoop(sessionB)
	})

	assert.Zero(t, first.restartCount(), "one apply, one restart")

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	winner, loser, winnerCall := sessionA, sessionB, applyCallA
	if pending.SessionID == sessionB {
		winner, loser, winnerCall = sessionB, sessionA, applyCallB
	}

	require.Equal(t, winner, pending.SessionID)
	assert.Equal(t, winnerCall, pending.ToolCallID)

	msgsLoser := first.parentMessages(loser)
	require.NoError(t, llm.ValidateToolPairing(msgsLoser))
	assert.Equal(t, 1, countToolResultsFor(msgsLoser, tool.IDConfigEdit),
		"the refused call is answered rather than suspended")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	_, err = second.bootVerdict(t)
	require.NoError(t, err)

	second.waitForConfigResult(winner)
	second.mgr.waitIdle(winner)

	msgsWinner := second.parentMessages(winner)
	require.NoError(t, llm.ValidateToolPairing(msgsWinner))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsWinner, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsWinner, tool.IDConfigEdit))
}

// The commit is what arms the marker, and the marker is a promise to answer one
// exact call after the restart. A staged change whose suspend the transcript does
// not carry has no such call, so committing writes a promise nothing can keep.
func TestRunStagedApply_RefusesToCommitForASuspendTheTranscriptDoesNotCarry(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	// The tool stages and suspends, but the assistant turn carrying c1 never
	// reached the store — nothing in the transcript is waiting for a verdict.
	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(ctx, h.sessionID, "c1"), configEditArgs(configHarnessCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	assert.Empty(t, h.mgr.applier.Restart(), "an unbacked suspend must not restart the daemon")
	assert.Equal(t, toolConfig, h.configBytes(t), "and must not write the config")

	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending, "no marker is armed for a call no boot could answer")

	assert.False(t, h.mgr.applier.Has(h.sessionID), "the call is settled in-process, not across a restart")
	assert.True(t, h.mgr.applier.ClaimApply(),
		"the slot is free for the next change")
}

// The same session may go on to make a change that does suspend durably, and it
// must be applied normally — the refusal above is about one call, not the session.
func TestRunStagedApply_ADurableSuspendAfterAnUnbackedOneStillApplies(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(ctx, h.sessionID, "c1"), configEditArgs(configHarnessCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	require.ErrorIs(t, h.grantedCall(t, "c2", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	assert.Len(t, h.mgr.applier.Restart(), 1)
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")

	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, "c2", pending.ToolCallID, "the marker names the call that really suspended")
}

// A stale producer verdict is acknowledged at ingress and rejected without inventing a tool result.
func TestScenario_AMarkerForACallTheTranscriptDoesNotCarryIsNeverConsumed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	d := newApplyDaemonWith(t, dbPath, configDir, plainRespond)
	defer d.shutdown()

	require.NoError(t, d.mgr.Start(d.ctx))

	sessionID, err := d.mgr.Send(d.ctx, d.projectID, "say hello", "fake-model", nil)
	require.NoError(t, err)
	d.mgr.waitIdle(sessionID)

	staged, v := d.ops.StageDocument([]byte(toolConfig))
	require.False(t, v.Failed(), "%s", v.Reason())
	require.False(t, d.ops.Commit(staged, configops.Pending{
		SessionID: sessionID, ToolCallID: "ghost-call", ToolName: tool.IDConfigEdit,
	}).Failed())

	before := len(d.parentMessages(sessionID))
	_, err = d.bootVerdict(t)
	require.NoError(t, err)
	d.waitUntil("stale verdict resolved", func() bool {
		pending, err := d.sessStore.ListPending(d.ctx, sessionID)
		return err == nil && len(pending) == 0 && !d.mgr.HasActiveLoop(sessionID)
	})
	var state string
	require.NoError(
		t,
		d.db.QueryRowContext(d.ctx, `SELECT state FROM session_inbox WHERE session_id=? AND delivery_key='config_apply:ghost-call'`, sessionID).
			Scan(&state),
	)
	assert.Equal(t, string(sessionstore.InputStateRejected), state)
	assert.Len(t, d.parentMessages(sessionID), before)
	assert.Zero(t, countToolResultsFor(d.parentMessages(sessionID), tool.IDConfigEdit))
	still, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, still, "a committed stale verdict must not arm later unrelated rollback")
	assert.NoError(t, llm.ValidateToolPairing(d.parentMessages(sessionID)))
}

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

	second.waitForConfigResult(sessionID)
	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit), "exactly one result for the call")
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
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

	third.waitForConfigResult(sessionID)
	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
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

	second.startInboxWake()
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "are you done yet?"))

	second.mgr.waitIdle(sessionID)

	queued, err := second.sessStore.PeekPending(second.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, "are you done yet?", queued.RawContent)

	msgs := second.parentMessages(sessionID)
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit), "the apply was not repeated")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is still out with the world")
	assert.Zero(t, second.restartCount(), "a wake-up must not stage a second apply")

	// The queued message waited behind the call; the verdict still lands first.
	_, err = second.bootVerdict(t)
	require.NoError(t, err)

	second.waitForConfigResult(sessionID)
	second.mgr.waitIdle(sessionID)

	final := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDConfigEdit))
	assert.True(t, hasUserContaining(final, "are you done yet?"), "the queued message runs after the verdict")
}

// A commit that never landed is rejected in-process. The session is owed that
// answer just as much as it is owed a verdict from across a restart.
func TestScenario_ConfigApplyRejectionReachesTheSessionInProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	d := newApplyDaemon(t, dbPath, configDir)
	defer d.shutdown()

	d.mgr.applier = configapply.New(failingCommitOps{d.ops}, d.sessStore)

	sessionID := startConfigEditSession(t, d, "switch the default model")

	d.waitUntil("the rejection reached the transcript", func() bool {
		return countToolResultsFor(d.parentMessages(sessionID), tool.IDConfigEdit) == 1
	})

	d.mgr.waitIdle(sessionID)

	msgs := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "rejected")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
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

	applied, err := enqueueCallResult(

		second.ctx, second.mgr.store,

		sessionID,
		applyCallID,
		tool.IDConfigEdit,
		"Config applied: default model",
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

	applied, err = enqueueCallResult(

		third.ctx, third.mgr.store,

		sessionID,
		applyCallID,
		tool.IDConfigEdit,
		"Config applied: default model",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")

	require.NoError(t, third.ops.ClearPending(*replay))

	third.waitForConfigResult(sessionID)
	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
}

// One-shot rule driven through the real store: after the successful apply has
// spent the grant, a replayed call in a later turn is refused.
func TestScenario_ConfigEditGrantIsOneShot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	consumed := currentActivationOf(t, first.subagentHarness, sessionID)
	require.NotNil(t, consumed)
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the successful apply spent the grant")

	// A second successful mutation cannot use the same grant: consuming again
	// with a different call id conflicts, and the session was never re-suspended.
	err := first.sessStore.ConsumeActivationBinding(
		first.ctx, sessionstore.ActivationBinding{
			InputID: consumed.InputID, SessionID: sessionID,
			ToolID: tool.IDConfigEdit, Command: configapply.ConfigEditCommand, ToolCallID: "cfg-edit-call-2",
		})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)
}

// The crash window: the first image committed the apply but died before
// spending the grant. The next boot spends it off the marker — it must not
// expire with a false "was not changed" receipt or re-arm the one-shot grant.
func TestScenario_ConfigEditCrashBeforeGrantConsumeSettlesOnBoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")

	first.shutdown()

	// first.shutdown() is graceful: its apply consumed the grant before the
	// restart. Rewind the row to the exact durable state a crash between the
	// committed apply and the consume leaves — pending, tool_call_id unset.
	_, err = first.db.Exec(`UPDATE session_tool_activations
		SET state = 'pending', tool_call_id = NULL, resolved_at = NULL
		WHERE session_id = ? AND tool_id = ?`, sessionID, tool.IDConfigEdit)
	require.NoError(t, err)

	// The crash window: nothing spent the grant before the image went down.
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer second.shutdown()

	activation, err := second.sessStore.
		CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationPending, activation.State,
		"the crash left the grant pending — the window under test")

	// The boot path runs without the apply process: resolve, spend, deliver.
	outcome, err := second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	require.False(t, outcome.RolledBack)

	second.mgr.applier.ConsumeConfigEditActivation(
		second.ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID,
	)

	consumed, err := second.sessStore.
		CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the boot spends the grant the crashed process left pending")

	// One-shot is not re-armed: a second settlement attempt must conflict.
	err = second.sessStore.ConsumeActivationBinding(
		second.ctx, sessionstore.ActivationBinding{
			InputID: consumed.InputID, SessionID: sessionID,
			ToolID: tool.IDConfigEdit, Command: configapply.ConfigEditCommand, ToolCallID: "cfg-edit-call-2",
		})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)

	// The verdict still reaches the session, and the receipt says the config
	// changed — the expiry receipt's "was not changed" would have been a lie.
	require.NoError(t, second.mgr.Start(second.ctx))

	message := "Config applied: " + outcome.Pending.Summary
	_, err = enqueueCallResult(

		second.ctx, second.mgr.store,

		outcome.Pending.SessionID,
		outcome.Pending.ToolCallID,
		outcome.Pending.ToolName,
		message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))

	second.waitForConfigResult(sessionID)
	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
}

func TestScenario_AbandonedApply(t *testing.T) {
	tests := []struct {
		name    string
		failure string
	}{
		{name: "settles without restarting failed loop"},
		{name: "retries failed result on explicit input", failure: "write failure"},
		{name: "respects stop", failure: "stop"},
		{name: "respects kill", failure: "kill"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testAbandonedApply(t, tt.failure) })
	}
}

func TestScenario_AbandonedConfigCommand(t *testing.T) {
	tests := []struct {
		name    string
		restart bool
	}{
		{name: "expires grant before next input"},
		{name: "recovers after result failure and restart", restart: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) { testAbandonedConfigCommand(t, tt.restart) })
	}
}

func TestScenario_ConfigQuestionCannotReplaceDocumentWithSandboxFragment(t *testing.T) {
	configDir := newApplyConfigDir(t)
	configPath := filepath.Join(configDir, "config.yaml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "config-question.db"), configDir,
		func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, tool.IDConfigEdit) {
				return &llmwire.Response{Text: "configuration unchanged"}
			}
			if hasUserContaining(messages, "/config") {
				return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
					{
						ID:   "fragment",
						Name: tool.IDConfigEdit,
						Arguments: json.RawMessage(
							`{"document":"sandbox:\n  enabled: true\n  projects:\n    /tmp/project:\n      rules: []\n"}`,
						),
					},
				}}
			}
			return &llmwire.Response{Text: "ready"}
		})
	defer d.shutdown()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	d.waitUntil("opening turn settled", func() bool { return !d.mgr.HasActiveLoop(id) })
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/config - is the sandbox unconfigured?"))
	d.waitUntil("invalid replacement answered", func() bool {
		return hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit) && !d.mgr.HasActiveLoop(id)
	})
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, d.restartCount())
	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending)
	assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "configuration fragments")
}

// The full happy path: a real /config user turn authorizes exactly one solo
// config_edit call; the apply commits through the marker protocol; the grant is
// spent; the verdict reaches the call only after the restart.
func TestScenario_ConfigEditVerdictReachesTheSessionAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, configEditCallID, pending.ToolCallID)
	require.Equal(t, tool.IDConfigEdit, pending.ToolName)

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	require.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is out with the world")

	consumed := currentActivationOf(t, first.subagentHarness, sessionID)
	require.NotNil(t, consumed, "the /config grant exists")
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the successful apply spends the grant; it may not stay pending")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)

	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.mgr.waitIdle(sessionID)

	msgs = second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
	assert.Equal(t, 0, second.restartCount())

	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName))

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "id: claude-opus-5")
	assert.Contains(t, string(body), "${WORK_API_KEY}", "credentials stay references on disk")
}

// Without a user /config turn the model sees config_edit but gets an
// authorization error before anything is staged.
func TestScenario_ConfigEditWithoutActivationNeverStages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	d := newApplyDaemonWith(t, dbPath, configDir, unauthorizedConfigEditRespond)
	defer d.shutdown()

	require.NoError(t, d.mgr.Start(d.ctx))

	// The responder calls config_edit; no activation exists, so the session must
	// be answered with the authorization refusal, in-process.
	d.startInboxWake()
	sessionID, err := d.mgr.Send(
		d.ctx,
		d.projectID,
		"reconfigure the daemon",
		"fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)

	d.waitUntil("the refused call reached the transcript", func() bool {
		return countToolResultsFor(d.parentMessages(sessionID), tool.IDConfigEdit) == 1
	})
	d.mgr.waitIdle(sessionID)

	msgs := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "/config")
	assert.Zero(t, d.restartCount(), "an unauthorized call never applies")
	assert.Equal(t, toolConfig, configBytesOf(t, configDir))
	assert.Nil(t, currentActivationOf(t, d.subagentHarness, sessionID))
}

// A syntactically valid candidate whose boot fails is rolled back and the
// rejection reaches the session — the same ResolvePending contract main.go runs.
func TestScenario_ConfigEditBootInvalidCandidateRollsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "a staged candidate is committed before the restart")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer second.shutdown()

	// The second image comes up on a config it cannot serve: ResolvePending is
	// handed the boot error the real daemon would carry, and must roll back.
	outcome, err := second.ops.ResolvePending(*pending, errors.New("cold catalog: unknown model claude-opus-9"))
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Failed())
	assert.True(t, outcome.RolledBack)

	second.startInboxWake()
	message := "Config change rejected — " + outcome.Verdict.Reason()
	_, err = enqueueCallResult(

		second.ctx, second.mgr.store,

		outcome.Pending.SessionID,
		outcome.Pending.ToolCallID,
		outcome.Pending.ToolName,
		message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))

	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "rolled back")

	assert.Equal(t, toolConfig, configBytesOf(t, configDir), "the rollback restores the previous config")
}

// The verdict is delivered exactly once even when the first image died between
// the write and the delivery: a replayed delivery inserts no second result.
func TestScenario_ConfigEditVerdictRedeliveryIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	applied, err := enqueueCallResult(

		second.ctx, second.mgr.store,

		sessionID,
		configEditCallID,
		tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	require.True(t, applied)

	// Dies after the injection, before the acknowledgement.
	second.shutdown()

	third := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer third.shutdown()

	require.NoError(t, third.mgr.Start(third.ctx))

	replay, err := third.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay, "an unacknowledged verdict is replayed")

	_, err = third.ops.ResolvePending(*replay, nil)
	require.NoError(t, err)

	applied, err = enqueueCallResult(

		third.ctx, third.mgr.store,

		sessionID,
		configEditCallID,
		tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")

	require.NoError(t, third.ops.ClearPending(*replay))

	third.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(third.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!third.mgr.HasActiveLoop(sessionID)
	})
	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
}

// A staged change suspends: nothing is written until the loop has persisted the
// suspend and the daemon runs the apply.
func TestConfigTool_SuccessStagesAndSuspends(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	assert.Equal(t, toolConfig, h.configBytes(t), "nothing is written by the tool itself")
	assert.True(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.applier.Calls(h.sessionID))
	assert.Equal(t, 0, h.restartCount())

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restartCount(), "the apply asks the daemon to come back")
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")
	assert.True(t, h.mgr.applier.Has(h.sessionID), "the call stays open until its verdict arrives")
}

// Guard violations are ordinary tool errors: nothing staged, no suspend, no
// restart — the model can correct itself in the same turn.
func TestConfigTool_GuardViolationsAreImmediateErrors(t *testing.T) {
	h := newConfigHarness(t)

	t.Run("missing activation grant", func(t *testing.T) {
		h.recordCall(t, "c-grant", tool.IDConfigEdit)

		_, err := h.tools[tool.IDConfigEdit].Execute(
			tool.WithCallID(context.Background(), "c-grant"), configEditArgs(configHarnessCandidate),
		)
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend)
		assert.Contains(t, err.Error(), "/config")
	})

	t.Run("invalid document", func(t *testing.T) {
		err := h.grantedCall(t, "c-var", "providers: [unclosed\n")
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend, "a refusal must not suspend the session")
	})

	t.Run("empty document", func(t *testing.T) {
		err := h.grantedCall(t, "c-doc", "")
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend)
		assert.Contains(t, err.Error(), "document is required")
	})

	assert.False(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, 0, h.restartCount())
	assert.Equal(t, toolConfig, h.configBytes(t))
}

// The apply pipeline hands over a staged change exactly once, so a second run —
// after a verdict, or after a wake for some other reason — cannot repeat it.
func TestRunStagedApply_HandsOverExactlyOnce(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restartCount(), "the second pass finds nothing to apply")
}

// Two applies in sequence: the first is answered by its verdict, and only then
// does the session get to make another.
func TestConfigTool_TwoAppliesInSequence(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	// The daemon comes back and delivers the verdict.
	h.restart(t, "c1", tool.IDConfigEdit)
	assert.False(t, h.mgr.applier.Has(h.sessionID))

	require.ErrorIs(t, h.grantedCall(t, "c2", toolConfig), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	assert.Equal(t, 2, h.restartCount())
	assert.Contains(t, h.configBytes(t), "id: claude-sonnet-5\n      provider: work\n    - id: claude-opus-5")
}

// A call with no tool_call id has nothing to answer against; suspending would
// strand the session.
func TestConfigTool_RefusesWithoutACallID(t *testing.T) {
	h := newConfigHarness(t)

	_, err := h.tools[tool.IDConfigEdit].Execute(
		tool.WithActivationGrant(context.Background(), tool.ActivationGrant{
			SessionID: h.sessionID, ToolID: tool.IDConfigEdit, Command: "/config",
		}),
		configEditArgs(configHarnessCandidate),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call id")
}

// config_edit lives on every root session with an applier, never on children.
// The tool description states the restart contract a caller cannot see.
func TestConfigTool_DescriptionsCarryTheContract(t *testing.T) {
	h := newConfigHarness(t)

	assert.Contains(t, h.tools[tool.IDConfigEdit].Description(), "restarts the daemon")
}

// Two config changes in one turn: the second is refused outright. An apply ends
// in a restart, so only the change staged against the config the daemon comes
// back on can be trusted — and a silently dropped second one would strand the
// call that made it.
func TestConfigTool_RefusesASecondApplyInTheSameTurn(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	err := h.grantedCall(t, "c2", toolConfig)
	require.Error(t, err)
	require.NotErrorIs(t, err, tool.ErrSuspend, "a refused stage must not suspend a second call")
	assert.Contains(t, err.Error(), "one change at a time")

	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.applier.Calls(h.sessionID))

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	assert.Equal(t, 1, h.restartCount())
}

// The apply slot is one, daemon-wide. The marker, the config file and the
// restart an apply ends in are all global, so a second staged change — from any
// session — would overwrite the first and strand the call it belongs to.
// The whole document replaces the config: a literal credential and an extra
// model land in the file exactly as written.
func TestConfigTool_WholeDocumentReachesTheConfig(t *testing.T) {
	h := newConfigHarness(t)

	document := toolConfig + "    - id: claude-haiku-4-5\n      provider: work\n"
	require.ErrorIs(t, h.grantedCall(t, "c1", document), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Contains(t, h.configBytes(t), "id: claude-haiku-4-5")
}

func TestConfigTool_DeliversOneVerdictAfterRestart(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "tags-1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	h.restart(t, "tags-1", tool.IDConfigEdit)

	assert.False(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, 1, h.restartCount())
	messages, err := h.sessions.LoadActiveMessages(context.Background(), h.sessionID)
	require.NoError(t, err)
	var verdicts int
	for _, message := range messages {
		if message.ToolName == tool.IDConfigEdit && message.Content == "Config applied." {
			verdicts++
		}
	}
	assert.Equal(t, 1, verdicts)
}

func TestScenario_ConfigDocumentThroughHTTPPreservesModelIDs(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "intact response", true: "damaged response rejected"}[damaged],
			func(t *testing.T) {
				testConfigDocumentThroughHTTP(t, damaged)
			},
		)
	}
}

// TestSetModelRecordsTheEffortTheNextRunSends drives a switch to a reasoning model
// without naming a level: the model's own default is what the session runs, so it
// is what the record must carry — the record is the only thing the next run reads.
func TestSetModelRecordsTheEffortTheNextRunSends(t *testing.T) {
	provider := newEffortProvider(t)
	h := newEffortHarness(t, provider.url)

	defer h.shutdown()

	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "plain-model", nil)
	require.NoError(t, err)
	// The two-phase check spends a hidden candidate and a confirmation, both
	// "answer N" from the stub, so one visible turn is two assistant rows.
	h.waitUntil("first turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 2
	})
	h.mgr.waitIdle(id)

	require.NoError(t, h.mgr.SetModel(h.ctx, id, "thinker", ""))

	rec, err := h.sessStore.GetSession(h.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"an unnamed level settles on the model's default, and the record keeps that")

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn answered", func() bool {
		return countAssistantReplies(h.parentMessages(id)) == 4
	})
	h.mgr.waitIdle(id)

	reqs := provider.snapshot()
	require.Len(t, reqs, 4)
	assert.Equal(t, "plain-model", reqs[0].model)
	assert.Empty(t, reqs[0].effort, "a model with no effort selector carries none")

	assert.Equal(t, "thinker", reqs[2].model)
	assert.Equal(t, "high", reqs[2].effort,
		"the run recreated from the record must ask for the level the switch settled on")
}
