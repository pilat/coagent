package telegram

import (
	"github.com/pilat/coagent/internal/controllerapi"
)

func managerIDFromAttributes(attrs map[string]any) (string, bool) {
	managerID, ok := attrs[controllerapi.SessionAttributeManagerID].(string)

	return managerID, ok && managerID != ""
}

func (m *Manager) ownsSession(attrs map[string]any) bool {
	managerID, ok := managerIDFromAttributes(attrs)

	return ok && managerID == m.ID()
}

func (m *Manager) filterOwnedActiveSessions(
	sessions []controllerapi.SessionInfo,
) []controllerapi.SessionInfo {
	owned := make([]controllerapi.SessionInfo, 0, len(sessions))
	for _, session := range filterActiveSessions(sessions) {
		if m.ownsSession(session.Attributes) {
			owned = append(owned, session)
		}
	}

	return owned
}
