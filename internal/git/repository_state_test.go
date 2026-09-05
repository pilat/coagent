package git

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	initGitRepo(t, dir)

	cmd := exec.Command("git", "symbolic-ref", "HEAD", "refs/heads/main")
	cmd.Dir = dir
	require.NoError(t, cmd.Run())

	return dir
}

func gitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	require.NoError(t, cmd.Run())
}

// gitCmdAllowFail runs git and swallows a non-zero exit; use only where the
// non-zero outcome is the expected one (e.g. a conflicted merge).
func gitCmdAllowFail(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test",
		"GIT_COMMITTER_EMAIL=test@test.com",
	)
	_, _ = cmd.CombinedOutput()
}

func stageFile(t *testing.T, dir, name, content string) {
	t.Helper()
	createFile(t, dir, name, content)
	gitCmd(t, dir, "add", name)
}

func TestRepositoryState_CleanRepository(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Equal(t, "main", state.Branch)
	assert.Regexp(t, `^[0-9a-f]{12,64}$`, state.Hash)
	assert.Zero(t, state.Staged)
	assert.Zero(t, state.Unstaged)
	assert.Zero(t, state.Untracked)
	assert.Zero(t, state.Conflicted)
}

func TestRepositoryState_WorkingTreeClassifications(t *testing.T) {
	t.Run("staged only", func(t *testing.T) {
		dir := newTestRepo(t)
		createFile(t, dir, "base.txt", "base")
		commitAll(t, dir, "initial")
		stageFile(t, dir, "staged.txt", "staged")

		state, err := New().RepositoryState(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, RepositoryAvailable, state.Status)
		assert.Equal(t, 1, state.Staged)
		assert.Zero(t, state.Unstaged)
		assert.Zero(t, state.Untracked)
		assert.Zero(t, state.Conflicted)
	})

	t.Run("unstaged only", func(t *testing.T) {
		dir := newTestRepo(t)
		createFile(t, dir, "file.txt", "original")
		commitAll(t, dir, "initial")
		createFile(t, dir, "file.txt", "modified")

		state, err := New().RepositoryState(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, RepositoryAvailable, state.Status)
		assert.Zero(t, state.Staged)
		assert.Equal(t, 1, state.Unstaged)
		assert.Zero(t, state.Untracked)
		assert.Zero(t, state.Conflicted)
	})

	t.Run("untracked only", func(t *testing.T) {
		dir := newTestRepo(t)
		createFile(t, dir, "base.txt", "base")
		commitAll(t, dir, "initial")
		createFile(t, dir, "untracked.txt", "new")

		state, err := New().RepositoryState(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, RepositoryAvailable, state.Status)
		assert.Zero(t, state.Staged)
		assert.Zero(t, state.Unstaged)
		assert.Equal(t, 1, state.Untracked)
		assert.Zero(t, state.Conflicted)
	})

	t.Run("mixed", func(t *testing.T) {
		dir := newTestRepo(t)
		createFile(t, dir, "committed.txt", "committed")
		commitAll(t, dir, "initial")
		createFile(t, dir, "committed.txt", "modified")
		stageFile(t, dir, "staged.txt", "staged")
		createFile(t, dir, "untracked.txt", "new")

		state, err := New().RepositoryState(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, RepositoryAvailable, state.Status)
		assert.Equal(t, 1, state.Staged)
		assert.Equal(t, 1, state.Unstaged)
		assert.Equal(t, 1, state.Untracked)
		assert.Zero(t, state.Conflicted)
	})

	t.Run("conflicted merge", func(t *testing.T) {
		dir := newTestRepo(t)
		createFile(t, dir, "shared.txt", "base")
		commitAll(t, dir, "initial")
		gitCmd(t, dir, "checkout", "-b", "feature")
		createFile(t, dir, "shared.txt", "feature change")
		commitAll(t, dir, "feature edit")
		gitCmd(t, dir, "checkout", "main")
		createFile(t, dir, "shared.txt", "main change")
		commitAll(t, dir, "main edit")
		// A conflicted merge exits non-zero by design; the merge must stop
		// with UU entries in the index.
		gitCmdAllowFail(t, dir, "merge", "feature", "--no-edit")

		state, err := New().RepositoryState(context.Background(), dir)
		require.NoError(t, err)
		assert.Equal(t, RepositoryAvailable, state.Status)
		assert.Equal(t, 1, state.Conflicted)
	})
}

func TestRepositoryState_DetachedHead(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")
	gitCmd(t, dir, "checkout", "--detach")

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Equal(t, DetachedHeadMarker, state.Branch)
	assert.Regexp(t, `^[0-9a-f]{12,64}$`, state.Hash)
}

func TestRepositoryState_NotARepository(t *testing.T) {
	dir := t.TempDir()

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryNotRepository, state.Status)
}

func TestRepositoryState_NoCommitsYet(t *testing.T) {
	dir := newTestRepo(t)

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Equal(t, "main", state.Branch)
	assert.Empty(t, state.Hash)
	assert.Zero(t, state.Staged)
}

func TestRepositoryState_BranchCappedAtMaxRunes(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")

	longBranch := strings.Repeat("b", 160)
	gitCmd(t, dir, "checkout", "-b", longBranch)

	state, err := New().RepositoryState(context.Background(), dir)
	require.NoError(t, err)

	assert.Equal(t, RepositoryAvailable, state.Status)
	assert.Len(t, []rune(state.Branch), 128)
}

func TestRepositoryState_LinkedWorktreeReportsItsOwnState(t *testing.T) {
	main := newTestRepo(t)
	createFile(t, main, "README.md", "# Test")
	commitAll(t, main, "initial")

	worktree := filepath.Join(t.TempDir(), "linked-wt")
	gitCmd(t, main, "worktree", "add", worktree, "-b", "worktree-branch")

	// Dirty the main work tree only: the linked worktree must stay clean.
	createFile(t, main, "untracked.txt", "main side")

	mainState, err := New().RepositoryState(context.Background(), main)
	require.NoError(t, err)
	assert.Equal(t, "main", mainState.Branch)
	assert.Equal(t, 1, mainState.Untracked)

	wtState, err := New().RepositoryState(context.Background(), worktree)
	require.NoError(t, err)
	assert.Equal(t, RepositoryAvailable, wtState.Status)
	assert.Equal(t, "worktree-branch", wtState.Branch)
	assert.Zero(t, wtState.Untracked, "linked worktree must not report main work tree state")
}

func TestRepositoryState_CancelledContext(t *testing.T) {
	dir := newTestRepo(t)
	createFile(t, dir, "README.md", "# Test")
	commitAll(t, dir, "initial")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	state, err := New().RepositoryState(ctx, dir)
	require.Error(t, err)
	assert.Equal(t, RepositoryUnavailable, state.Status)
	assert.NotContains(t, err.Error(), "fatal:", "raw git output must not reach callers")
}

func TestRepositoryState_MissingGitBinary(t *testing.T) {
	dir := newTestRepo(t)
	t.Setenv("PATH", "")

	state, err := New().RepositoryState(context.Background(), dir)
	require.Error(t, err)
	assert.Equal(t, RepositoryUnavailable, state.Status)
	assert.NotContains(t, err.Error(), "fatal:", "raw git output must not reach callers")
}
