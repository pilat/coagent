package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/git"
	"github.com/pilat/coagent/internal/projectpath"
)

func (s *service) resolveSessionProject(
	ctx context.Context,
	data controllerapi.SessionCreateData,
) (int64, error) {
	if data.SystemProject != "" {
		expected := filepath.Join(
			projectpath.ResolveRoot(s.unifiedConfig()), controllerapi.CoagentSystemProjectDir,
		)
		if !projectpath.Same(data.WorkDir, expected) {
			return 0, errors.New("system project is outside the canonical configuration directory")
		}

		projectID, err := s.backend.GetOrCreateSystemProject(ctx, data.WorkDir, data.SystemProject)
		if err != nil {
			return 0, fmt.Errorf("get system project: %w", err)
		}

		return projectID, nil
	}

	if filepath.Base(filepath.Clean(data.WorkDir)) == controllerapi.CoagentSystemProjectDir {
		return 0, errors.New("reserved system project requires its internal identity")
	}

	projectID, err := s.backend.GetOrCreateProject(ctx, data.WorkDir)
	if err != nil {
		return 0, fmt.Errorf("get project: %w", err)
	}

	return projectID, nil
}

func createWorktree(ctx context.Context, workDir string) (string, error) {
	client := git.NewWorktreeClient()

	gitRoot, err := client.FindRoot(ctx, workDir)
	if err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	worktreePath, fullWorkDir, branchName := git.ComputeWorktreePaths(workDir, gitRoot, time.Now())
	if err := client.CreateWorktree(ctx, gitRoot, worktreePath, branchName); err != nil {
		return "", fmt.Errorf("worktree: %w", err)
	}

	return fullWorkDir, nil
}
