package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/projectpath"
)

func TestOrchestratorSpawnRegisteredProjects(t *testing.T) {
	h := orchestratorProjectHarness(t)
	h.startInboxWake()
	caller := modeRoot(t, h)
	external := t.TempDir()
	projectID, err := h.store.GetOrCreateNamedProject(h.ctx, external, "external")
	require.NoError(t, err)
	worktree := projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(h.mgr.build.Config.UnifiedConfig),
		external,
		"existing",
	)
	require.NoError(t, os.MkdirAll(worktree, 0o755))
	_, err = h.store.GetOrCreateNamedProject(h.ctx, worktree, "external/existing")
	require.NoError(t, err)
	vanished := filepath.Join(t.TempDir(), "vanished")
	_, err = h.store.GetOrCreateNamedProject(h.ctx, vanished, "vanished")
	require.NoError(t, err)
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, []byte("not a directory"), 0o600))
	_, err = h.store.GetOrCreateNamedProject(h.ctx, file, "file")
	require.NoError(t, err)

	for _, selection := range []string{"EXTERNAL", external} {
		t.Run("visible repository "+selection, func(t *testing.T) {
			id, name, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, selection, "", "external work")
			require.NoError(t, err)
			assert.Equal(t, "external", name)
			h.waitAnswer(id, "done")
			assertSpawnedRoot(t, h, caller, id, projectID, "external work")
		})
	}

	for _, tc := range []struct{ selection, want string }{
		{"external/existing", `project "external/existing" is a worktree; name its base project and pass worktree to create a fresh one`},
		{worktree, fmt.Sprintf("project %q is a worktree; name its base project and pass worktree to create a fresh one", worktree)},
		{"vanished", fmt.Sprintf("project %q directory is missing: %s; open an existing project instead", "vanished", vanished)},
		{"file", fmt.Sprintf("project %q path is not a directory: %s; open a project directory instead", "file", file)},
		{"unknown", `no project named "unknown"; known projects: external, initial`},
	} {
		t.Run(tc.selection, func(t *testing.T) {
			before := orchestratorSessionCount(t, h)
			_, _, err := (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, tc.selection, "", "do work")
			require.EqualError(t, err, tc.want)
			assert.Equal(t, before, orchestratorSessionCount(t, h))
		})
	}
}
