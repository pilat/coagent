package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

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
