package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLedgerFailure_SpawnRefusesInsteadOfDegrading is the summary gate: with the
// ledger unreadable the spawn must be REFUSED, not quietly granted at depth 1,
// outside the parent quota and without a wall-clock timeout.
func TestLedgerFailure_SpawnRefusesInsteadOfDegrading(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner LinkStore) LinkStore {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	// Healthy store: the same request succeeds — this gate must not simply refuse
	// everything.
	ok, err := h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)
	require.NotZero(t, ok.ChildID)
	h.waitForDelivery(ok.ChildID)
	h.mgr.waitIdle(ok.ChildID)

	h.mgr.mu.Lock()
	loopsBefore := len(h.mgr.loops)
	h.mgr.mu.Unlock()

	childrenBefore := h.mgr.admit.liveChildren()

	flaky.failGetLink(1, 0)

	// Assert on the returned error, not on HasActiveLoop: the spawn dies in
	// childDepth before the child session exists, so there is no id to look up.
	res, err := h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.Error(t, err)
	assert.Equal(t, childResult{}, res)

	h.mgr.mu.Lock()
	loopsAfter := len(h.mgr.loops)
	h.mgr.mu.Unlock()

	assert.Equal(t, loopsBefore, loopsAfter, "no runner was started")
	assert.Equal(t, childrenBefore, h.mgr.admit.liveChildren(), "no child slot was taken")
}
