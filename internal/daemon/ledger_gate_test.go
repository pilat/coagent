package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/subagent"
)

// TestLedgerFailure_SpawnRefusesInsteadOfDegrading is the summary gate: with the
// ledger unreadable the spawn must be REFUSED, not quietly granted at depth 1,
// outside the parent quota and without a wall-clock timeout.
func TestLedgerFailure_SpawnRefusesInsteadOfDegrading(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	// Healthy store: the same request succeeds — this gate must not simply refuse
	// everything.
	h.startInboxWake()
	ok, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)
	require.NotZero(t, ok.ChildID)
	h.waitForDelivery(ok.ChildID)
	h.mgr.waitIdle(ok.ChildID)
	h.waitUntil("healthy child runner removed", func() bool { return runnerCount(h.mgr.runners) == 0 })

	loopsBefore := runnerCount(h.mgr.runners)

	childrenBefore := runnerChildCount(h.mgr.runners)

	flaky.failGetLink(1, 0)

	// Assert on the returned error, not on HasActiveLoop: the spawn dies in
	// childDepth before the child session exists, so there is no id to look up.
	h.startInboxWake()
	res, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.Error(t, err)
	assert.Equal(t, subagent.ChildResult{}, res)

	loopsAfter := runnerCount(h.mgr.runners)

	assert.Equal(t, loopsBefore, loopsAfter, "no runner was started")
	assert.Equal(t, childrenBefore, runnerChildCount(h.mgr.runners), "no child slot was taken")
}
