package managercontrol

import (
	"context"
	"fmt"
)

// resolveSessionProject resolves the registered project a session runs in or forks a worktree from.
func (s *service) resolveSessionProject(ctx context.Context, workDir string) (int64, error) {
	projectID, err := s.store.GetOrCreateProject(ctx, workDir)
	if err != nil {
		return 0, fmt.Errorf("get project: resolve project: %w", err)
	}

	return projectID, nil
}
