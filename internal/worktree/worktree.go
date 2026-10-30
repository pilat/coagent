package worktree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
)

// rollbackTimeout bounds the failure-path rollback: it runs on a context that
// may already be canceled, and a wedged git there must not block the caller.
const rollbackTimeout = 30 * time.Second

// maxDisplayNameRunes caps "<repo>/<branch>" so managers with a display limit
// (Telegram topics: 128) can always create the fork's surface.
const maxDisplayNameRunes = 128

// Result owns a newly created branch and worktree until its root session exists.
type Result struct {
	Path        string
	DisplayName string
	RepoRoot    string
	remove      func() error
}

// Create forks the freshly fetched remote default, leaving the invoking checkout untouched.
func Create(ctx context.Context, cfg *config.UnifiedConfig, workDir, name string, noExecution bool) (Result, error) {
	name, err := validateWorktreeName(name)
	if err != nil {
		return Result{}, err
	}

	client := git.NewWorktreeClient(noExecution)
	if err := client.ValidateBranchName(ctx, name); err != nil {
		return Result{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	repoRoot, err := sourceRepository(ctx, client, workDir, noExecution)
	if err != nil {
		return Result{}, err
	}

	remote, branch, err := client.DefaultBranch(ctx, repoRoot)
	if err != nil {
		return Result{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	if err := assertNameFree(ctx, client, repoRoot, name); err != nil {
		return Result{}, err
	}

	worktreesRoot := projectpath.ResolveWorktreesRoot(cfg)

	projectsRoot := projectpath.ResolveRoot(cfg)
	if err := projectpath.ValidateNoOverlap(projectsRoot, worktreesRoot); err != nil {
		return Result{}, fmt.Errorf("validating root layout: %w", err)
	}

	worktreePath := projectpath.WorktreePath(worktreesRoot, repoRoot, name)
	if pathExists(worktreePath) {
		return Result{}, fmt.Errorf("worktree path already exists: %s", worktreePath)
	}

	remove := func() error {
		// WithoutCancel: the add can fail on a canceled context, yet the
		// rollback must still run and needs its own bound to stay exit-able.
		rbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancel()

		return client.RemoveWorktree(rbCtx, repoRoot, worktreePath, name)
	}

	if err := client.FetchBranch(ctx, repoRoot, remote, branch); err != nil {
		return Result{}, err //nolint:wrapcheck // git client wraps with operation context.
	}

	// refs/remotes, not the short "<remote>/<branch>" form: a local branch or
	// tag of that name would shadow the freshly fetched remote state.
	startRef := "refs/remotes/" + remote + "/" + branch
	if err := client.CreateBranch(ctx, repoRoot, name, startRef); err != nil {
		return Result{}, err //nolint:wrapcheck // branch creation is atomic: refused, not partially created.
	}

	if err := os.MkdirAll(filepath.Dir(worktreePath), 0o755); err != nil {
		if removeErr := remove(); removeErr != nil {
			return Result{}, errors.Join(fmt.Errorf("create worktree parent: %w", err), removeErr)
		}

		return Result{}, fmt.Errorf("create worktree parent: %w", err)
	}

	output, err := client.AddWorktree(ctx, repoRoot, worktreePath, name)
	if err != nil {
		if removeErr := remove(); removeErr != nil {
			return Result{}, errors.Join(worktreeAddError(err, output), removeErr)
		}

		return Result{}, worktreeAddError(err, output)
	}

	return Result{
		Path: worktreePath,
		DisplayName: projectpath.RepoDisplayName(
			repoRoot,
			maxDisplayNameRunes-1-utf8.RuneCountInString(name),
		) + "/" + name,
		RepoRoot: repoRoot,
		remove:   remove,
	}, nil
}

// Abandon preserves the original failure and any failure to undo the newly created state.
func (r Result) Abandon(err error) error {
	if r.remove == nil {
		return err
	}

	return errors.Join(err, r.remove())
}

// StampAttributes attests the shared git directory for session confinement.
func (r Result) StampAttributes(attributes map[string]any) {
	if r.RepoRoot != "" {
		attributes["repo_root"] = r.RepoRoot
		attributes[controllerapi.SessionAttributeWorktreeOrigin] = controllerapi.WorktreeOriginController
	}
}

func sourceRepository(
	ctx context.Context,
	client git.WorktreeClient,
	workDir string,
	noExecution bool,
) (string, error) {
	repoRoot, err := client.RepoRoot(ctx, workDir)
	if err != nil {
		return "", err //nolint:wrapcheck // git client wraps with operation context.
	}

	if noExecution {
		if err := client.CheckRepositoryConfig(ctx, repoRoot); err != nil {
			return "", fmt.Errorf("check worktree source: %w", err)
		}
	}

	// ':' in a repository basename would fail display-name registration only
	// after the worktree materializes; refuse before anything is created.
	repoSegment := filepath.Base(repoRoot)
	if strings.ContainsRune(repoSegment, ':') {
		return "", fmt.Errorf("repository directory %q is reserved", repoSegment)
	}

	return repoRoot, nil
}

// Names reach both the project registry and git argv.
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

func assertNameFree(ctx context.Context, client git.WorktreeClient, repoRoot, name string) error {
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
