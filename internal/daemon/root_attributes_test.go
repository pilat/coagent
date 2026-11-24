package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestRootCreationWithoutAttributesStoresAnEmptyObject(t *testing.T) {
	t.Run("without worktree", func(t *testing.T) {
		h := newHarness(t, harnessOptions{respond: trivialRespond})
		id, err := h.mgr.CreateSession(h.ctx, h.projectID, "", "", "fake-model", nil)
		require.NoError(t, err)
		var attributes string
		require.NoError(t, h.db.QueryRowContext(h.ctx,
			"SELECT attributes FROM sessions WHERE id = ?", id).Scan(&attributes))
		assert.Equal(t, "{}", attributes)
	})
	t.Run("with worktree", func(t *testing.T) {
		clone := orchestratorClone(t)
		worktrees := t.TempDir()
		h := newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
			cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: t.TempDir(), WorktreesRoot: worktrees}
		}})
		project, err := h.store.GetOrCreateProject(h.ctx, clone)
		require.NoError(t, err)
		id, err := h.mgr.CreateSession(h.ctx, project, "api", "", "fake-model", nil)
		require.NoError(t, err)
		attributes := h.session(id).Attributes
		assert.Equal(t, clone, attributes["repo_root"])
		assert.Equal(
			t,
			controllerapi.WorktreeOriginController,
			attributes[controllerapi.SessionAttributeWorktreeOrigin],
		)
	})
}
