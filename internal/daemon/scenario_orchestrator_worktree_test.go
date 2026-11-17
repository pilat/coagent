package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
)

func TestOrchestratorSpawnWorktree(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprint(refusal), func(t *testing.T) {
			worktrees := t.TempDir()
			clone := orchestratorClone(t)
			h := newHarness(t, harnessOptions{respond: trivialRespond, configure: func(cfg *config.Config) {
				cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: filepath.Dir(clone), WorktreesRoot: worktrees}
			}})
			h.startInboxWake()
			caller := modeRoot(t, h)
			_, err := h.store.GetOrCreateProject(h.ctx, clone)
			require.NoError(t, err)
			marker := filepath.Join(t.TempDir(), "executed")
			script := filepath.Join(t.TempDir(), "monitor")
			require.NoError(
				t,
				os.WriteFile(
					script,
					fmt.Appendf(nil, "#!/bin/sh\nprintf called >> %q\nprintf 'token\\000'\n", marker),
					0o755,
				),
			)
			key := "core.fsmonitor"
			if refusal {
				key = "core.sshCommand"
			}
			orchestratorGit(t, clone, "config", key, script)
			before := orchestratorSessionCount(t, h)
			id, where, err := (orchestratorHost{h.mgr}).SpawnSession(
				h.ctx,
				caller,
				clone,
				"api",
				"work in the worktree",
			)
			path := projectpath.WorktreePath(worktrees, clone, "api")
			if refusal {
				require.ErrorContains(t, err, "repository config defines")
				assert.Equal(t, before, orchestratorSessionCount(t, h))
				assert.NoDirExists(t, path)
				assert.NoFileExists(t, marker)
				return
			}
			require.NoError(t, err)
			h.waitAnswer(id, "done")
			assert.DirExists(t, path)
			assert.NoFileExists(t, marker)
			assert.Equal(t, projectpath.RepoNamespace(clone)+"/api", where)
			record := h.session(id)
			assert.Equal(t, clone, record.Attributes["repo_root"])
			assert.Equal(
				t,
				controllerapi.WorktreeOriginController,
				record.Attributes[controllerapi.SessionAttributeWorktreeOrigin],
			)
			name, err := h.store.GetProjectName(h.ctx, record.ProjectID)
			require.NoError(t, err)
			assert.Equal(t, where, name)
			workDir, err := h.store.GetProjectWorkDir(h.ctx, record.ProjectID)
			require.NoError(t, err)
			assert.Equal(t, path, workDir)
		})
	}
}

func orchestratorClone(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, "clone")
	require.NoError(t, exec.Command("git", "init", "-q", "--bare", "-b", "trunk", remote).Run())
	require.NoError(t, exec.Command("git", "init", "-q", "-b", "trunk", seed).Run())
	orchestratorGit(t, seed, "config", "user.email", "t@t.t")
	orchestratorGit(t, seed, "config", "user.name", "t")
	orchestratorGit(t, seed, "config", "commit.gpgSign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "f.txt"), []byte("v1"), 0o644))
	orchestratorGit(t, seed, "add", "-A")
	orchestratorGit(t, seed, "commit", "-m", "init")
	orchestratorGit(t, seed, "remote", "add", "origin", remote)
	orchestratorGit(t, seed, "push", "-q", "origin", "trunk")
	orchestratorGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	require.NoError(t, exec.Command("git", "clone", "-q", remote, clone).Run())
	orchestratorGit(t, clone, "config", "user.email", "t@t.t")
	orchestratorGit(t, clone, "config", "user.name", "t")
	orchestratorGit(t, clone, "config", "commit.gpgSign", "false")
	return clone
}

func orchestratorGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}
