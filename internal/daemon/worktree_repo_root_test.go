package daemon

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
)

func TestSessionRepoRoot_InheritedAcrossDurableSubagentTree(t *testing.T) {
	h := newSubagentHarness(t)
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       root.ID,
					RootID:         root.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	grandchildID, err := func() (int64, error) {
		var id int64
		err := h.sessStore.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       childID,
					RootID:         root.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)

	for _, sessionID := range []int64{root.ID, childID, grandchildID} {
		rec, loadErr := h.sessStore.GetSession(h.ctx, sessionID)
		require.NoError(t, loadErr)
		got, rootErr := h.mgr.sessionRepoRoot(h.ctx, rec)
		require.NoError(t, rootErr)
		assert.Equal(t, "/source", got)
	}

	child, err := h.sessStore.GetSession(h.ctx, childID)
	require.NoError(t, err)
	assert.Empty(t, child.Attributes["repo_root"])

	plain, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	got, err := h.mgr.sessionRepoRoot(h.ctx, plain)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestSessionRepoRoot_RejectsCrossProjectRoot(t *testing.T) {
	h := newSubagentHarness(t)
	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"repo_root": "/source",
	})
	require.NoError(t, err)
	other := &sessionstore.SessionRecord{RootID: root.ID, ProjectID: h.projectID + 1}
	_, err = h.mgr.sessionRepoRoot(h.ctx, other)
	require.ErrorContains(t, err, "does not match project")
}
