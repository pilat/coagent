package telegram

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

func (m *Manager) handleSessionCreated(
	ctx context.Context,
	sessionID int64,
	n sessionevent.Notification,
) {
	if !m.ownsSession(n.Attributes) {
		return
	}

	if topicID, ok := topicIDFromAttributes(n.Attributes); ok && m.verifyTopicExists(ctx, topicID) {
		m.registerTopic(sessionID, topicID)
		m.setWorkDir(sessionID, n.WorkDir)

		return
	}

	if _, err := m.createTopicForSession(ctx, sessionID, n.WorkDir, n.Name, n.Attributes); err != nil {
		logger.Ctx(ctx).Named("telegram").Error(
			"bind_session_topic",
			zap.Int64("session_id", sessionID),
			zap.Error(err),
		)
	}
}
