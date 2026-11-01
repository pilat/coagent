package daemon

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/registry"
)

func TestHarnessScenario_AssistantCreatesWorktree(t *testing.T) {
	for _, remote := range []bool{true, false} {
		t.Run(map[bool]string{true: "remote", false: "no remote"}[remote], func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			repo := assistantWorktreeRepository(t)
			if !remote {
				assistantWorktreeGit(t, repo, "remote", "remove", "origin")
			}
			require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("dirty"), 0o600))
			worktrees := t.TempDir()
			h := newHarness(t, harnessOptions{
				configure: func(cfg *config.Config) {
					withKnownModels([]string{"fake-model"})(cfg)
					cfg.UnifiedConfig.WorktreesRoot = worktrees
				},
				respond: func(system string, _ []llmwire.Message) *llmwire.Response {
					if strings.HasPrefix(system, registry.AssistantPrompt) {
						return textReply("")
					}
					return textReply("worktree answer")
				},
			})
			root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
			require.NoError(t, err)
			_, err = h.store.GetOrCreateNamedProject(h.ctx, repo, "source")
			require.NoError(t, err)
			h.startInboxWake()
			card, err := assistantFor(h, root.ID).Open(h.ctx,
				"source", "inspect the fresh worktree", "worktree", "feat-x")
			if !remote {
				require.ErrorContains(t, err, "repository has no remote")
				var count int
				require.NoError(
					t,
					h.db.QueryRowContext(h.ctx, `SELECT count(*) FROM sessions WHERE agent_type <> 'assistant'`).
						Scan(&count),
				)
				assert.Zero(t, count)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, projectpath.RepoNamespace(repo)+"/feat-x", card.Project)
			record, err := h.store.GetSession(h.ctx, card.ID)
			require.NoError(t, err)
			path, err := h.store.GetProjectWorkDir(h.ctx, record.ProjectID)
			require.NoError(t, err)
			assert.Equal(t, projectpath.WorktreePath(worktrees, repo, "feat-x"), path)
			assert.Equal(t, repo, record.Attributes["repo_root"])
			assert.Equal(
				t,
				controllerapi.WorktreeOriginController,
				record.Attributes[controllerapi.SessionAttributeWorktreeOrigin],
			)
			data, err := os.ReadFile(filepath.Join(path, "f.txt"))
			require.NoError(t, err)
			assert.Equal(t, "remote version", string(data))
			waitAssistantObservation(t, h, root.ID, 1)
			rows := assistantChatMessages(t, h, root.ID)
			require.Len(t, rows, 1)
			assert.Equal(t, card.ID, rows[0].Origin)
			assert.Equal(t, "worktree answer", rows[0].Content)
			assertAssistantOwnOutputCount(t, h, root.ID, 0)
		})
	}
}

func assistantWorktreeRepository(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	remote, repo := filepath.Join(base, "remote.git"), filepath.Join(base, "repo")
	assistantWorktreeGit(t, base, "init", "--bare", "-b", "trunk", remote)
	assistantWorktreeGit(t, base, "clone", remote, repo)
	assistantWorktreeGit(t, repo, "config", "user.email", "test@example.invalid")
	assistantWorktreeGit(t, repo, "config", "user.name", "Test")
	assistantWorktreeGit(t, repo, "config", "commit.gpgSign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "f.txt"), []byte("remote version"), 0o600))
	assistantWorktreeGit(t, repo, "add", "f.txt")
	assistantWorktreeGit(t, repo, "commit", "-m", "initial")
	assistantWorktreeGit(t, repo, "push", "origin", "trunk")
	return repo
}

func assistantWorktreeGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	output, err := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "%s", output)
}
