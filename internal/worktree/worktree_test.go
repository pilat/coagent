package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
)

func TestCreateWorktree_HappyPath(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	wt, err := Create(ctx, s, clone, "api", false)
	require.NoError(t, err)

	assert.DirExists(t, wt.Path)
	assert.Equal(t, "api", filepath.Base(wt.Path))
	assert.Equal(
		t,
		projectpath.RepoNamespace(clone)+"/api",
		wt.DisplayName,
		"project name must read as <repo>/<branch>",
	)
	assert.Equal(t, "v1", readTestFile(t, filepath.Join(wt.Path, "f.txt")))

	root := s.WorktreesRoot
	assert.Equal(t, root, filepath.Dir(filepath.Dir(wt.Path)),
		"worktree project must not be a direct child of any picker root")
}

func TestCreateWorktree_FromLinkedWorktreeUsesFreshRemoteDefault(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	linked := filepath.Join(t.TempDir(), "linked")
	runGit(t, clone, "worktree", "add", "-b", "side", linked, "origin/trunk")

	require.NoError(t, os.WriteFile(filepath.Join(clone, "f.txt"), []byte("remote-v2"), 0o644))
	runGit(t, clone, "commit", "-am", "remote advance")
	runGit(t, clone, "push", "origin", "trunk")
	require.NoError(t, os.WriteFile(filepath.Join(clone, "f.txt"), []byte("local-main-v3"), 0o644))
	runGit(t, clone, "commit", "-am", "local main advance")

	require.NoError(t, os.WriteFile(filepath.Join(linked, "f.txt"), []byte("linked-dirty"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(linked, "linked-only.txt"), []byte("junk"), 0o644))

	wt, err := Create(ctx, s, linked, "api", false)
	require.NoError(t, err)

	assert.Equal(t, "remote-v2", readTestFile(t, filepath.Join(wt.Path, "f.txt")))
	assert.NoFileExists(t, filepath.Join(wt.Path, "linked-only.txt"))
	assert.Equal(t,
		gitOutput(t, clone, "rev-parse", "refs/remotes/origin/trunk"),
		gitOutput(t, clone, "rev-parse", "api"),
		"the new branch must use the fetched remote default, not either worktree HEAD",
	)
}

func TestCreateWorktree_RefusesBadNames(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	for _, name := range []string{"", "bad name", "-x", "a/b", "a..b"} {
		_, err := Create(ctx, s, clone, name, false)
		assert.Error(t, err, "name %q must be refused", name)
	}
}

func TestCreateWorktree_LoserRefusalLeavesWinnerUntouched(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	winner, err := Create(ctx, s, clone, "api", false)
	require.NoError(t, err)

	_, err = Create(ctx, s, clone, "api", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already exists")

	assert.DirExists(t, winner.Path, "a losing /gwt must not roll back the winner's worktree")

	client := git.NewWorktreeClient()
	exists, err := client.BranchExists(ctx, clone, "api")
	require.NoError(t, err)
	assert.True(t, exists, "a losing /gwt must not delete the winner's branch")
}

func TestCreateWorktree_RollsBackWhenCheckoutHookFails(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	hooks := t.TempDir()
	hookPath := filepath.Join(hooks, "post-checkout")
	require.NoError(t, os.WriteFile(hookPath, []byte("#!/bin/sh\necho HOOK-BOOM 1>&2\nexit 3\n"), 0o755))
	runGit(t, clone, "config", "core.hooksPath", hooks)

	_, err := Create(ctx, s, clone, "hooked", false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "HOOK-BOOM", "hook output must surface to the operator")

	client := git.NewWorktreeClient()
	repoRoot, err := client.RepoRoot(ctx, clone)
	require.NoError(t, err)
	assert.NoDirExists(t, projectpath.WorktreePath(
		projectpath.ResolveWorktreesRoot(s), repoRoot, "hooked",
	), "materialized worktree must be rolled back")

	exists, err := client.BranchExists(ctx, clone, "hooked")
	require.NoError(t, err)
	assert.False(t, exists, "the created branch must be rolled back too")

	runGit(t, clone, "config", "--unset", "core.hooksPath")
	retry, err := Create(ctx, s, clone, "hooked", false)
	require.NoError(t, err, "the same name must be re-runnable after rollback")
	assert.DirExists(t, retry.Path)
}

func TestCreateWorktree_RemoveClosureRollsEverythingBack(t *testing.T) {
	clone := setupClone(t)
	s := serviceWithWorktreesRoot(t)
	ctx := context.Background()

	wt, err := Create(ctx, s, clone, "api", false)
	require.NoError(t, err)
	require.DirExists(t, wt.Path)

	require.NoError(t, wt.remove())
	assert.NoDirExists(t, wt.Path, "remove must undo the worktree")

	client := git.NewWorktreeClient()
	exists, err := client.BranchExists(ctx, clone, "api")
	require.NoError(t, err)
	assert.False(t, exists, "remove must delete the branch too")
}

func TestCreateWorktree_CapsDisplayNameForSessionLists(t *testing.T) {
	clone := setupNamedClone(t, strings.Repeat("r", 140))
	s := serviceWithWorktreesRoot(t)

	wt, err := Create(context.Background(), s, clone, "api", false)
	require.NoError(t, err)

	assert.LessOrEqual(t, utf8.RuneCountInString(wt.DisplayName), 128,
		"display name must stay compact in session listings")
	assert.True(t, strings.HasSuffix(wt.DisplayName, "/api"), "branch segment must survive capping")
	assert.Equal(t, "api", filepath.Base(wt.Path))
}

func TestCreateWorktree_DistinguishesSameBasenameRepositories(t *testing.T) {
	first := setupNamedClone(t, strings.Repeat("r", 140))
	second := setupNamedClone(t, strings.Repeat("r", 140))
	s := serviceWithWorktreesRoot(t)

	wt1, err := Create(context.Background(), s, first, "api", false)
	require.NoError(t, err)
	wt2, err := Create(context.Background(), s, second, "api", false)
	require.NoError(t, err)

	assert.NotEqual(t, wt1.DisplayName, wt2.DisplayName,
		"two same-basename repositories must not share a display name")
	assert.LessOrEqual(t, utf8.RuneCountInString(wt1.DisplayName), 128)
}

func TestCreateWorktree_RefusesColonRepositoryBeforeCreating(t *testing.T) {
	clone := setupNamedClone(t, "api:prod")
	s := serviceWithWorktreesRoot(t)

	_, err := Create(context.Background(), s, clone, "api", false)
	require.Error(t, err, "a ':' repository basename would fail registration after materialization")

	// The refusal must precede branch and worktree creation.
	client := git.NewWorktreeClient()
	exists, err := client.BranchExists(context.Background(), clone, "api")
	require.NoError(t, err)
	assert.False(t, exists)
}

func serviceWithWorktreesRoot(t *testing.T) *config.UnifiedConfig {
	t.Helper()

	return &config.UnifiedConfig{WorktreesRoot: t.TempDir(), ProjectsRoot: t.TempDir()}
}

func setupClone(t *testing.T) string {
	t.Helper()

	return setupNamedClone(t, "clone")
}

func setupNamedClone(t *testing.T, name string) string {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, name)

	require.NoError(t, exec.Command("git", "init", "-q", "--bare", "-b", "trunk", remote).Run())

	require.NoError(t, exec.Command("git", "init", "-q", "-b", "trunk", seed).Run())
	runGit(t, seed, "config", "user.email", "t@t.t")
	runGit(t, seed, "config", "user.name", "t")
	runGit(t, seed, "config", "commit.gpgSign", "false")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "f.txt"), []byte("v1"), 0o644))
	runGit(t, seed, "add", "-A")
	runGit(t, seed, "commit", "-m", "init")
	runGit(t, seed, "remote", "add", "origin", remote)
	runGit(t, seed, "push", "-q", "origin", "trunk")
	runGit(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")

	require.NoError(t, exec.Command("git", "clone", "-q", remote, clone).Run())
	runGit(t, clone, "config", "user.email", "t@t.t")
	runGit(t, clone, "config", "user.name", "t")
	runGit(t, clone, "config", "commit.gpgSign", "false")

	return clone
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)

	return strings.TrimSpace(string(out))
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(b)
}
