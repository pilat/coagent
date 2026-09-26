package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestSessionRepoRoot_InheritedAcrossDurableSubagentTree(t *testing.T) {
	h := newSubagentHarness(t)
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
		controllerapi.SessionAttributeWorktreeOrigin: controllerapi.WorktreeOriginController,
	})
	require.NoError(t, err)
	childID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, root.ID, root.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)
	grandchildID, err := h.sessStore.CreateSubagentSession(
		h.ctx, h.projectID, childID, root.ID, "general", "fake-model", "",
	)
	require.NoError(t, err)

	for _, sessionID := range []int64{root.ID, childID, grandchildID} {
		rec, loadErr := h.sessStore.GetSession(h.ctx, sessionID)
		require.NoError(t, loadErr)
		got, created, rootErr := h.mgr.sessionRepoRoot(h.ctx, rec)
		require.NoError(t, rootErr)
		assert.Equal(t, "/source", got)
		assert.True(t, created)
	}

	child, err := h.sessStore.GetSession(h.ctx, childID)
	require.NoError(t, err)
	assert.Empty(t, child.Attributes["repo_root"])

	plain, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	got, created, err := h.mgr.sessionRepoRoot(h.ctx, plain)
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.False(t, created)

	legacy, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	got, created, err = h.mgr.sessionRepoRoot(h.ctx, legacy)
	require.NoError(t, err)
	assert.Equal(t, "/source", got)
	assert.False(t, created)
}

func TestSessionRepoRoot_RejectsCrossProjectRoot(t *testing.T) {
	h := newSubagentHarness(t)
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	other := &sessionstore.SessionRecord{RootID: root.ID, ProjectID: h.projectID + 1}
	_, _, err = h.mgr.sessionRepoRoot(h.ctx, other)
	require.ErrorContains(t, err, "does not match project")
}
