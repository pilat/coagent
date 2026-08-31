package managerdiscovery

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
)

func (s *service) CreateProject(
	ctx context.Context,
	managerID string,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
	if data.System {
		if managerID != controllerapi.BuiltinCLIManagerID {
			return nil, errors.New("the reserved system project belongs to the local chat")
		}

		return s.createSystemProject(ctx, data.Name)
	}

	name, err := projectpath.SanitizeName(data.Name)
	if err != nil {
		return nil, fmt.Errorf("sanitize project name: %w", err)
	}

	path := filepath.Join(projectpath.ResolveRoot(s.unifiedConfig()), name)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create project dir: %w", err)
	}

	projectID, err := s.backend.GetOrCreateProject(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("resolve project: %w", err)
	}

	return &controllerapi.ProjectCreateResultData{ID: projectID, Name: name, Path: path}, nil
}

func (s *service) createSystemProject(
	ctx context.Context,
	name string,
) (*controllerapi.ProjectCreateResultData, error) {
	if name != controllerapi.CoagentSystemProjectName {
		return nil, fmt.Errorf("unknown system project %q", name)
	}

	path := filepath.Join(projectpath.ResolveRoot(s.unifiedConfig()), controllerapi.CoagentSystemProjectDir)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, fmt.Errorf("create system project dir: %w", err)
	}

	projectID, err := s.backend.GetOrCreateSystemProject(ctx, path, name)
	if err != nil {
		return nil, fmt.Errorf("resolve system project: %w", err)
	}

	return &controllerapi.ProjectCreateResultData{ID: projectID, Name: name, Path: path}, nil
}

func (s *service) ListRecentProjects(ctx context.Context) (*controllerapi.ProjectListResultData, error) {
	recent, err := s.backend.ListRecentProjects(ctx, projectpath.ResolveRoot(s.unifiedConfig()))
	if err != nil {
		return nil, fmt.Errorf("list recent projects: %w", err)
	}

	return &controllerapi.ProjectListResultData{Projects: recent}, nil
}
