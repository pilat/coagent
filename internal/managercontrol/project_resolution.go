package managercontrol

import (
	"context"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/sessionstore"
)

// resolveSessionProject resolves the registered project a session runs in or forks a worktree from.
func (s *service) resolveSessionProject(ctx context.Context, workDir string) (int64, error) {
	projectID, err := s.store.GetOrCreateProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("get project: resolve project: %w", err)
	}

	return projectID, nil
}

func (c *appController) requireProject(ctx context.Context, dir string) error {
	projects, err := c.operator.app.store.ListProjects(ctx)
	if err != nil {
		return fmt.Errorf("list app projects: %w", err)
	}

	if !slices.ContainsFunc(projects, func(project sessionstore.ProjectRow) bool {
		return !project.Hidden && project.WorkDir == dir
	}) {
		return fmt.Errorf("no project at %q", dir)
	}

	return nil
}
