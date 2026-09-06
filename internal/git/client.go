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

	"github.com/pilat/coagent/internal/procexec"
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
}

var _ Client = (*client)(nil)

type client struct {
	runner procexec.Runner
}

func New() Client {
	return &client{}
}

// NewSandboxed creates a client whose Git subprocesses run through runner.
func NewSandboxed(runner procexec.Runner) Client {
	return &client{runner: runner}
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

	cmd, err := c.command(ctx, parentDir, nonInteractiveGitEnv(), "clone", "--depth", strconv.Itoa(CloneDepth), repoURL, destPath)
	if err != nil {
		return fmt.Errorf("construct git clone: %w", err)
	}
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

	cmd, err := c.command(ctx, repoPath, nonInteractiveGitEnv(), "pull")
	if err != nil {
		return fmt.Errorf("construct git pull: %w", err)
	}
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

	cmd, err := c.command(ctx, repoPath, nil, "rev-parse", "--git-dir")
	if err != nil {
		return false
	}

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

	cmd, err := c.command(ctx, repoPath, nonInteractiveGitEnv(), "fsck", "--no-dangling")
	if err != nil {
		return fmt.Errorf("construct git fsck: %w", err)
	}
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

	cmd, err := c.command(ctx, repoPath, nil, "remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("construct git remote lookup: %w", err)
	}

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("getting remote URL: %w", err)
	}

	return strings.TrimSpace(string(output)), nil
}

func (c *client) command(ctx context.Context, workDir string, env []string, args ...string) (*exec.Cmd, error) {
	if c.runner == nil {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = workDir
		cmd.Env = env
		return cmd, nil
	}

	cmd, err := c.runner.Command(ctx, procexec.Request{
		Path:    "git",
		Args:    args,
		WorkDir: workDir,
		Env:     env,
	})
	if err != nil {
		return nil, fmt.Errorf("sandbox git command: %w", err)
	}

	return cmd, nil
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
