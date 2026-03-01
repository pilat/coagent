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

type Client interface {
	// Clone returns an error if destPath already exists.
	Clone(ctx context.Context, repoURL, destPath string) error

	Pull(ctx context.Context, repoPath string) error

	IsCloned(ctx context.Context, repoPath string) bool

	GetRemoteURL(ctx context.Context, repoPath string) (string, error)
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
// enter the process env), so this is safe to inherit.
func nonInteractiveGitEnv() []string {
	return append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GCM_INTERACTIVE=never",
		"GIT_ASKPASS=",
	)
}
