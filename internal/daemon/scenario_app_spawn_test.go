package daemon

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/projectpath"
)

func TestAppSpawnRefusals(t *testing.T) {
	h := orchestratorModelHarness(t)
	_, path := orchestratorProject(t, h, "private-project-name")
	unknown := t.TempDir()
	for _, selection := range []string{unknown, "private-project-name"} {
		_, err := h.mgr.SpawnForApp(h.ctx, "bot", selection, "", "")
		require.EqualError(t, err, fmt.Sprintf("no project at %q", selection))
		if selection == unknown {
			assert.NotContains(t, err.Error(), "private-project-name")
		}
	}
	worktree := projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(h.mgr.build.Config.UnifiedConfig), path, "existing")
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	_, err := h.store.GetOrCreateNamedProject(h.ctx, worktree, "private-project-name/existing")
	require.NoError(t, err)
	_, err = h.mgr.SpawnForApp(h.ctx, "bot", worktree, "", "")
	require.EqualError(t, err, fmt.Sprintf(
		"project %q is a worktree; name its base project and pass worktree to create a fresh one", worktree))
	_, err = h.mgr.SpawnForApp(h.ctx, "bot", path, "", "plain")
	require.EqualError(
		t,
		err,
		`model "plain" is not an advertised tagged candidate; choose one of: fast, deep, or omit model for the daemon's default model`,
	)
	held := 0
	t.Cleanup(func() {
		for held > 0 {
			h.mgr.runners.release(false, 0)
			held--
		}
	})
	for range maxTotal - spawnHeadroom + 1 {
		require.True(t, h.mgr.runners.tryAdmit(false, 0))
		held++
	}
	before := orchestratorSessionCount(t, h)
	_, err = h.mgr.SpawnForApp(h.ctx, "bot", path, "", "")
	require.EqualError(t, err, fmt.Sprintf(
		"daemon is near its session capacity (%d of %d running); try again after some sessions finish", held, maxTotal))
	assert.Equal(t, before, orchestratorSessionCount(t, h))
}
