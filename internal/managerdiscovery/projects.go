package managerdiscovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
)

func (s *Service) CreateProject(
	ctx context.Context,
	_ string,
	data controllerapi.ProjectCreateData,
) (*controllerapi.ProjectCreateResultData, error) {
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

func (s *Service) ListRecentProjects(ctx context.Context) (*controllerapi.ProjectListResultData, error) {
	recent, err := listRecentProjects(ctx, s.backend, projectpath.ResolveRoot(s.unifiedConfig()))
	if err != nil {
		return nil, fmt.Errorf("list recent projects: %w", err)
	}

	return &controllerapi.ProjectListResultData{Projects: recent}, nil
}
