package controllerapi

import "github.com/pilat/coagent/internal/sessionevent"

// SessionNotification wraps a notification with the originating session ID.
// Used by global subscribers who need to know which session sent the notification.
type SessionNotification struct {
	SessionID    int64
	Notification sessionevent.Notification
}
