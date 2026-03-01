package git

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type WorktreeClient interface {
	FindRoot(ctx context.Context, path string) (string, error)
	CreateWorktree(ctx context.Context, repoRoot, worktreePath, branchName string) error
}

var _ WorktreeClient = (*worktreeClient)(nil)

type worktreeClient struct{}

func NewWorktreeClient() WorktreeClient {
	return &worktreeClient{}
}

func (c *worktreeClient) FindRoot(ctx context.Context, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")

	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("finding git root for %s: %w", path, err)
	}

	return strings.TrimSpace(string(output)), nil
}

func (c *worktreeClient) CreateWorktree(ctx context.Context, repoRoot, worktreePath, branchName string) error {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", "-b", branchName, worktreePath)

	output, err := cmd.CombinedOutput()
	if err != nil {
		// Branch may already exist — retry without -b
		if strings.Contains(string(output), "already exists") {
			cmd2 := exec.CommandContext(ctx, "git", "-C", repoRoot, "worktree", "add", worktreePath, branchName)
			output2, err2 := cmd2.CombinedOutput()

			if err2 != nil {
				return fmt.Errorf("git worktree add failed: %w (output: %s)", err2, string(output2))
			}

			return nil
		}

		return fmt.Errorf("git worktree add failed: %w (output: %s)", err, string(output))
	}

	return nil
}

// ComputeWorktreePaths computes the worktree path, full work dir, and branch name
// from the original path selected by the user and the git root.
//
//nolint:nonamedreturns // three same-typed string results are ambiguous at call sites without names
func ComputeWorktreePaths(originalPath, gitRoot string, now time.Time) (worktreePath, fullWorkDir, branchName string) {
	branchName = fmt.Sprintf("%s-%s", filepath.Base(gitRoot), now.Format(WorktreeBranchDateFormat))
	worktreePath = filepath.Join(filepath.Dir(gitRoot), branchName)

	tail := strings.TrimPrefix(originalPath, gitRoot)
	tail = strings.TrimPrefix(tail, "/")

	if tail == "" {
		fullWorkDir = worktreePath
	} else {
		fullWorkDir = filepath.Join(worktreePath, tail)
	}

	return worktreePath, fullWorkDir, branchName
}
