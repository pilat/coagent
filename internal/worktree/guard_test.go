package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
)

func TestCreateWorktree_NoExecutionHooks(t *testing.T) {
	for _, guarded := range []bool{false, true} {
		t.Run(fmt.Sprint(guarded), func(t *testing.T) {
			clone := setupClone(t)
			markers := t.TempDir()
			for _, hook := range []string{"post-checkout", "reference-transaction"} {
				script := fmt.Sprintf("#!/bin/sh\nprintf called >> %q\n", filepath.Join(markers, hook))
				require.NoError(t, os.WriteFile(filepath.Join(clone, ".git", "hooks", hook), []byte(script), 0o755))
			}
			created, err := Create(t.Context(), serviceWithWorktreesRoot(t), clone, "guarded", guarded)
			require.NoError(t, err)
			assert.Equal(t, "v1", readTestFile(t, filepath.Join(created.Path, "f.txt")))
			for _, hook := range []string{"post-checkout", "reference-transaction"} {
				if guarded {
					assert.NoFileExists(t, filepath.Join(markers, hook))
				} else {
					assert.FileExists(t, filepath.Join(markers, hook))
				}
			}
		})
	}
}

func TestCreateWorktree_ConfigPreflight(t *testing.T) {
	for _, key := range []string{
		"filter.x.smudge", "credential.helper", "credential.https://example.invalid.helper",
		"core.sshCommand", "core.gitProxy", "core.alternateRefsCommand", "core.askPass", "remote.origin.uploadpack", "include.path",
	} {
		t.Run(key, func(t *testing.T) {
			clone := setupClone(t)
			cfg := serviceWithWorktreesRoot(t)
			marker := filepath.Join(t.TempDir(), "executed")
			value := fmt.Sprintf("touch %q", marker)
			if key == "include.path" {
				value = filepath.Join(t.TempDir(), "included")
				require.NoError(t, os.WriteFile(value, []byte("[filter \"x\"]\nsmudge = cat\n"), 0o600))
			}
			runGit(t, clone, "config", key, value)
			_, err := Create(t.Context(), cfg, clone, "refused", true)
			require.ErrorContains(t, err, "repository config defines")
			require.ErrorContains(t, err, "create the worktree with /gwt")
			assert.NoFileExists(t, marker)
			assert.NoDirExists(t, projectpath.WorktreePath(cfg.WorktreesRoot, clone, "refused"))
			exists, err := git.NewWorktreeClient(true).BranchExists(t.Context(), clone, "refused")
			require.NoError(t, err)
			assert.False(t, exists)
		})
	}
}

func TestCreateWorktree_AllowsGlobalConfig(t *testing.T) {
	clone := setupClone(t)
	global := filepath.Join(t.TempDir(), "global-config")
	included := filepath.Join(t.TempDir(), "global-included")
	require.NoError(
		t,
		os.WriteFile(included, []byte("[filter \"x\"]\nsmudge = cat\n[credential]\nhelper = unused-helper\n"), 0o600),
	)
	require.NoError(
		t,
		os.WriteFile(global, fmt.Appendf(nil, "[include]\npath = %s\n", included), 0o600),
	)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	created, err := Create(t.Context(), serviceWithWorktreesRoot(t), clone, "global", true)
	require.NoError(t, err)
	assert.DirExists(t, created.Path)
}

func TestCreateWorktree_RefusesLocalIncludes(t *testing.T) {
	for _, scope := range []string{"local", "worktree"} {
		for _, key := range []string{"include.path", "includeIf.onbranch:guarded.path"} {
			t.Run(scope+"/"+key, func(t *testing.T) {
				clone := setupClone(t)
				if scope == "worktree" {
					runGit(t, clone, "config", "extensions.worktreeConfig", "true")
				}
				included := filepath.Join(t.TempDir(), "benign-config")
				require.NoError(t, os.WriteFile(included, []byte("[test]\nvalue = benign\n"), 0o600))
				runGit(t, clone, "config", "--"+scope, key, included)
				_, err := Create(t.Context(), serviceWithWorktreesRoot(t), clone, "guarded", true)
				require.ErrorContains(t, err, "repository config defines "+strings.ToLower(key))
				require.ErrorContains(t, err, "create the worktree with /gwt")
			})
		}
	}
}

func TestCreateWorktree_ConditionalIncludeCannotExecute(t *testing.T) {
	clone := setupClone(t)
	cfg := serviceWithWorktreesRoot(t)
	marker := filepath.Join(t.TempDir(), "executed")
	included := filepath.Join(clone, ".git", "guarded.inc")
	require.NoError(t, os.WriteFile(included, fmt.Appendf(nil,
		"[filter \"planted\"]\nsmudge = touch %s; cat\n", marker), 0o600))
	require.NoError(
		t,
		os.WriteFile(filepath.Join(clone, ".git", "info", "attributes"), []byte("* filter=planted\n"), 0o600),
	)
	runGit(t, clone, "config", "includeIf.onbranch:guarded.path", "guarded.inc")
	_, err := Create(t.Context(), cfg, clone, "guarded", true)
	require.ErrorContains(t, err, "repository config defines includeif.onbranch:guarded.path")
	require.ErrorContains(t, err, "create the worktree with /gwt")
	assert.NoFileExists(t, marker)
	assert.NoDirExists(t, projectpath.WorktreePath(cfg.WorktreesRoot, clone, "guarded"))
	exists, err := git.NewWorktreeClient(true).BranchExists(t.Context(), clone, "guarded")
	require.NoError(t, err)
	assert.False(t, exists)
}

func TestCreateWorktree_Abandon(t *testing.T) {
	clone := setupClone(t)
	created, err := Create(t.Context(), serviceWithWorktreesRoot(t), clone, "abandoned", true)
	require.NoError(t, err)
	attributes := map[string]any{}
	created.StampAttributes(attributes)
	assert.Equal(t, clone, attributes["repo_root"])
	original := errors.New("registration failed")
	require.ErrorIs(t, created.Abandon(original), original)
	assert.NoDirExists(t, created.Path)
	exists, err := git.NewWorktreeClient().BranchExists(t.Context(), clone, "abandoned")
	require.NoError(t, err)
	assert.False(t, exists)
	require.ErrorContains(t, created.Abandon(original), "delete branch")
	require.ErrorIs(t, (Result{}).Abandon(original), original)
}

func TestCreateWorktree_Fsmonitor(t *testing.T) {
	clone := setupClone(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor")
	command := filepath.Join(t.TempDir(), "monitor")
	require.NoError(
		t,
		os.WriteFile(command, fmt.Appendf(nil, "#!/bin/sh\nprintf called >> %q\nprintf 'token\\000'\n", marker), 0o755),
	)
	runGit(t, clone, "config", "core.fsmonitor", command)
	_, err := Create(t.Context(), serviceWithWorktreesRoot(t), clone, "monitor", false)
	require.NoError(t, err)
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		t.Log("fsmonitor does not execute on this git worktree path")
		return
	}
	require.NoError(t, os.Remove(marker))
	_, err = Create(t.Context(), serviceWithWorktreesRoot(t), clone, "monitor-guarded", true)
	require.NoError(t, err)
	assert.NoFileExists(t, marker)
}
