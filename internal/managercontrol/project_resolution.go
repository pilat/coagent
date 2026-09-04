package managercontrol

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
)

// resolveSessionProject resolves the project a session runs in. worktreeName,
// when set, is the /gwt display name ("<repo>/<branch>") registered against the
// worktree directory instead of deriving the project name from its basename.
func (s *service) resolveSessionProject(
	ctx context.Context,
	data controllerapi.SessionCreateData,
	worktreeName string,
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

	if worktreeName != "" {
		projectID, err := s.backend.GetOrCreateNamedProject(ctx, data.WorkDir, worktreeName)
		if err != nil {
			return 0, fmt.Errorf("get worktree project: %w", err)
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
