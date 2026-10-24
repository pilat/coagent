package telegram

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
)

func (m *Manager) refreshActiveProgress(ctx context.Context) error {
	progress, ok := m.controller.(controllerapi.ProgressController)
	if !ok {
		return nil
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for progress refresh: %w", err)
	}

	for _, session := range sessions {
		if !session.HasActiveLoop || session.ID == m.assistantRootID {
			continue
		}

		if err := progress.RefreshProgress(ctx, session.ID); err != nil {
			return fmt.Errorf("refresh session %d progress: %w", session.ID, err)
		}
	}

	return nil
}
