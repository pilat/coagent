package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
)

// rollbackTimeout bounds the failure-path rollback: it runs on a context that
// may already be canceled, and a wedged git there must not block the caller.
const rollbackTimeout = 30 * time.Second

// maxDisplayNameRunes caps "<repo>/<branch>" so managers with a display limit
// (Telegram topics: 128) can always create the fork's surface.
const maxDisplayNameRunes = 128

// createdWorktree is a materialized fork plus how to undo it. remove rolls back
// the worktree and branch and must be called at most once; callers invoke it
// when registration or launch fails after the fork exists.
type createdWorktree struct {
	path        string
	displayName string
	remove      func() error
}

// createWorktree forks workDir's repository into a new git worktree named name,
// returning the session directory and the project display name
// ("<repo>/<branch>"). The branch is cut fresh from the remote default branch,
// never from workDir's own (possibly dirty) state. Ownership makes failure
// atomic: the branch exists only after an atomic git branch creation, so
// rollback — and the remove handed to the caller for post-creation failures —
// can only ever delete state this call just made.
func (s *service) createWorktree(ctx context.Context, workDir, name string) (createdWorktree, error) {
	name, err := validateWorktreeName(name)
	if err != nil {
		return createdWorktree{}, err
	}

	client := git.NewWorktreeClient()
	if err := client.ValidateBranchName(ctx, name); err != nil {
		return createdWorktree{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	repoRoot, err := client.RepoRoot(ctx, workDir)
	if err != nil {
		return createdWorktree{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	// ':' in a repository basename would fail display-name registration only
	// after the worktree materializes; refuse before anything is created.
	repoSegment := filepath.Base(repoRoot)
	if strings.ContainsRune(repoSegment, ':') {
		return createdWorktree{}, fmt.Errorf("repository directory %q is reserved", repoSegment)
	}

	remote, branch, err := client.DefaultBranch(ctx, repoRoot)
	if err != nil {
		return createdWorktree{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	if err := s.assertNameFree(ctx, client, repoRoot, name); err != nil {
		return createdWorktree{}, err
	}

	worktreesRoot := projectpath.ResolveWorktreesRoot(s.unifiedConfig())

	projectsRoot := projectpath.ResolveRoot(s.unifiedConfig())
	if err := projectpath.ValidateNoOverlap(projectsRoot, worktreesRoot); err != nil {
		return createdWorktree{}, fmt.Errorf("validating root layout: %w", err)
	}

	worktreePath := projectpath.WorktreePath(worktreesRoot, repoRoot, name)
	if pathExists(worktreePath) {
		return createdWorktree{}, fmt.Errorf("worktree path already exists: %s", worktreePath)
	}

	remove := func() error {
		// WithoutCancel: the add can fail on a canceled context, yet the
		// rollback must still run and needs its own bound to stay exit-able.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		return client.RemoveWorktree(rbCtx, repoRoot, worktreePath, name)
	}

	if err := client.FetchBranch(ctx, repoRoot, remote, branch); err != nil {
		return createdWorktree{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	// refs/remotes, not the short "<remote>/<branch>" form: a local branch or
	// tag of that name would shadow the freshly fetched remote state.
	startRef := "refs/remotes/" + remote + "/" + branch
	if err := client.CreateBranch(ctx, repoRoot, name, startRef); err != nil {
		return createdWorktree{}, err //nolint:wrapcheck // branch creation is atomic: refused, not partially created.
	}

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		if removeErr := remove(); removeErr != nil {
			return createdWorktree{}, errors.Join(fmt.Errorf("create worktree parent: %w", err), removeErr)
		}

		return createdWorktree{}, fmt.Errorf("create worktree parent: %w", err)
	}

	output, err := client.AddWorktree(ctx, repoRoot, worktreePath, name)
	if err != nil {
		if removeErr := remove(); removeErr != nil {
			return createdWorktree{}, errors.Join(worktreeAddError(err, output), removeErr)
		}

		return createdWorktree{}, worktreeAddError(err, output)
	}

	return createdWorktree{
		path: worktreePath,
		displayName: projectpath.RepoDisplayName(
			repoRoot,
			maxDisplayNameRunes-1-utf8.RuneCountInString(name),
		) + "/" + name,
		remove: remove,
	}, nil
}

// validateWorktreeName applies the registry name rules before anything is
// created: sanitization, and a leading-dash guard because git would read such
// a name as an option despite exec's argv passing.
func validateWorktreeName(name string) (string, error) {
	name, err := projectpath.SanitizeName(name)
	if err != nil {
		return "", fmt.Errorf("worktree name: %w", err)
	}

	if strings.HasPrefix(name, "-") {
		return "", errors.New("worktree name must not start with '-'")
	}

	return name, nil
}

func (s *service) assertNameFree(ctx context.Context, client git.WorktreeClient, repoRoot, name string) error {
	exists, err := client.BranchExists(ctx, repoRoot, name)
	if err != nil {
		return err //nolint:wrapcheck // git client wraps with operation context.
	}

	if exists {
		return fmt.Errorf("branch %q already exists", name)
	}

	return nil
}

// worktreeAddError folds git's combined output into the error so a failing
// post-checkout hook surfaces its own message rather than a bare exit status.
func worktreeAddError(err error, output string) error {
	trimmed := strings.TrimSpace(output)
	if trimmed == "" {
		return err
	}

	return fmt.Errorf("%w\n%s", err, trimmed)
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)

	return err == nil
}
