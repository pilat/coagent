package daemon

import (
	"context"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

// publish is the one choke point that drops child-session events. It fails open:
// silencing a root session on a transient DB error is the worse outcome.
//
//nolint:contextcheck,nolintlint // contextcheck reads this comment as config, so nolintlint sees it as unused
func (s *svc) publish(sessionID int64, n sessionevent.Notification) {
	if err := n.Validate(); err != nil {
		logger.Named("daemon.publish").Error(
			"invalid_notification",
			zap.Int64("session_id", sessionID),
			zap.String("notification_type", string(n.Type)),
			zap.Error(err),
		)

		return
	}

	isChild, known := s.lookupChild(sessionID)
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
			s.pubsub.Publish(sessionID, n)

			return
		}

		isChild = rec.ParentID != 0
		s.cacheChild(sessionID, isChild)
	}

	if isChild {
		return
	}

	s.pubsub.Publish(sessionID, n)
}

// lookupChild reports the cached child verdict for a session and whether the
// cache held one at all.
func (s *svc) lookupChild(sessionID int64) (bool, bool) {
	s.childMu.Lock()
	defer s.childMu.Unlock()

	isChild, known := s.childCache[sessionID]

	return isChild, known
}

func (s *svc) cacheChild(sessionID int64, isChild bool) {
	s.childMu.Lock()
	defer s.childMu.Unlock()

	s.childCache[sessionID] = isChild
}
