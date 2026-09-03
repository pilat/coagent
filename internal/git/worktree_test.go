package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSymrefHead(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "standard main", in: "ref: refs/heads/main\tHEAD\ndeadbeef\tHEAD\n", want: "main"},
		{name: "non-standard trunk", in: "ref: refs/heads/trunk\tHEAD\n", want: "trunk"},
		{name: "slashed name", in: "ref: refs/heads/release/2.0\tHEAD\n", want: "release/2.0"},
		{name: "space separated", in: "ref: refs/heads/dev HEAD\n", want: "dev"},
		{name: "no symref", in: "deadbeef\tHEAD\n", want: ""},
		{name: "empty", in: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, parseSymrefHead(tt.in))
		})
	}
}

func TestWorktreeClient_DefaultBranch_ReadsRemoteHead(t *testing.T) {
	_, clone := setupRemoteAndClone(t)

	remote, branch, err := NewWorktreeClient().DefaultBranch(context.Background(), clone)
	require.NoError(t, err)
	assert.Equal(t, "origin", remote)
	assert.Equal(t, "trunk", branch, "default branch must come from the remote, not a hardcoded main")
}

func TestWorktreeClient_DefaultBranch_NoRemote(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir, "main")
	writeFile(t, dir, "f", "x")
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-m", "init")

	_, _, err := NewWorktreeClient().DefaultBranch(context.Background(), dir)
	require.Error(t, err, "remote-first /gwt must refuse a repo with no remote")
}

func TestWorktreeClient_DefaultBranch_RefusesHostileSymref(t *testing.T) {
	remote, clone := setupRemoteAndClone(t)

	// The remote advertises an option-like default branch; without validation it
	// flows straight into `git fetch <remote> <branch>` argv.
	gitRun(t, remote, "update-ref", "refs/heads/--upload-pack=/bin/false", "refs/heads/trunk")
	gitRun(t, remote, "symbolic-ref", "HEAD", "refs/heads/--upload-pack=/bin/false")

	_, _, err := NewWorktreeClient().DefaultBranch(context.Background(), clone)
	require.Error(t, err, "remote metadata is untrusted and must never reach argv as an option")
}

func TestWorktreeClient_RepoRoot_StableFromLinkedWorktree(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	root, err := client.RepoRoot(ctx, clone)
	require.NoError(t, err)
	assert.Equal(t, realpath(t, clone), realpath(t, root))

	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, clone, "worktree", "add", "-b", "side", wt, "origin/trunk")

	fromLinked, err := client.RepoRoot(ctx, wt)
	require.NoError(t, err)
	assert.Equal(t, realpath(t, root), realpath(t, fromLinked),
		"repo root resolved from a linked worktree must be the main worktree")
}

func TestWorktreeClient_RepoRoot_SeparateGitDir(t *testing.T) {
	client := NewWorktreeClient()
	ctx := context.Background()

	base := t.TempDir()
	seed := filepath.Join(base, "seed")
	gitInit(t, seed, "main")
	writeFile(t, seed, "f", "x")
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "init")

	work := filepath.Join(base, "work")
	gitdir := filepath.Join(base, "work-gitdir")
	require.NoError(t, exec.Command("git", "clone", "-q", "--separate-git-dir="+gitdir, seed, work).Run())

	root, err := client.RepoRoot(ctx, work)
	require.NoError(t, err)
	assert.Equal(t, realpath(t, work), realpath(t, root),
		"the parent of a separate git dir is not the worktree root")
}

func TestWorktreeClient_CreateBranch_BasesOnRefsRemotesNotLocalShadow(t *testing.T) {
	remote, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	advanceRemoteTrunk(t, remote, "v2")
	require.NoError(t, client.FetchBranch(ctx, clone, "origin", "trunk"))

	// A local branch named "origin/trunk" sits at the stale clone point; the
	// short name would resolve to it, the full tracking ref must win.
	gitRun(t, clone, "branch", "origin/trunk", "HEAD")

	require.NoError(t, client.CreateBranch(ctx, clone, "api", "refs/remotes/origin/trunk"))

	want := gitOut(t, clone, "rev-parse", "refs/remotes/origin/trunk")
	got := gitOut(t, clone, "rev-parse", "api")
	assert.Equal(t, want, got, "the branch must base on the fetched remote ref, not a local shadow")

	up := exec.Command("git", "-C", clone, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "api@{u}")
	require.Error(t, up.Run(), "created branch must have no upstream")
}

func TestWorktreeClient_CreateBranch_RefusesTakenNameAtomically(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	gitRun(t, clone, "branch", "taken", "refs/remotes/origin/trunk")

	err := client.CreateBranch(ctx, clone, "taken", "refs/remotes/origin/trunk")
	require.Error(t, err, "git branch must refuse a taken name without touching it")

	// The pre-existing branch must be exactly where the other actor left it.
	want := gitOut(t, clone, "rev-parse", "refs/remotes/origin/trunk")
	assert.Equal(t, want, gitOut(t, clone, "rev-parse", "taken"))
}

func TestWorktreeClient_AddWorktree_FreshRemoteNoUpstreamNoLeak(t *testing.T) {
	remote, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	// Drift: local checkout goes to a dirty feature branch while the remote's
	// trunk advances past what the clone last saw.
	gitRun(t, clone, "switch", "-c", "feature")
	writeFile(t, clone, "junk.txt", "untracked")
	writeFile(t, clone, "f.txt", "locally-dirty")
	advanceRemoteTrunk(t, remote, "v2")

	require.NoError(t, client.FetchBranch(ctx, clone, "origin", "trunk"))
	require.NoError(t, client.CreateBranch(ctx, clone, "api", "refs/remotes/origin/trunk"))

	wt := filepath.Join(t.TempDir(), "api")
	out, err := client.AddWorktree(ctx, clone, wt, "api")
	require.NoError(t, err, out)

	assert.Equal(t, "v2", readFile(t, filepath.Join(wt, "f.txt")), "worktree must reflect fresh remote state")
	assert.NoFileExists(t, filepath.Join(wt, "junk.txt"), "local untracked junk must not leak into the worktree")

	up := exec.Command("git", "-C", wt, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}")
	require.Error(t, up.Run(), "new branch must have no upstream (--no-track)")

	assert.FileExists(t, filepath.Join(clone, "junk.txt"), "the invoking worktree must be untouched")
}

func TestWorktreeClient_BranchExists(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	exists, err := client.BranchExists(ctx, clone, "nope")
	require.NoError(t, err)
	assert.False(t, exists)

	gitRun(t, clone, "branch", "taken", "origin/trunk")

	exists, err = client.BranchExists(ctx, clone, "taken")
	require.NoError(t, err)
	assert.True(t, exists)
}

func TestWorktreeClient_ValidateBranchName(t *testing.T) {
	client := NewWorktreeClient()
	ctx := context.Background()

	for _, ok := range []string{"api", "feature-1", "release/2.0"} {
		require.NoError(t, client.ValidateBranchName(ctx, ok), ok)
	}

	for _, bad := range []string{"bad name", "a..b", ".hidden", "tip.lock"} {
		require.Error(t, client.ValidateBranchName(ctx, bad), bad)
	}
}

func TestWorktreeClient_RemoveWorktree_Rollback(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	wt := filepath.Join(t.TempDir(), "gone")
	require.NoError(t, client.CreateBranch(ctx, clone, "gone", "refs/remotes/origin/trunk"))
	out, err := client.AddWorktree(ctx, clone, wt, "gone")
	require.NoError(t, err, out)
	require.DirExists(t, wt)

	require.NoError(t, client.RemoveWorktree(ctx, clone, wt, "gone"))
	assert.NoDirExists(t, wt)

	exists, err := client.BranchExists(ctx, clone, "gone")
	require.NoError(t, err)
	assert.False(t, exists, "rollback must delete the branch too")
}

func TestWorktreeClient_RemoveWorktree_SkipsMissingWorktree(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	require.NoError(t, client.CreateBranch(ctx, clone, "onlybranch", "refs/remotes/origin/trunk"))

	missing := filepath.Join(t.TempDir(), "never-created")
	require.NoError(t, client.RemoveWorktree(ctx, clone, missing, "onlybranch"))

	exists, err := client.BranchExists(ctx, clone, "onlybranch")
	require.NoError(t, err)
	assert.False(t, exists, "rollback must still delete the branch when no worktree materialized")
}

func TestWorktreeClient_RemoveWorktree_RollbackThroughSymlinkedPath(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	// git registers worktrees under canonical paths (/var → /private/var on
	// macOS); the guard must match the physical path, not the caller's spelling.
	base := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	require.NoError(t, os.Symlink(base, alias))

	wt := filepath.Join(alias, "gone")
	require.NoError(t, client.CreateBranch(ctx, clone, "gone", "refs/remotes/origin/trunk"))
	out, err := client.AddWorktree(ctx, clone, wt, "gone")
	require.NoError(t, err, out)
	require.DirExists(t, wt)

	require.NoError(t, client.RemoveWorktree(ctx, clone, wt, "gone"))
	assert.NoDirExists(t, wt)

	exists, err := client.BranchExists(ctx, clone, "gone")
	require.NoError(t, err)
	assert.False(t, exists, "rollback must delete the branch too")
}

func TestWorktreeClient_AddWorktree_FailingHookPropagates(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	hooks := t.TempDir()
	writeFile(t, hooks, "post-checkout", "#!/bin/sh\necho HOOK-BOOM 1>&2\nexit 3\n")
	hookPath := filepath.Join(hooks, "post-checkout")
	require.NoError(t, os.Chmod(hookPath, 0o755)) //nolint:gosec // hook needs exec bit
	gitRun(t, clone, "config", "core.hooksPath", hooks)

	wt := filepath.Join(t.TempDir(), "hooked")
	require.NoError(t, client.CreateBranch(ctx, clone, "hooked", "refs/remotes/origin/trunk"))
	out, err := client.AddWorktree(ctx, clone, wt, "hooked")
	require.Error(t, err, "a failing post-checkout hook must fail the add")
	assert.Contains(t, out, "HOOK-BOOM", "hook stderr must be captured for surfacing")
}

func TestWorktreeClient_RepoRoot_BareRepositoryNamedDotGit(t *testing.T) {
	client := NewWorktreeClient()

	bare := filepath.Join(t.TempDir(), ".git")
	require.NoError(t, os.MkdirAll(bare, 0o755))
	gitRun(t, bare, "init", "-q", "--bare", bare)

	root, err := client.RepoRoot(context.Background(), bare)
	require.NoError(t, err)
	assert.Equal(t, realpath(t, bare), realpath(t, root),
		"a bare repository whose dir is named .git is its own repo root")
}

func TestWorktreeClient_RepoRoot_SubmoduleRelativeCoreWorktree(t *testing.T) {
	client := NewWorktreeClient()

	host := filepath.Join(t.TempDir(), "host")
	gitInit(t, host, "main")
	gitRun(t, host, "commit", "-q", "--allow-empty", "-m", "init")
	gitRun(t, host, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "./", "sub")

	root, err := client.RepoRoot(context.Background(), filepath.Join(host, "sub"))
	require.NoError(t, err)
	assert.Equal(t, realpath(t, filepath.Join(host, "sub")), realpath(t, root),
		"a relative core.worktree must resolve against the git dir, not the cwd")
}

func TestWorktreeClient_DefaultBranch_RefusesOptionLikeRemote(t *testing.T) {
	_, clone := setupRemoteAndClone(t)

	cfg := filepath.Join(clone, ".git", "config")
	f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = fmt.Fprintf(f, "\n[remote \"--upload-pack=evil\"]\n\turl = %s\n", cfg)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	gitRun(t, clone, "remote", "remove", "origin")

	_, _, err = NewWorktreeClient().DefaultBranch(context.Background(), clone)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unusable",
		"a dash-leading remote name must be refused before reaching git argv")
}

func TestWorktreeClient_BranchExists_FatalExitIsNotAbsence(t *testing.T) {
	client := NewWorktreeClient()

	exists, err := client.BranchExists(context.Background(), t.TempDir(), "api")
	require.Error(t, err, "a fatal git exit (corrupt or missing repo) must surface, not read as 'branch absent'")
	assert.False(t, exists)
}

func TestWorktreeClient_RemoveWorktree_SparesForeignWorktreeAtSamePath(t *testing.T) {
	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	// A foreign actor materialized its own worktree at the path between our
	// existence check and AddWorktree (the rollback TOCTOU window).
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, clone, "worktree", "add", "-b", "foreign", wt, "origin/trunk")
	require.NoError(t, client.CreateBranch(ctx, clone, "ours", "refs/remotes/origin/trunk"))

	require.NoError(t, client.RemoveWorktree(ctx, clone, wt, "ours"))

	assert.DirExists(t, wt, "rollback must not delete another actor's worktree")
	exists, err := client.BranchExists(ctx, clone, "ours")
	require.NoError(t, err)
	assert.False(t, exists, "our own branch is still ours to delete")
}

func TestWorktreeClient_AddWorktree_BoundsWedgedHook(t *testing.T) {
	old := worktreeAddTimeout
	worktreeAddTimeout = 2 * time.Second
	t.Cleanup(func() { worktreeAddTimeout = old })

	_, clone := setupRemoteAndClone(t)
	client := NewWorktreeClient()
	ctx := context.Background()

	hooks := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\nsleep 30\n"), 0o755))
	gitRun(t, clone, "config", "core.hooksPath", hooks)

	require.NoError(t, client.CreateBranch(ctx, clone, "hooked", "refs/remotes/origin/trunk"))
	wt := filepath.Join(t.TempDir(), "hooked")

	start := time.Now()
	_, err := client.AddWorktree(ctx, clone, wt, "hooked")
	require.Error(t, err, "a wedged post-checkout hook must hit the worktree add deadline")
	assert.Less(t, time.Since(start), 30*time.Second, "the deadline, not the hook, must end the add")
}

// --- helpers ---

func setupRemoteAndClone(t *testing.T) (string, string) {
	t.Helper()

	base := t.TempDir()
	remote := filepath.Join(base, "remote.git")
	seed := filepath.Join(base, "seed")
	clone := filepath.Join(base, "clone")

	require.NoError(t, exec.Command("git", "init", "-q", "--bare", "-b", "trunk", remote).Run())

	gitInit(t, seed, "trunk")
	writeFile(t, seed, "f.txt", "v1")
	gitRun(t, seed, "add", "-A")
	gitRun(t, seed, "commit", "-m", "init")
	gitRun(t, seed, "remote", "add", "origin", remote)
	gitRun(t, seed, "push", "-q", "origin", "trunk")
	gitRun(t, remote, "symbolic-ref", "HEAD", "refs/heads/trunk")

	require.NoError(t, exec.Command("git", "clone", "-q", remote, clone).Run())
	gitRun(t, clone, "config", "user.email", "t@t.t")
	gitRun(t, clone, "config", "user.name", "t")

	return remote, clone
}

func advanceRemoteTrunk(t *testing.T, remote, content string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "advance")
	require.NoError(t, exec.Command("git", "clone", "-q", remote, work).Run())
	gitRun(t, work, "config", "user.email", "t@t.t")
	gitRun(t, work, "config", "user.name", "t")
	writeFile(t, work, "f.txt", content)
	gitRun(t, work, "commit", "-aqm", "advance")
	gitRun(t, work, "push", "-q", "origin", "trunk")
}

func gitInit(t *testing.T, dir, branch string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, exec.Command("git", "init", "-q", "-b", branch, dir).Run())
	gitRun(t, dir, "config", "user.email", "t@t.t")
	gitRun(t, dir, "config", "user.name", "t")
}

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)

	return strings.TrimSpace(string(out))
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return string(b)
}

func realpath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)

	return resolved
}
