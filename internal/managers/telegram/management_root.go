package telegram

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
)

// ensureManagementRoot provisions or resumes this manager's one live
// management root, bound to the current service topic. The durable
// session-store ensure is the uniqueness boundary, not this in-memory ID.
func (m *Manager) ensureManagementRoot(ctx context.Context, topicID int64) (int64, error) {
	if topicID <= 0 {
		return 0, fmt.Errorf("invalid service topic %d", topicID)
	}

	rootID, err := m.controller.EnsureManagementRoot(ctx, controllerapi.ManagementRootEnsureData{
		TopicID: topicID,
	})
	if err != nil {
		return 0, fmt.Errorf("ensure management root: %w", err)
	}

	return rootID, nil
}

// isManagementSession reports whether a claimed session carries the
// management-surface role; its output always renders in the service topic.
func isManagementSession(attrs map[string]any) bool {
	raw, ok := attrs[controllerapi.SessionAttributeManagementSurface]
	if !ok || raw == nil {
		return false
	}

	switch v := raw.(type) {
	case bool:
		return v
	case string:
		return v != "" && v != "false" && v != "0"
	default:
		return true
	}
}

// filterOutManagementRoot drops the management root from session pickers so
// /kill can never offer or destroy it.
func filterOutManagementRoot(sessions []controllerapi.SessionInfo, rootID int64) []controllerapi.SessionInfo {
	if rootID <= 0 {
		return sessions
	}

	kept := make([]controllerapi.SessionInfo, 0, len(sessions))

	for _, s := range sessions {
		if s.ID != rootID {
			kept = append(kept, s)
		}
	}

	return kept
}
