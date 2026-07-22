package daemon

import (
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

const subscriberBufferSize = 64

var _ NotificationSource = (*pubSub)(nil)

// pubSub is a fan-out notification hub for session notifications.
// Per-session subscribers receive notifications for a specific session.
// Global subscribers receive notifications from all sessions (with session ID).
type pubSub struct {
	mu      sync.RWMutex
	subs    map[int64][]chan sessionevent.Notification // per-session subscribers
	globals []chan controllerapi.SessionNotification   // wildcard subscribers (carry session ID)
}

func newPubSub() *pubSub {
	return &pubSub{
		subs: make(map[int64][]chan sessionevent.Notification),
	}
}

// Subscribe creates a buffered channel that receives notifications for a specific session.
func (ps *pubSub) Subscribe(sessionID int64) <-chan sessionevent.Notification {
	ch := make(chan sessionevent.Notification, subscriberBufferSize)

	ps.mu.Lock()

	ps.subs[sessionID] = append(ps.subs[sessionID], ch)
	ps.mu.Unlock()

	return ch
}

// SubscribeAll creates a buffered channel that receives notifications from all sessions.
func (ps *pubSub) SubscribeAll() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification, subscriberBufferSize)

	ps.mu.Lock()

	ps.globals = append(ps.globals, ch)
	ps.mu.Unlock()

	return ch
}

// Unsubscribe removes a per-session subscriber. Does NOT close the channel.
func (ps *pubSub) Unsubscribe(sessionID int64, ch <-chan sessionevent.Notification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	subs := ps.subs[sessionID]
	for i, s := range subs {
		if s == ch {
			ps.subs[sessionID] = append(subs[:i], subs[i+1:]...)
			break
		}
	}

	if len(ps.subs[sessionID]) == 0 {
		delete(ps.subs, sessionID)
	}
}

// UnsubscribeAll removes a global subscriber. Does NOT close the channel.
func (ps *pubSub) UnsubscribeAll(ch <-chan controllerapi.SessionNotification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for i, s := range ps.globals {
		if s == ch {
			ps.globals = append(ps.globals[:i], ps.globals[i+1:]...)
			break
		}
	}
}

// Publish sends a notification to all per-session subscribers for the given session
// and to all global subscribers. Non-blocking: if a subscriber's channel is full, the
// notification is dropped with a warning log.
func (ps *pubSub) Publish(sessionID int64, n sessionevent.Notification) {
	log := logger.Named("daemon.pubsub")

	ps.mu.RLock()
	perSession := append([]chan sessionevent.Notification(nil), ps.subs[sessionID]...)
	globals := append([]chan controllerapi.SessionNotification(nil), ps.globals...)
	ps.mu.RUnlock()

	for _, ch := range perSession {
		select {
		case ch <- n:
		default:
			log.Warn(
				"pubsub_slow_subscriber",
				zap.Int64("session_id", sessionID),
				zap.String("type", string(n.Type)),
			)
		}
	}

	sn := controllerapi.SessionNotification{SessionID: sessionID, Notification: n}

	for _, ch := range globals {
		select {
		case ch <- sn:
		default:
			log.Warn(
				"pubsub_slow_global_subscriber",
				zap.Int64("session_id", sessionID),
				zap.String("type", string(n.Type)),
			)
		}
	}
}
