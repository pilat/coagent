package telegram

import (
	"github.com/pilat/coagent/api/coagent/v1"
)

func (m *Manager) ownsSession(ownerID string) bool {
	return ownerID != "" && ownerID == m.ID()
}

func (m *Manager) filterOwnedActiveSessions(
	sessions []*coagentv1.SessionDetails,
) []*coagentv1.SessionDetails {
	owned := make([]*coagentv1.SessionDetails, 0, len(sessions))
	for _, session := range filterActiveSessions(sessions) {
		if m.ownsSession(session.GetOwnerId()) {
			owned = append(owned, session)
		}
	}

	return owned
}
