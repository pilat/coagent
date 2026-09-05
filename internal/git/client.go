package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// gitTimeout bounds each network git op (clone/pull) so a slow TLS / DNS
// blackhole / auth-prompt can't wedge the caller (var: tests shrink it).
var gitTimeout = 2 * time.Minute

// gitWaitDelay bounds the drain after the kill: git spawns git-remote-https, which
// inherits the output pipe and outlives the parent SIGKILL, so CombinedOutput would
// block past gitTimeout without it. Wait force-closes the pipes after this grace.
var gitWaitDelay = 10 * time.Second

// worktreeAddTimeout bounds `git worktree add` including the repo's
// post-checkout hook: hooks legitimately install dependencies, but a wedged
// one must not pin the manager's poll loop.
var worktreeAddTimeout = 10 * time.Minute

type Client interface {
	// Clone returns an error if destPath already exists.
	Clone(ctx context.Context, repoURL, destPath string) error

	Pull(ctx context.Context, repoPath string) error

	IsCloned(ctx context.Context, repoPath string) bool

	GetRemoteURL(ctx context.Context, repoPath string) (string, error)

	// HealthCheck reports whether the local clone passes git fsck; a non-nil
	// error means the repository is corrupt and worth re-cloning from scratch.
	HealthCheck(ctx context.Context, repoPath string) error

	// RepositoryState collects a bounded, read-only snapshot of workDir's Git
	// state. A non-repository is (NotRepository, nil); a failed probe is
	// (Unavailable, err) where err never carries raw command output.
	RepositoryState(ctx context.Context, workDir string) (RepositoryState, error)
}

var _ Client = (*client)(nil)

type client struct{}

func New() Client {
	return &client{}
}

func (c *client) Clone(ctx context.Context, repoURL, destPath string) error {
	if _, err := os.Stat(destPath); err == nil {
		return fmt.Errorf("%w: %s", ErrDestinationExists, destPath)
	}

	parentDir := filepath.Dir(destPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("creating parent directory: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", strconv.Itoa(CloneDepth), repoURL, destPath)
	cmd.Env = nonInteractiveGitEnv()
	cmd.WaitDelay = gitWaitDelay

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// Pull pulls latest changes from remote
func (c *client) Pull(ctx context.Context, repoPath string) error {
	if !c.IsCloned(ctx, repoPath) {
		return fmt.Errorf("%w: %s", ErrNotARepo, repoPath)
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "pull")
	cmd.Dir = repoPath
	cmd.Env = nonInteractiveGitEnv()
	cmd.WaitDelay = gitWaitDelay

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git pull failed: %w (output: %s)", err, string(output))
	}

	return nil
}

func (c *client) IsCloned(ctx context.Context, repoPath string) bool {
	gitDir := filepath.Join(repoPath, ".git")
	info, err := os.Stat(gitDir)

	if err != nil || !info.IsDir() {
		return false
	}

	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--git-dir")
	cmd.Dir = repoPath

	if err := cmd.Run(); err != nil {
		return false
	}

	return true
}

// HealthCheck runs a full git fsck (no --connectivity-only: it must open every
// pack, which is what catches AppleDouble junk and truncated indexes). fsck
// exits non-zero on broken objects/indexes and zero on a sound clone, so the
// verdict is exit-code based — locale- and version-independent.
func (c *client) HealthCheck(ctx context.Context, repoPath string) error {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", "fsck", "--no-dangling")
	cmd.Dir = repoPath
	cmd.Env = nonInteractiveGitEnv()
	cmd.WaitDelay = gitWaitDelay

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fsck failed: %w (output: %s)", err, string(output))
	}

	return nil
}

func (c *client) GetRemoteURL(ctx context.Context, repoPath string) (string, error) {
	if !c.IsCloned(ctx, repoPath) {
		return "", fmt.Errorf("%w: %s", ErrNotARepo, repoPath)
	}

	cmd := exec.CommandContext(ctx, "git", "remote", "get-url", "origin")
	cmd.Dir = repoPath

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("getting remote URL: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

// nonInteractiveGitEnv disables every credential-prompt path so git fails fast
// instead of blocking on stdin/GUI. os.Environ carries no secrets (they never
// enter the process env), so this is safe to inherit. LC_ALL=C pins git's
// messages to English: log greps and error diagnosis rely on stable output.
func nonInteractiveGitEnv() []string {
	return append(os.Environ(),
		"LC_ALL=C",
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_ASKPASS=",
	)
}
