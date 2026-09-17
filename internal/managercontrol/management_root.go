package managercontrol

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/projectpath"
)

// ensureManagementRoot provisions the shared hidden project and returns the
// manager's one live management root bound to the current service topic.
func (s *service) ensureManagementRoot(
	ctx context.Context,
	managerID string,
	topicID int64,
) (int64, error) {
	if err := requireManagerIdentity(managerID); err != nil {
		return 0, err
	}

	if topicID <= 0 {
		return 0, fmt.Errorf("invalid service topic %d", topicID)
	}

	path := filepath.Join(
		projectpath.ResolveRoot(s.unifiedConfig()),
		controllerapi.CoagentManagementProjectDir,
	)
	if err := os.MkdirAll(path, 0o755); err != nil {
		return 0, fmt.Errorf("create management project dir: %w", err)
	}

	projectID, err := s.backend.GetOrCreateHiddenProject(ctx, path)
	if err != nil {
		return 0, fmt.Errorf("resolve management project: %w", err)
	}

	record, _, err := s.backend.EnsureManagementRoot(
		ctx, projectID, managerID, topicID, managementRootName(path), path,
	)
	if err != nil {
		return 0, fmt.Errorf("ensure management root: %w", err)
	}

	return record.ID, nil
}

func managementRootName(path string) string {
	return filepath.Base(path) + " management"
}
