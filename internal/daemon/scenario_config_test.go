package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// The global apply guard prevents a second session from overwriting the first marker and stranding its tool call.
func TestScenario_ASecondSessionCannotOverwriteAStagedApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)
	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	first.startInboxWake()
	sessionA, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_A switch the default model", "fake-model", managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	first.waitUntil("A's opener settled", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionA, configapply.ConfigEditCommand))
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("A suspended on its config call", func() bool { return !first.mgr.HasActiveLoop(sessionA) })

	// B stages against the config A is already restarting into.
	first.startInboxWake()
	sessionB, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_B switch the default model", "fake-model", managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	first.waitUntil("B's opener settled", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionB, configapply.ConfigEditCommand))
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, sessionA, pending.SessionID, "the marker still names the session that is owed a verdict")
	assert.Equal(t, applyCallA, pending.ToolCallID)
	assert.Equal(t, "claude-opus-5", defaultModelInFile(t, configDir), "A's change is the one on disk")
	assert.Zero(t, first.restartCount(), "a refused stage never asks for a second restart")
	msgsB := first.messages(sessionB)
	require.NoError(t, llm.ValidateToolPairing(msgsB))
	assert.Equal(t, 1, countToolResultsFor(msgsB, tool.IDConfigEdit), "B is answered in-process")
	assert.Contains(t, lastToolResultContent(msgsB, tool.IDConfigEdit), "config change")
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	require.NoError(t, second.mgr.Start(second.ctx))
	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	second.waitUntil("config result", func() bool {
		return hasToolResultFor(second.messages(sessionA), tool.IDConfigEdit) && !second.mgr.HasActiveLoop(sessionA)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionA) })
	msgsA := second.messages(sessionA)
	require.NoError(t, llm.ValidateToolPairing(msgsA))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsA, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsA, tool.IDConfigEdit), "A's call is resolved exactly once")
	assert.Contains(t, lastToolResultContent(msgsA, tool.IDConfigEdit), "Config applied")
}

// The session record must explain why boot refuses a verdict that can never reach its original call.
func TestScenario_AVerdictOwedToAKilledSessionIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)
	sessionID := stageApplyAndStop(t, dbPath, configDir)
	second := newApplyDaemon(t, dbPath, configDir)
	require.NoError(t, second.mgr.Start(second.ctx))
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "/kill"))
	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	_, err = enqueueCallResult(
		second.ctx, second.mgr.store, pending.SessionID, pending.ToolCallID, pending.ToolName, "Config applied",
	)
	require.Error(t, err, "a killed session can never take the verdict")
	rec := second.session(sessionID)
	assert.NotNil(t, rec.KilledAt, "the record is what the boot reads to tell 'never' from 'not now'")
}

// Concurrent applies have one marker owner; the loser is refused inline and neither call remains dangling.
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
			id, err := first.mgr.Send(first.ctx, first.projectID, prompt, "fake-model", managerAttrs("telegram:main"))
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
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	for _, session := range []int64{sessionA, sessionB} {
		first.startInboxWake()
		require.NoError(t, first.mgr.sendToSession(first.ctx, session, configapply.ConfigEditCommand))
	}
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
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
	msgsLoser := first.messages(loser)
	require.NoError(t, llm.ValidateToolPairing(msgsLoser))
	assert.Equal(t, 1, countToolResultsFor(msgsLoser, tool.IDConfigEdit),
		"the refused call is answered rather than suspended")
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	require.NoError(t, second.mgr.Start(second.ctx))
	_, err = second.bootVerdict(t)
	require.NoError(t, err)
	second.waitUntil("config result", func() bool {
		return hasToolResultFor(second.messages(winner), tool.IDConfigEdit) && !second.mgr.HasActiveLoop(winner)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(winner) })
	msgsWinner := second.messages(winner)
	require.NoError(t, llm.ValidateToolPairing(msgsWinner))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsWinner, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsWinner, tool.IDConfigEdit))
}

// An apply marker promises one exact post-restart answer, so an unbacked suspend must never commit it.
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
	assert.True(t, h.mgr.applier.ClaimApply(), "the slot is free for the next change")
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
	require.NoError(t, d.mgr.Start(d.ctx))
	sessionID, err := d.mgr.Send(d.ctx, d.projectID, "say hello", "fake-model", nil)
	require.NoError(t, err)
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	staged, v := d.ops.StageDocument([]byte(toolConfig))
	require.False(t, v.Failed(), "%s", v.Reason())
	require.False(t, d.ops.Commit(staged, configops.Pending{
		SessionID: sessionID, ToolCallID: "ghost-call", ToolName: tool.IDConfigEdit,
	}).Failed())
	before := len(d.messages(sessionID))
	_, err = d.bootVerdict(t)
	require.NoError(t, err)
	d.waitUntil("stale verdict resolved", func() bool {
		pending, err := d.store.ListPending(d.ctx, sessionID)
		return err == nil && len(pending) == 0 && !d.mgr.HasActiveLoop(sessionID)
	})
	var state string
	require.NoError(
		t,
		d.db.QueryRowContext(
			d.ctx, `SELECT state FROM session_inbox WHERE session_id=? AND delivery_key='config_apply:ghost-call'`,
			sessionID,
		).
			Scan(&state),
	)
	assert.Equal(t, string(sessionstore.InputStateRejected), state)
	assert.Len(t, d.messages(sessionID), before)
	assert.Zero(t, countToolResultsFor(d.messages(sessionID), tool.IDConfigEdit))
	still, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, still, "a committed stale verdict must not arm later unrelated rollback")
	assert.NoError(t, llm.ValidateToolPairing(d.messages(sessionID)))
}

// The verdict for a session-owned apply is produced by a different process image
// than the one that suspended the call. It must still reach that exact call.
func TestScenario_ConfigApplyVerdictReachesTheSessionAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)
	sessionID := stageApplyAndStop(t, dbPath, configDir)
	second := newApplyDaemon(t, dbPath, configDir)
	require.NoError(t, second.mgr.Start(second.ctx))
	outcome, err := second.bootVerdict(t)
	require.NoError(t, err, "the verdict must reach the session that suspended")
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)
	second.waitUntil("config result", func() bool {
		return hasToolResultFor(second.messages(sessionID), tool.IDConfigEdit) && !second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	msgs := second.messages(sessionID)
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
	require.NoError(t, third.mgr.Start(third.ctx))
	_, err = third.bootVerdict(t)
	require.NoError(t, err)
	third.waitUntil("config result", func() bool {
		return hasToolResultFor(third.messages(sessionID), tool.IDConfigEdit) && !third.mgr.HasActiveLoop(sessionID)
	})
	third.waitUntil("session idle", func() bool { return !third.mgr.HasActiveLoop(sessionID) })
	msgs := third.messages(sessionID)
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
	require.NoError(t, second.mgr.Start(second.ctx))
	second.startInboxWake()
	require.NoError(t, second.mgr.sendToSession(second.ctx, sessionID, "are you done yet?"))
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	queued, err := second.store.PeekPending(second.ctx, sessionID)
	require.NoError(t, err)
	assert.Equal(t, "are you done yet?", queued.RawContent)
	msgs := second.messages(sessionID)
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit), "the apply was not repeated")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is still out with the world")
	assert.Zero(t, second.restartCount(), "a wake-up must not stage a second apply")

	// The queued message waited behind the call; the verdict still lands first.
	_, err = second.bootVerdict(t)
	require.NoError(t, err)
	second.waitUntil("config result", func() bool {
		return hasToolResultFor(second.messages(sessionID), tool.IDConfigEdit) && !second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	final := second.messages(sessionID)
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
	d.mgr.applier = configapply.New(failingCommitOps{d.ops}, d.store)
	sessionID := startConfigEditSession(t, d, "switch the default model")
	d.waitUntil("rejection stored", func() bool {
		return countToolResultsFor(d.messages(sessionID), tool.IDConfigEdit) == 1
	})
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	msgs := d.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "rejected")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Zero(t, d.restartCount(), "a rejected commit never restarts")
	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName))
}

// Restart replay inserts no duplicate verdict when a crash left the marker after the original delivery committed.
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
		second.ctx, second.mgr.store, sessionID, applyCallID, tool.IDConfigEdit, "Config applied: default model",
	)
	require.NoError(t, err)
	require.True(t, applied)

	// Dies after the injection, before the acknowledgement.
	second.shutdown()
	third := newApplyDaemon(t, dbPath, configDir)
	require.NoError(t, third.mgr.Start(third.ctx))
	replay, err := third.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay, "an unacknowledged verdict is replayed")
	_, err = third.ops.ResolvePending(*replay, nil)
	require.NoError(t, err)
	applied, err = enqueueCallResult(
		third.ctx, third.mgr.store, sessionID, applyCallID, tool.IDConfigEdit, "Config applied: default model",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")
	require.NoError(t, third.ops.ClearPending(*replay))
	third.waitUntil("config result", func() bool {
		return hasToolResultFor(third.messages(sessionID), tool.IDConfigEdit) && !third.mgr.HasActiveLoop(sessionID)
	})
	third.waitUntil("session idle", func() bool { return !third.mgr.HasActiveLoop(sessionID) })
	msgs := third.messages(sessionID)
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
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("config suspended", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})
	consumed := currentActivationOf(t, first.harness, sessionID)
	require.NotNil(t, consumed)
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State, "the successful apply spent the grant")

	// A second successful mutation cannot use the same grant: consuming again
	// with a different call id conflicts, and the session was never re-suspended.
	err := first.store.ConsumeActivationBinding(
		first.ctx, sessionstore.ActivationBinding{
			InputID: consumed.InputID, SessionID: sessionID,
			ToolID: tool.IDConfigEdit, Command: configapply.ConfigEditCommand, ToolCallID: "cfg-edit-call-2",
		})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)
}

// Boot spends an applied but unconsumed grant from its marker without expiring or re-arming the one-shot authorization.
func TestScenario_ConfigEditCrashBeforeGrantConsumeSettlesOnBoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("config suspended", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	first.shutdown()

	// Rewind the consumed grant to the durable pre-consume crash state: pending with no tool_call_id.
	_, err = first.db.Exec(`UPDATE session_tool_activations
		SET state = 'pending', tool_call_id = NULL, resolved_at = NULL
		WHERE session_id = ? AND tool_id = ?`, sessionID, tool.IDConfigEdit)
	require.NoError(t, err)

	// The crash window: nothing spent the grant before the image went down.
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	activation, err := second.store.CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationPending, activation.State,
		"the crash left the grant pending — the window under test")

	// The boot path runs without the apply process: resolve, spend, deliver.
	outcome, err := second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	require.False(t, outcome.RolledBack)
	second.mgr.applier.ConsumeConfigEditActivation(second.ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID)
	consumed, err := second.store.CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the boot spends the grant the crashed process left pending")

	// One-shot is not re-armed: a second settlement attempt must conflict.
	err = second.store.ConsumeActivationBinding(
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
		second.ctx, second.mgr.store, outcome.Pending.SessionID, outcome.Pending.ToolCallID, outcome.Pending.ToolName,
		message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))
	second.waitUntil("config result", func() bool {
		return hasToolResultFor(second.messages(sessionID), tool.IDConfigEdit) && !second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	msgs := second.messages(sessionID)
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
				return textReply("configuration unchanged")
			}
			if hasUserContaining(messages, "/config") {
				return callReply(
					"fragment", tool.IDConfigEdit,
					`{"document":"sandbox:\n  enabled: true\n  projects:\n    /tmp/project:\n      rules: []\n"}`,
				)
			}
			return textReply("ready")
		})
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	d.waitUntil("opening turn settled", func() bool { return !d.mgr.HasActiveLoop(id) })
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/config - is the sandbox unconfigured?"))
	d.waitUntil("invalid replacement answered", func() bool {
		return hasToolResultFor(d.messages(id), tool.IDConfigEdit) && !d.mgr.HasActiveLoop(id)
	})
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, d.restartCount())
	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending)
	assert.Contains(t, lastToolResultContent(d.messages(id), tool.IDConfigEdit), "configuration fragments")
}

// One authorized solo config_edit commits and spends its grant; only restart delivers the call's verdict.
func TestScenario_ConfigEditVerdictReachesTheSessionAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("config suspended", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, configEditCallID, pending.ToolCallID)
	require.Equal(t, tool.IDConfigEdit, pending.ToolName)
	msgs := first.messages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	require.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is out with the world")
	consumed := currentActivationOf(t, first.harness, sessionID)
	require.NotNil(t, consumed, "the /config grant exists")
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the successful apply spends the grant; it may not stay pending")
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	require.NoError(t, second.mgr.Start(second.ctx))
	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)
	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.messages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	msgs = second.messages(sessionID)
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

// Without a user /config turn the model sees config_edit but gets an authorization error before anything is staged.
func TestScenario_ConfigEditWithoutActivationNeverStages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	d := newApplyDaemonWith(t, dbPath, configDir, unauthorizedConfigEditRespond)
	require.NoError(t, d.mgr.Start(d.ctx))

	// The responder calls config_edit; no activation exists, so the session must
	// be answered with the authorization refusal, in-process.
	d.startInboxWake()
	sessionID, err := d.mgr.Send(
		d.ctx, d.projectID, "reconfigure the daemon", "fake-model", managerAttrs("telegram:main"),
	)
	require.NoError(t, err)
	d.waitUntil("refusal stored", func() bool {
		return countToolResultsFor(d.messages(sessionID), tool.IDConfigEdit) == 1
	})
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	msgs := d.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "/config")
	assert.Zero(t, d.restartCount(), "an unauthorized call never applies")
	assert.Equal(t, toolConfig, configBytesOf(t, configDir))
	assert.Nil(t, currentActivationOf(t, d.harness, sessionID))
}

// A syntactically valid candidate whose boot fails is rolled back and the
// rejection reaches the session — the same ResolvePending contract main.go runs.
func TestScenario_ConfigEditBootInvalidCandidateRollsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("config suspended", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "a staged candidate is committed before the restart")
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	// The second image comes up on a config it cannot serve: ResolvePending is
	// handed the boot error the real daemon would carry, and must roll back.
	outcome, err := second.ops.ResolvePending(*pending, errors.New("cold catalog: unknown model claude-opus-9"))
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Failed())
	assert.True(t, outcome.RolledBack)
	second.startInboxWake()
	message := "Config change rejected — " + outcome.Verdict.Reason()
	_, err = enqueueCallResult(
		second.ctx, second.mgr.store, outcome.Pending.SessionID, outcome.Pending.ToolCallID, outcome.Pending.ToolName,
		message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))
	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.messages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.waitUntil("session idle", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	msgs := second.messages(sessionID)
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
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("config suspended", func() bool {
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
		second.ctx, second.mgr.store, sessionID, configEditCallID, tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	require.True(t, applied)

	// Dies after the injection, before the acknowledgement.
	second.shutdown()
	third := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	require.NoError(t, third.mgr.Start(third.ctx))
	replay, err := third.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay, "an unacknowledged verdict is replayed")
	_, err = third.ops.ResolvePending(*replay, nil)
	require.NoError(t, err)
	applied, err = enqueueCallResult(
		third.ctx, third.mgr.store, sessionID, configEditCallID, tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")
	require.NoError(t, third.ops.ClearPending(*replay))
	third.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(third.messages(sessionID), tool.IDConfigEdit) == 1 &&
			!third.mgr.HasActiveLoop(sessionID)
	})
	third.waitUntil("session idle", func() bool { return !third.mgr.HasActiveLoop(sessionID) })
	msgs := third.messages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
}

// A staged change suspends: nothing is written until the loop has persisted the suspend and the daemon runs the apply.
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

// Two applies in sequence: the first is answered by its verdict, and only then does the session get to make another.
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

// A call with no tool_call id has nothing to answer against; suspending would strand the session.
func TestConfigTool_RefusesWithoutACallID(t *testing.T) {
	h := newConfigHarness(t)
	_, err := h.tools[tool.IDConfigEdit].Execute(
		tool.WithActivationGrant(context.Background(), tool.ActivationGrant{
			SessionID: h.sessionID, ToolID: tool.IDConfigEdit, Command: "/config",
		}), configEditArgs(configHarnessCandidate),
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

// A second apply in the same turn must be refused explicitly rather than silently stranding its call during restart.
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

// One global marker prevents competing applies; whole-document replacement preserves the supplied model and credential.
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

// A model switch persists its own default effort because the next run reads only the session record.
func TestSetModelRecordsTheEffortTheNextRunSends(t *testing.T) {
	provider := newEffortProvider(t)
	h := newHarness(
		t, harnessOptions{
			configure: withEffortModels(provider.url),
			clientFor: configuredClient(withEffortModels(provider.url)),
		},
	)
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "plain-model", nil)
	require.NoError(t, err)
	// The two-phase check spends a hidden candidate and a confirmation, both
	// "answer N" from the stub, so one visible turn is two assistant rows.
	h.waitUntil("first turn answered", func() bool {
		replyCount := 0
		for _, m := range h.messages(id) {
			if m.Role == llmwire.RoleAssistant && m.Content != "" {
				replyCount++
			}
		}
		return replyCount == 2
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	require.NoError(t, h.mgr.SetModel(h.ctx, id, "thinker", ""))
	rec := h.session(id)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"an unnamed level settles on the model's default, and the record keeps that")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn answered", func() bool {
		replyCount := 0
		for _, m := range h.messages(id) {
			if m.Role == llmwire.RoleAssistant && m.Content != "" {
				replyCount++
			}
		}
		return replyCount == 4
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	reqs := provider.snapshot()
	require.Len(t, reqs, 4)
	assert.Equal(t, "plain-model", reqs[0].model)
	assert.Empty(t, reqs[0].effort, "a model with no effort selector carries none")
	assert.Equal(t, "thinker", reqs[2].model)
	assert.Equal(t, "high", reqs[2].effort,
		"the run recreated from the record must ask for the level the switch settled on")
}

const (
	applyCallA = "cfg-call-a"
	applyCallB = "cfg-call-b"
)

// Distinct opener prompts and call IDs reveal a stranded verdict when two root sessions contend for one config apply.
func twoSessionApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	if !hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return textReply("ready to reconfigure")
	}
	if hasUserContaining(msgs, "APPLY_B") {
		return callReply(applyCallB, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidateB)+`}`)
	}
	return callReply(applyCallA, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidateA)+`}`)
}

const configEditCandidateA = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

const configEditCandidateB = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

func defaultModelInFile(t *testing.T, configDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	opus := strings.Index(string(body), "id: claude-opus-5")
	sonnet := strings.Index(string(body), "id: claude-sonnet-5")
	require.NotEqual(t, -1, opus)
	require.NotEqual(t, -1, sonnet)
	if opus < sonnet {
		return "claude-opus-5"
	}
	return "claude-sonnet-5"
}

// failingCommitOps is an ops layer whose write fails, so the apply is rejected
// with no restart and no marker — the verdict has to come back inline.
type failingCommitOps struct {
	configops.Service
}

func (failingCommitOps) Commit(*configops.Staged, configops.Pending) configops.Verdict {
	return configops.Reject("", errors.New("no space left on device"))
}

// configEditCallID is the call id the responder always uses, so the redelivery test can replay the exact same verdict.
const configEditCallID = "cfg-edit-call-1"

func testAbandonedConfigCommand(t *testing.T, restart bool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "abandoned-command.db")
	configDir := newApplyConfigDir(t)
	d := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer func() { d.shutdown() }()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	_, err = d.db.ExecContext(
		d.ctx,
		`CREATE TRIGGER reject_config_suspend BEFORE UPDATE OF status ON sessions WHEN NEW.status = 'suspended' BEGIN SELECT RAISE(ABORT, 'injected suspend failure'); END`,
	)
	require.NoError(t, err)
	if restart {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/config change the default model"))
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	d.waitUntil(
		"config grant expired",
		func() bool { return currentActivationOf(t, d.harness, id) == nil },
	)
	if !restart {
		d.waitUntil(
			"abandoned config result settled",
			func() bool { return hasToolResultFor(d.messages(id), tool.IDConfigEdit) },
		)
	}
	activation := currentActivationOf(t, d.harness, id)
	assert.Nil(t, activation)
	if restart {
		assert.False(t, hasToolResultFor(d.messages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		d.shutdown()
		d = newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
		require.NoError(t, d.mgr.Start(d.ctx))
		d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	} else {
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		assert.Contains(t, configLastToolResultContent(d.messages(id), tool.IDConfigEdit), "Config change abandoned")
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "continue after cancellation"))
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	assert.True(t, hasUserContaining(d.messages(id), "continue after cancellation"))
	assert.Equal(t, 1, countToolResultsFor(d.messages(id), tool.IDConfigEdit))
	assert.Zero(t, d.restartCount())
}

func testAbandonedApply(t *testing.T, failure string) {
	t.Helper()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "abandoned.db"), newApplyConfigDir(t),
		func(string, []llmwire.Message) *llmwire.Response { return textReply("ready") })
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", nil)
	require.NoError(t, err)
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: configEditCallID, Name: tool.IDConfigEdit}})
	require.NoError(t, err)
	_, err = d.store.Commit(d.ctx, sessionstore.Commit{SessionID: id, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	}}})
	require.NoError(t, err)
	_, err = configapply.NewConfigEdit(id, d.mgr.applier).Execute(
		grantedCall(d.ctx, id, configEditCallID), configEditArgs(configEditCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)
	configuredModels := d.mgr.build.Config.UnifiedConfig.Models
	d.mgr.build.Config.UnifiedConfig.Models = nil
	failWrite := failure == "write failure"
	if failWrite {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "keep this input"))
	if failure == "stop" || failure == "kill" {
		if failure == "stop" {
			require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/stop"))
			assert.Contains(t, configLastToolResultContent(d.messages(id), tool.IDConfigEdit), "Stopped by user")
		} else {
			require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/kill"))
			assert.False(t, hasToolResultFor(d.messages(id), tool.IDConfigEdit))
		}
		assert.Zero(t, d.restartCount())
		return
	}
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	d.waitUntil("failed apply settled", func() bool {
		record, err := d.store.GetSession(d.ctx, id)
		return err == nil && record.Status == sessionstore.SessionStatusError &&
			!d.mgr.HasActiveLoop(id) && (failWrite || !d.mgr.applier.Has(id))
	})
	pending, err := d.store.PeekPending(d.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	if failWrite {
		assert.True(t, d.mgr.applier.Has(id))
		assert.False(t, hasToolResultFor(d.messages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)
		d.mgr.build.Config.UnifiedConfig.Models = configuredModels
		require.NoError(t, d.mgr.sendToSession(d.ctx, id, "retry now"))
		d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
		_, err = d.store.PeekPending(d.ctx, id)
		require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
		assert.True(t, hasUserContaining(d.messages(id), "keep this input"))
	}
	assert.False(t, d.mgr.applier.Has(id))
	assert.Contains(t, configLastToolResultContent(d.messages(id), tool.IDConfigEdit), "Config change abandoned")
	assert.Zero(t, d.restartCount())
	apply, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, apply)
}

// The opener settles before durable /config authorization; each full-document edit receives one verdict without replay.
func configEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	if hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return callReply(configEditCallID, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidate)+`}`)
	}
	return textReply("ready to reconfigure")
}

// unauthorizedConfigEditRespond calls config_edit with no /config turn, so the
// session must answer the authorization refusal in-process.
func unauthorizedConfigEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	return callReply(configEditCallID, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidate)+`}`)
}

// Settle the opener before durable /config input so grant promotion cannot race the session's initial model call.
func startConfigEditSession(t *testing.T, d *applyDaemon, prompt string) int64 {
	t.Helper()
	d.startInboxWake()
	sessionID, err := d.mgr.Send(d.ctx, d.projectID, prompt, "fake-model", managerAttrs("telegram:main"))
	require.NoError(t, err)
	d.waitUntil("opener turn settled", func() bool {
		return !d.mgr.HasActiveLoop(sessionID)
	})
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	require.NoError(t, d.mgr.sendToSession(d.ctx, sessionID, configapply.ConfigEditCommand))
	return sessionID
}

// currentActivationOf returns the session's current grant. A missing grant is
// nil; any read failure fails the test instead of passing as absence.
func currentActivationOf(t *testing.T, h *harness, sessionID int64) *sessionstore.ToolActivation {
	t.Helper()
	activation, err := h.store.CurrentActivation(context.Background(), sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}
	require.NoError(t, err)
	return activation
}

func configBytesOf(t *testing.T, configDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	return string(body)
}

type configHarness struct {
	*harness
	// sessionID is a real session row: the apply pipeline reads its transcript
	// before it commits, so a config tool needs somewhere to have suspended.
	sessionID int64
	sessions  *sessionstore.Store
	factory   *mockFactory
	tools     map[string]tool.Tool
	restarts  int
	config    string
}

func newConfigHarness(t *testing.T) *configHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	secretsPath := filepath.Join(dir, "secrets")
	require.NoError(t, os.WriteFile(configPath, []byte(toolConfig), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, []byte(toolSecrets), 0o600))
	factory := &mockFactory{}
	base := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	h := &configHarness{harness: base, config: configPath}
	mgr, store := base.mgr, base.store
	sessions, ok := mgr.store.(*sessionstore.Store)
	require.True(t, ok)
	h.sessions = sessions
	h.factory = factory
	projectID, err := store.GetOrCreateProject(context.Background(), t.TempDir())
	require.NoError(t, err)
	h.projectID = projectID
	mgr.applier = configapply.New(configops.New(configPath, secretsPath), h.sessions)
	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = map[string]tool.Tool{tool.IDConfigEdit: configapply.NewConfigEdit(h.sessionID, mgr.applier)}
	return h
}

// configHarnessCandidate reorders the models, so staging it visibly changes the file.
const configHarnessCandidate = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

// grantedCall carries what config_edit demands: the call id plus the durable
// /config grant the tool revalidates before staging.
func grantedCall(ctx context.Context, sessionID int64, callID string) context.Context {
	ctx = tool.WithCallID(ctx, callID)
	return tool.WithActivationGrant(ctx, tool.ActivationGrant{
		SessionID: sessionID, ToolID: tool.IDConfigEdit, Command: "/config", ToolCallID: callID,
	})
}

func configEditArgs(document string) json.RawMessage {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(`{"document":` + string(encoded) + `}`)
}

// grantedCall runs config_edit the way the loop does: the assistant turn is
// persisted first, then the tool executes with the grant in context.
func (h *configHarness) grantedCall(t *testing.T, callID, document string) error {
	t.Helper()
	h.recordCall(t, callID, tool.IDConfigEdit)
	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(context.Background(), h.sessionID, callID), configEditArgs(document),
	)
	if errors.Is(err, tool.ErrSuspend) {
		require.NoError(
			t, h.sessions.UpdateSessionStatus(t.Context(), h.sessionID, sessionstore.SessionStatusSuspended),
		)
	}
	return err
}

// liveSession creates a real session record, so notification delivery has somewhere to land.
func (h *configHarness) liveSession(t *testing.T) int64 {
	t.Helper()
	rec := h.createRoot(map[string]any{"channel": "cli", "manager_id": "cli"})
	return rec
}

// recordCall appends the assistant turn a tool_call arrives in, which is what makes a later suspend durable.
func (h *configHarness) recordCall(t *testing.T, callID, toolName string) {
	t.Helper()
	activation, activationErr := h.sessions.PendingActivation(t.Context(), h.sessionID)
	if activationErr == nil && !h.mgr.applier.Has(h.sessionID) {
		_, err := h.sessions.Commit(t.Context(), sessionstore.Commit{
			SessionID: h.sessionID,
			Activation: &sessionstore.ActivationChange{
				InputID: activation.InputID,
				State:   sessionstore.ActivationExpired,
			},
		})
		require.NoError(t, err)
	}
	if !h.mgr.applier.Has(h.sessionID) {
		input, err := h.sessions.Enqueue(
			context.Background(),
			sessionstore.Input{SessionID: h.sessionID, Source: sessionstore.InputSourceUser, Content: "/config"},
		)
		require.NoError(t, err)
		_, err = h.sessions.Commit(
			context.Background(), sessionstore.Commit{
				SessionID: h.sessionID,
				Accept: []sessionstore.Accept{
					{
						InputID:    input.Input.ID,
						State:      sessionstore.InputStateAccepted,
						Content:    "/config",
						LinkRef:    -1,
						ModelBound: true,
					},
				},
				Activation: &sessionstore.ActivationChange{
					InputID: input.Input.ID,
					State:   sessionstore.ActivationPending,
					ToolID:  toolName,
					Command: "/config",
				},
			},
		)
		require.NoError(t, err)
	}
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: toolName}})
	require.NoError(t, err)
	_, err = h.sessions.Commit(
		context.Background(), sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:      llmwire.RoleAssistant,
			ToolCalls: calls,
		}}},
	)
	require.NoError(t, err)
}

// restart models the boot a committed apply causes: the new process image comes
// up with a free apply slot and delivers the verdict the call was waiting for.
func (h *configHarness) restart(t *testing.T, callID, toolName string) {
	t.Helper()
	h.restarts += len(h.mgr.applier.Restart())
	h.mgr.applier.ReleaseApply()
	h.mgr.applier = configapply.New(h.mgr.applier.Ops(), h.sessions)
	h.tools[tool.IDConfigEdit] = configapply.NewConfigEdit(h.sessionID, h.mgr.applier)
	_, err := h.sessions.Commit(
		context.Background(), sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:       llmwire.RoleTool,
			ToolCallID: callID,
			ToolName:   toolName,
			Content:    "Config applied.",
		}}},
	)
	require.NoError(t, err)
}

func (h *configHarness) restartCount() int { return h.restarts + len(h.mgr.applier.Restart()) }

func (h *configHarness) configBytes(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.config)
	require.NoError(t, err)
	return string(data)
}

type configWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
}

func testConfigDocumentThroughHTTP(t *testing.T, damaged bool) {
	t.Helper()
	initial := strings.ReplaceAll(toolConfig, "claude-sonnet-5", "anthropic/claude-sonnet-4.6")
	initial = strings.ReplaceAll(initial, "claude-opus-5", "anthropic/claude-opus-4.6")
	candidate := initial + "sandbox:\n    rules:\n        - deny: ~/.ssh\n"
	if damaged {
		candidate = strings.ReplaceAll(candidate, "claude-sonnet-4.6", "")
		candidate = strings.ReplaceAll(candidate, "claude-opus-4.6", "")
	}
	configDir := newApplyConfigDirWith(t, initial)
	configPath := filepath.Join(configDir, "config.yaml")
	var sawRead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []configWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "ready"}
		finish := "stop"
		command, read, edited := false, false, false
		for _, input := range request.Messages {
			command = command || input.Role == "user" && strings.Contains(input.Content, "/config")
			if input.Role == "tool" && input.Name == "read" {
				read = true
				sawRead.Store(true)
				assert.Contains(t, input.Content, "anthropic/claude-sonnet-4.6")
				assert.Contains(t, input.Content, "anthropic/claude-opus-4.6")
			}
			edited = edited || input.Role == "tool" && input.Name == tool.IDConfigEdit
		}
		if command && !edited {
			name, args := "read", map[string]string{"file_path": configPath}
			if read {
				name, args = tool.IDConfigEdit, map[string]string{"document": candidate}
			}
			encoded, err := json.Marshal(args)
			assert.NoError(t, err)
			message["tool_calls"] = []any{map[string]any{
				"id": name + "-call", "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)},
			}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		}))
	}))
	defer server.Close()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "transport.db"), configDir, configEditRespond)
	workDir, err := d.mgr.store.GetProjectWorkDir(d.ctx, d.projectID)
	require.NoError(t, err)
	wireConfig := &config.Config{Model: "fake-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{"test": {Driver: "openai", BaseURL: server.URL, APIKey: "test-key"}},
		Models:    []config.ModelEntry{{ID: "fake-model", Provider: "test", MaxTokens: 8192, ContextWindow: 100000}},
	}}
	d.mgr.build.Config = wireConfig
	d.mgr.build.WorkDir = workDir
	id := startConfigEditSession(t, d, "hello")
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	assert.True(t, sawRead.Load())
	var storedDocuments []string
	for _, message := range d.messages(id) {
		for _, call := range message.ToolCalls {
			if call.Name == tool.IDConfigEdit {
				var args struct {
					Document string `json:"document"`
				}
				require.NoError(t, json.Unmarshal(call.Arguments, &args))
				storedDocuments = append(storedDocuments, args.Document)
			}
		}
	}
	assert.Equal(t, []string{candidate}, storedDocuments)
	actual, err := os.ReadFile(configPath)
	require.NoError(t, err)
	if damaged {
		assert.Equal(t, initial, string(actual))
		assert.Zero(t, d.restartCount())
		assert.Contains(t, configLastToolResultContent(d.messages(id), tool.IDConfigEdit), "duplicate model id")
		return
	}
	assert.Equal(t, candidate, string(actual))
	assert.Equal(t, 1, d.restartCount())
}

func lastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

// effortRequest is what one provider call named: its model and reasoning effort.
type effortRequest struct {
	model  string
	effort string
}

func withEffortModels(baseURL string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Model = "plain-model"
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL}},
			Models: []config.ModelEntry{
				{ID: "plain-model", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
				{
					ID: "thinker", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
					EffortLevels:  []string{"low", "high"},
					DefaultEffort: "high",
					Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: []string{"low", "high"}},
				},
			},
		}
	}
}

// effortProvider is an OpenRouter-shaped endpoint recording the reasoning effort
// of every call, so what a session actually asks for is observable.
type effortProvider struct {
	url string

	mu       sync.Mutex
	requests []effortRequest
}

func newEffortProvider(t *testing.T) *effortProvider {
	t.Helper()
	p := &effortProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		p.mu.Lock()
		p.requests = append(p.requests, effortRequest{model: body.Model, effort: body.Reasoning.Effort})
		turn := len(p.requests)
		p.mu.Unlock()
		_, _ = fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"answer %d"},"finish_reason":"stop"}],"usage":{}}`,
			turn,
		)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (p *effortProvider) snapshot() []effortRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]effortRequest(nil), p.requests...)
}

func configLastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}
