package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChildDepth_ReadErrorCancelsSpawn: a ledger read failure must not read as
// "parent has no link" — that would reset the nesting depth to 1 and let a spawn
// through that the cap should have rejected.
func TestChildDepth_ReadErrorCancelsSpawn(t *testing.T) {
	var flaky *flakyLinkStore

	h := newSubagentHarnessDecorated(t, trivialRespond, func(inner LinkStore) LinkStore {
		flaky = newFlakyLinkStore(inner)
		return flaky
	})
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	flaky.failGetLink(1, 0)

	_, err = h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.ErrorIs(t, err, errLinkRead)
}

// TestChildDepth_NoLinkKeepsDepthOne: the "no row" branch is the normal path for
// every root session and must stay untouched.
func TestChildDepth_NoLinkKeepsDepthOne(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	defer h.shutdown()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	depth, err := h.mgr.childDepth(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, depth)

	child, err := h.mgr.Spawn(h.ctx, spawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)

	link, err := h.links.GetLink(h.ctx, child.ChildID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, 1, link.Depth)
}
