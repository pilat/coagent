package daemon

import (
	"context"
	"encoding/json"
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
	_, err := h.tools[tool.IDSetDefaultModel].Execute(
		tool.WithCallID(ctx, "c1"), json.RawMessage(`{"id":"claude-opus-5"}`),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	h.mgr.runStagedApply(ctx, h.sessionID)

	assert.Equal(t, 0, h.restarts, "an unbacked suspend must not restart the daemon")
	assert.Equal(t, toolConfig, h.configBytes(t), "and must not write the config")

	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending, "no marker is armed for a call no boot could answer")

	assert.False(t, h.mgr.staged.has(h.sessionID), "the call is settled in-process, not across a restart")
	assert.True(t, h.mgr.stageApply(h.sessionID, "c2", tool.IDAddModel, &configops.Staged{}),
		"the slot is free for the next change")
}

// The same session may go on to make a change that does suspend durably, and it
// must be applied normally — the refusal above is about one call, not the session.
func TestRunStagedApply_ADurableSuspendAfterAnUnbackedOneStillApplies(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	_, err := h.tools[tool.IDSetDefaultModel].Execute(
		tool.WithCallID(ctx, "c1"), json.RawMessage(`{"id":"claude-opus-5"}`),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	require.ErrorIs(t, h.call(t, tool.IDSetDefaultModel, "c2", `{"id":"claude-opus-5"}`), tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	assert.Equal(t, 1, h.restarts)
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")

	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, "c2", pending.ToolCallID, "the marker names the call that really suspended")
}

// Why the pipeline checks before it commits: a marker naming a call the
// transcript does not carry can never be consumed. Delivery fails, the session is
// alive so the failure reads as transient, and the marker is kept — arming every
// later boot to roll a config that has been live for days back to its backup.
func TestScenario_AMarkerForACallTheTranscriptDoesNotCarryIsNeverConsumed(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	d := newApplyDaemonWith(t, dbPath, configDir, plainRespond)
	defer d.shutdown()

	require.NoError(t, d.mgr.Start(d.ctx))

	sessionID, err := d.mgr.Send(d.ctx, d.projectID, "say hello", "fake-model", nil)
	require.NoError(t, err)
	d.mgr.waitIdle(sessionID)

	staged, v := d.ops.Stage(configops.SetDefaultModel("claude-opus-5"))
	require.False(t, v.Failed(), "%s", v.Reason())
	require.False(t, d.ops.Commit(staged, configops.Pending{
		SessionID: sessionID, ToolCallID: "ghost-call", ToolName: tool.IDSetDefaultModel,
	}).Failed())

	_, err = d.mgr.DeliverPendingCallResult(
		d.ctx, sessionID, "ghost-call", tool.IDSetDefaultModel, "Config applied: default model",
	)
	require.Error(t, err, "there is no such call in the transcript to answer")

	// The boot keeps a marker whose delivery failed for a session that is still
	// able to take it, so this exact failure repeats on every later boot.
	rec, err := d.mgr.sessionStore.GetSession(d.ctx, sessionID)
	require.NoError(t, err)
	require.Nil(t, rec.KilledAt)
	require.NotEqual(t, sessionstore.SessionStatusStopped, rec.Status)
	require.NotEqual(t, sessionstore.SessionStatusStopping, rec.Status)

	still, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.NotNil(t, still, "the marker outlives the boot that could not consume it")

	assert.NoError(t, llm.ValidateToolPairing(d.parentMessages(sessionID)),
		"the failed delivery must not damage the transcript")
}
