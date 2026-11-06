package git

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/procexec"
)

// WorktreeClient runs host git with optional repository-execution restrictions.
type WorktreeClient struct{ noExecution bool }

func NewWorktreeClient(noExecution ...bool) *WorktreeClient {
	return &WorktreeClient{noExecution: len(noExecution) > 0 && noExecution[0]}
}

func (c *WorktreeClient) RepoRoot(ctx context.Context, path string) (string, error) {
	out, err := c.runGit(ctx, path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolving git repository for %s: %w", path, err)
	}

	commonDir := filepath.Clean(strings.TrimSpace(out))
	if commonDir == "" {
		return "", fmt.Errorf("empty git common dir for %s", path)
	}

	// A bare repo's git dir is the repo root itself; is-bare also catches
	// bare repositories whose directory is literally named ".git".
	if bare, err := c.runGit(ctx, path, "rev-parse", "--is-bare-repository"); err == nil &&
		strings.TrimSpace(bare) == "true" {
		return commonDir, nil
	}

	// Standard layouts (plain main tree or linked worktree) keep the common
	// git dir inside the main worktree as ".git".
	if filepath.Base(commonDir) != ".git" {
		// A detached git dir (--separate-git-dir) names no tree: prefer the
		// one recorded in core.worktree, then the invoking path's toplevel.
		if tree, err := c.runGit(ctx, path, "config", "--get", "core.worktree"); err == nil &&
			strings.TrimSpace(tree) != "" {
			// git resolves a relative core.worktree against the git dir.
			tree = strings.TrimSpace(tree)
			if !filepath.IsAbs(tree) {
				tree = filepath.Join(commonDir, tree)
			}

			return filepath.Clean(tree), nil
		}

		if tree, err := c.runGit(ctx, path, "rev-parse", "--path-format=absolute", "--show-toplevel"); err == nil {
			return filepath.Clean(strings.TrimSpace(tree)), nil
		}

		return commonDir, nil
	}

	return filepath.Dir(commonDir), nil
}

func (c *WorktreeClient) DefaultBranch(ctx context.Context, repoRoot string) (string, string, error) {
	remote, err := c.resolveRemote(ctx, repoRoot)
	if err != nil {
		return "", "", err
	}

	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	out, err := c.runGit(ctx, repoRoot, "ls-remote", "--symref", remote, "HEAD")
	if err != nil {
		return "", "", fmt.Errorf("reading default branch from remote %q: %w", remote, err)
	}

	branch := parseSymrefHead(out)
	if branch == "" {
		return "", "", fmt.Errorf("remote %q reports no default branch", remote)
	}

	// The symref is remote-controlled; without these checks a branch like
	// "--upload-pack=…" would reach fetch argv as an option.
	if strings.HasPrefix(branch, "-") || c.ValidateBranchName(ctx, branch) != nil {
		return "", "", fmt.Errorf("remote %q advertises an unusable default branch %q", remote, branch)
	}

	return remote, branch, nil
}

func (c *WorktreeClient) FetchBranch(ctx context.Context, repoRoot, remote, branch string) error {
	ctx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/%s/%s", branch, remote, branch)
	if _, err := c.runGit(ctx, repoRoot, "fetch", remote, refspec); err != nil {
		return fmt.Errorf("fetching %s/%s: %w", remote, branch, err)
	}

	return nil
}

func (c *WorktreeClient) BranchExists(ctx context.Context, repoRoot, branch string) (bool, error) {
	cmd := exec.CommandContext(
		ctx,
		"git",
		c.gitArgs(repoRoot, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)...)
	if err := procexec.Unprivileged(cmd).Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 1 {
			return false, nil // --quiet verify reports a missing ref with exit 1
		}

		return false, fmt.Errorf("checking branch %q: %w", branch, err)
	}

	return true, nil
}

func (c *WorktreeClient) ValidateBranchName(ctx context.Context, name string) error {
	if _, err := c.runGit(ctx, "", "check-ref-format", "refs/heads/"+name); err != nil {
		return fmt.Errorf("%q is not a valid branch name", name)
	}

	return nil
}

func (c *WorktreeClient) CreateBranch(ctx context.Context, repoRoot, branch, startRef string) error {
	if _, err := c.runGit(ctx, repoRoot, "branch", "--no-track", branch, startRef); err != nil {
		return fmt.Errorf("creating branch %q: %w", branch, err)
	}

	return nil
}

func (c *WorktreeClient) AddWorktree(
	ctx context.Context,
	repoRoot, worktreePath, branch string,
) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, worktreeAddTimeout)
	defer cancel()

	cmd := exec.CommandContext(
		ctx, "git", c.gitArgs(repoRoot, "worktree", "add", worktreePath, branch)...,
	)
	cmd.Env = nonInteractiveGitEnv()
	cmd.WaitDelay = gitWaitDelay

	output, err := procexec.Unprivileged(cmd).CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("git worktree add failed: %w", err)
	}

	return string(output), nil
}

func (c *WorktreeClient) RemoveWorktree(ctx context.Context, repoRoot, worktreePath, branch string) error {
	var errs []error

	if c.worktreeAtBranch(ctx, repoRoot, worktreePath, branch) {
		if _, err := c.runGit(ctx, repoRoot, "worktree", "remove", "--force", worktreePath); err != nil {
			errs = append(errs, fmt.Errorf("remove worktree: %w", err))
		}
	}

	if _, err := c.runGit(ctx, repoRoot, "branch", "-D", branch); err != nil {
		errs = append(errs, fmt.Errorf("delete branch: %w", err))
	}

	return errors.Join(errs...)
}

func (c *WorktreeClient) CheckRepositoryConfig(ctx context.Context, repoRoot string) error {
	const keys = `^(include\.path|includeif\..*\.path|filter\..*|credential(\..*)?\.helper|core\.(sshcommand|gitproxy|alternaterefscommand|askpass)|remote\..*\.uploadpack)$`

	// An inactive conditional include can become active during the new worktree's checkout.
	output, err := c.runGit(
		ctx,
		repoRoot,
		"config",
		"--no-includes",
		"--show-scope",
		"--name-only",
		"--get-regexp",
		keys,
	)
	if isExitCode(err, 1) && output == "" {
		return nil
	}

	if err != nil {
		return fmt.Errorf("check repository config: %w", err)
	}

	for line := range strings.SplitSeq(output, "\n") {
		scope, key, _ := strings.Cut(line, "\t")
		if scope == "local" || scope == "worktree" {
			return fmt.Errorf("repository config defines %s; create the worktree with /gwt", strings.TrimSpace(key))
		}
	}

	return nil
}

// worktreeAtBranch reports whether repoRoot has a registered worktree at
// worktreePath checked out at branch. Rollback may run long after our
// existence check, so the path can have been claimed by a foreign worktree.
func (c *WorktreeClient) worktreeAtBranch(ctx context.Context, repoRoot, worktreePath, branch string) bool {
	out, err := c.runGit(ctx, repoRoot, "worktree", "list", "--porcelain")
	if err != nil {
		return false
	}

	// git reports canonical paths; a symlinked spelling of the same directory
	// must still match the guard.
	want := canonicalizePath(worktreePath)

	var atPath bool

	for line := range strings.Lines(out) {
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "worktree "):
			atPath = canonicalizePath(strings.TrimPrefix(line, "worktree ")) == want
		case atPath && line == "branch refs/heads/"+branch:
			return true
		case line == "":
			atPath = false
		}
	}

	return false
}

// canonicalizePath resolves the deepest existing prefix of p so a symlinked
// ancestor compares equal to git's canonical spelling.
func canonicalizePath(p string) string {
	p = filepath.Clean(p)
	suffix := ""

	for {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(resolved, suffix)
		}

		parent := filepath.Dir(p)
		if parent == p {
			return filepath.Join(p, suffix)
		}

		suffix = filepath.Join(filepath.Base(p), suffix)
		p = parent
	}
}

// resolveRemote picks the remote to base a worktree on: origin when present,
// otherwise the sole remote. Ambiguity and absence are both errors — /gwt is
// remote-first and refuses to guess.
func (c *WorktreeClient) resolveRemote(ctx context.Context, repoRoot string) (string, error) {
	out, err := c.runGit(ctx, repoRoot, "remote")
	if err != nil {
		return "", fmt.Errorf("listing remotes: %w", err)
	}

	remotes := strings.Fields(out)
	switch {
	case len(remotes) == 0:
		return "", errors.New("repository has no remote to base a worktree on")
	case len(remotes) == 1:
		// A dash-leading name would be parsed as an option by ls-remote/fetch.
		if strings.HasPrefix(remotes[0], "-") {
			return "", fmt.Errorf("remote %q is unusable: the name would reach git argv as an option", remotes[0])
		}

		return remotes[0], nil
	case slices.Contains(remotes, "origin"):
		return "origin", nil
	}

	return "", fmt.Errorf("repository has multiple remotes and none named origin: %s", strings.Join(remotes, ", "))
}

func (c *WorktreeClient) runGit(ctx context.Context, dir string, args ...string) (string, error) {
	full := c.gitArgs(dir, args...)

	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Env = nonInteractiveGitEnv()
	cmd.WaitDelay = gitWaitDelay

	output, err := procexec.Unprivileged(cmd).Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return string(output), fmt.Errorf("%w: %s", err, strings.TrimSpace(string(exit.Stderr)))
		}

		return string(output), err
	}

	return string(output), nil
}

// parseSymrefHead extracts the branch name from `git ls-remote --symref <r> HEAD`
// output, whose first line is `ref: refs/heads/<branch>\tHEAD`.
func parseSymrefHead(out string) string {
	for line := range strings.Lines(out) {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ref: refs/heads/")
		if !ok {
			continue
		}

		if tab := strings.IndexAny(rest, "\t "); tab >= 0 {
			return rest[:tab]
		}

		return rest
	}

	return ""
}

func (c *WorktreeClient) gitArgs(dir string, args ...string) []string {
	var full []string
	if c.noExecution {
		full = append(full, "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
			"-c", "protocol.ext.allow=never", "-c", "submodule.recurse=false")
	}

	if dir != "" {
		full = append(full, "-C", dir)
	}

	return append(full, args...)
}
