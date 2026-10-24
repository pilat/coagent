package managercontrol

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
)

func (c *controller) EnsureAssistantRoot(ctx context.Context) (*controllerapi.AssistantRootEnsureData, error) {
	if err := requireManagerIdentity(c.managerID); err != nil {
		return nil, err
	}

	record, err := c.app.backend.EnsureAssistantRoot(ctx, c.managerID)
	if err != nil {
		return nil, fmt.Errorf("ensure assistant root: %w", err)
	}

	dir, err := c.app.store.GetProjectWorkDir(ctx, record.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("read assistant work directory: %w", err)
	}

	return &controllerapi.AssistantRootEnsureData{
		SessionID:  record.ID,
		WorkDir:    dir,
		Attributes: record.Attributes,
	}, nil
}

func (s *service) assistantTopicName(managerID string) string {
	if cfg := s.unifiedConfig(); cfg != nil {
		for _, manager := range cfg.Managers {
			if manager.ID == managerID {
				return manager.AssistantTopicName
			}
		}
	}

	return ""
}
