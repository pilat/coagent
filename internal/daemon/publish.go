package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

// Publish suppresses child events; failed route reads reach observers without claiming an owner.
//
//nolint:contextcheck,nolintlint // contextcheck reads this comment as config, so nolintlint sees it as unused
func (s *managerRouteSet) Publish(sessionID int64, n sessionevent.Notification) {
	if err := n.Validate(); err != nil {
		logger.Named("daemon.publish").Error(
			"invalid_notification",
			zap.Int64("session_id", sessionID),
			zap.String("notification_type", string(n.Type)),
			zap.Error(err),
		)

		return
	}

	isChild, managerID, known := s.lookupPublishRoute(sessionID)
	if !known {
		// Background: NotifySession carries no ctx, and inheriting a caller's dead
		// one would fail the check and mis-drop.
		rec, err := s.sessionStore.GetSession(context.Background(), sessionID)
		if err != nil || rec == nil {
			logger.Named("daemon.publish").Warn(
				"child_check_failed",
				zap.Int64("session_id", sessionID),
				zap.Error(err),
			)
			s.pubsub.PublishOwned(sessionID, "", n)

			return
		}

		isChild = rec.ParentID != 0
		managerID, _ = rec.Attributes[controllerapi.SessionAttributeManagerID].(string)
		managerID = s.cachePublishRoute(sessionID, isChild, managerID)
	}

	if isChild {
		return
	}

	s.pubsub.PublishOwned(sessionID, managerID, n)
}

func (s *managerRouteSet) lookupPublishRoute(sessionID int64) (bool, string, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	isChild, known := s.childCache[sessionID]
	managerID := s.ownerCache[sessionID]

	return isChild, managerID, known
}

func (s *managerRouteSet) cachePublishRoute(sessionID int64, isChild bool, managerID string) string {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	if cachedOwner, known := s.ownerCache[sessionID]; known {
		managerID = cachedOwner
	}

	s.childCache[sessionID] = isChild
	s.ownerCache[sessionID] = managerID

	return managerID
}
