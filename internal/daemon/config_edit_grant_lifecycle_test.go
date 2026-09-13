package daemon

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configtools"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

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
	err := first.sessStore.(sessionstore.ActivationStore).ConsumeActivationBinding(
		first.ctx, sessionstore.ActivationBinding{
			InputID: consumed.InputID, SessionID: sessionID,
			ToolID: tool.IDConfigEdit, Command: configtools.ConfigEditCommand, ToolCallID: "cfg-edit-call-2",
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

	activation, err := second.sessStore.(sessionstore.ActivationStore).
		CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationPending, activation.State,
		"the crash left the grant pending — the window under test")

	// The boot path runs without the apply process: resolve, spend, deliver.
	outcome, err := second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	require.False(t, outcome.RolledBack)

	second.mgr.ConsumeConfigEditActivation(
		second.ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID,
	)

	consumed, err := second.sessStore.(sessionstore.ActivationStore).
		CurrentActivation(second.ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the boot spends the grant the crashed process left pending")

	// One-shot is not re-armed: a second settlement attempt must conflict.
	err = second.sessStore.(sessionstore.ActivationStore).ConsumeActivationBinding(
		second.ctx, sessionstore.ActivationBinding{
			InputID: consumed.InputID, SessionID: sessionID,
			ToolID: tool.IDConfigEdit, Command: configtools.ConfigEditCommand, ToolCallID: "cfg-edit-call-2",
		})
	require.ErrorIs(t, err, sessionstore.ErrActivationConflict)

	// The verdict still reaches the session, and the receipt says the config
	// changed — the expiry receipt's "was not changed" would have been a lie.
	require.NoError(t, second.mgr.Start(second.ctx))

	message := "Config applied: " + outcome.Pending.Summary
	_, err = second.mgr.DeliverPendingCallResult(
		second.ctx, outcome.Pending.SessionID,
		outcome.Pending.ToolCallID, outcome.Pending.ToolName, message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))

	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
}
