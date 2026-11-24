package sessionbus

import (
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

const subscriberBufferSize = 64

// Bus publishes session notifications to session, manager and global subscribers.
type Bus struct {
	mu      sync.RWMutex
	subs    map[int64][]chan sessionevent.Notification // per-session subscribers
	globals []chan controllerapi.SessionNotification   // wildcard subscribers (carry session ID)
	roots   []chan controllerapi.SessionNotification
}

func New() *Bus {
	return &Bus{
		subs: make(map[int64][]chan sessionevent.Notification),
	}
}

// SubscribeRoots receives every root's events.
func (ps *Bus) SubscribeRoots() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification, subscriberBufferSize)

	ps.mu.Lock()
	ps.roots = append(ps.roots, ch)
	ps.mu.Unlock()

	return ch
}

// Subscribe creates a buffered channel that receives notifications for a specific session.
func (ps *Bus) Subscribe(sessionID int64) <-chan sessionevent.Notification {
	ch := make(chan sessionevent.Notification, subscriberBufferSize)

	ps.mu.Lock()

	ps.subs[sessionID] = append(ps.subs[sessionID], ch)
	ps.mu.Unlock()

	return ch
}

// SubscribeAll creates a buffered channel that receives notifications from all sessions.
func (ps *Bus) SubscribeAll() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification, subscriberBufferSize)

	ps.mu.Lock()

	ps.globals = append(ps.globals, ch)
	ps.mu.Unlock()

	return ch
}

// Unsubscribe removes a per-session subscriber. Does NOT close the channel.
func (ps *Bus) Unsubscribe(sessionID int64, ch <-chan sessionevent.Notification) {
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
func (ps *Bus) UnsubscribeAll(ch <-chan controllerapi.SessionNotification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for i, s := range ps.globals {
		if s == ch {
			ps.globals = append(ps.globals[:i], ps.globals[i+1:]...)
			break
		}
	}
}

// UnsubscribeRoots removes a root subscriber without closing it.
func (ps *Bus) UnsubscribeRoots(ch <-chan controllerapi.SessionNotification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for i, subscriber := range ps.roots {
		if subscriber == ch {
			ps.roots = append(ps.roots[:i], ps.roots[i+1:]...)
			break
		}
	}
}

// Publish fans an event out to observers and, for a root, to every root subscriber.
func (ps *Bus) Publish(sessionID int64, root bool, n sessionevent.Notification) {
	log := logger.Named("sessionbus")

	ps.mu.RLock()
	perSession := append([]chan sessionevent.Notification(nil), ps.subs[sessionID]...)
	globals := append([]chan controllerapi.SessionNotification(nil), ps.globals...)
	var managerSubscribers []chan controllerapi.SessionNotification

	if root {
		managerSubscribers = append(managerSubscribers, ps.roots...)
	}

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

	for _, ch := range managerSubscribers {
		select {
		case ch <- sn:
		default:
			log.Warn(
				"pubsub_slow_root_subscriber",
				zap.Int64("session_id", sessionID),
				zap.String("type", string(n.Type)),
			)
		}
	}
}
