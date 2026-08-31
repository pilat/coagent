package sessionbus

import (
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
)

const subscriberBufferSize = 64

// Source is the subscription side exposed by the daemon.
type Source interface {
	Subscribe(sessionID int64) <-chan sessionevent.Notification
	SubscribeManager(managerID string) <-chan controllerapi.SessionNotification
	SubscribeAll() <-chan controllerapi.SessionNotification
	Unsubscribe(sessionID int64, ch <-chan sessionevent.Notification)
	UnsubscribeManager(ch <-chan controllerapi.SessionNotification)
	UnsubscribeAll(ch <-chan controllerapi.SessionNotification)
}

// Bus adds host-owned publication to the subscription surface.
type Bus interface {
	Source
	Publish(sessionID int64, notification sessionevent.Notification)
	PublishOwned(sessionID int64, managerID string, notification sessionevent.Notification)
}

var _ Bus = (*bus)(nil)

// bus is a fan-out notification hub for session notifications.
// Per-session subscribers receive notifications for a specific session.
// Global subscribers receive notifications from all sessions (with session ID).
type bus struct {
	mu       sync.RWMutex
	subs     map[int64][]chan sessionevent.Notification // per-session subscribers
	globals  []chan controllerapi.SessionNotification   // wildcard subscribers (carry session ID)
	managers map[string][]chan controllerapi.SessionNotification
}

func New() Bus {
	return &bus{
		subs:     make(map[int64][]chan sessionevent.Notification),
		managers: make(map[string][]chan controllerapi.SessionNotification),
	}
}

// SubscribeManager receives only sessions durably owned by managerID.
func (ps *bus) SubscribeManager(managerID string) <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification, subscriberBufferSize)
	if managerID == "" {
		return ch
	}

	ps.mu.Lock()
	ps.managers[managerID] = append(ps.managers[managerID], ch)
	ps.mu.Unlock()

	return ch
}

// Subscribe creates a buffered channel that receives notifications for a specific session.
func (ps *bus) Subscribe(sessionID int64) <-chan sessionevent.Notification {
	ch := make(chan sessionevent.Notification, subscriberBufferSize)

	ps.mu.Lock()

	ps.subs[sessionID] = append(ps.subs[sessionID], ch)
	ps.mu.Unlock()

	return ch
}

// SubscribeAll creates a buffered channel that receives notifications from all sessions.
func (ps *bus) SubscribeAll() <-chan controllerapi.SessionNotification {
	ch := make(chan controllerapi.SessionNotification, subscriberBufferSize)

	ps.mu.Lock()

	ps.globals = append(ps.globals, ch)
	ps.mu.Unlock()

	return ch
}

// Unsubscribe removes a per-session subscriber. Does NOT close the channel.
func (ps *bus) Unsubscribe(sessionID int64, ch <-chan sessionevent.Notification) {
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
func (ps *bus) UnsubscribeAll(ch <-chan controllerapi.SessionNotification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for i, s := range ps.globals {
		if s == ch {
			ps.globals = append(ps.globals[:i], ps.globals[i+1:]...)
			break
		}
	}
}

// UnsubscribeManager removes a manager-scoped subscriber. It does not close it.
func (ps *bus) UnsubscribeManager(ch <-chan controllerapi.SessionNotification) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	for managerID, subscribers := range ps.managers {
		for i, subscriber := range subscribers {
			if subscriber != ch {
				continue
			}

			ps.managers[managerID] = append(subscribers[:i], subscribers[i+1:]...)
			if len(ps.managers[managerID]) == 0 {
				delete(ps.managers, managerID)
			}

			return
		}
	}
}

// Publish sends a notification to all per-session subscribers for the given session
// and to all global subscribers. Non-blocking: if a subscriber's channel is full, the
// notification is dropped with a warning log.
func (ps *bus) Publish(sessionID int64, n sessionevent.Notification) {
	ps.PublishOwned(sessionID, "", n)
}

// PublishOwned fans an event out to observers and to the session's one owning
// manager. An empty owner reaches no manager subscription.
func (ps *bus) PublishOwned(sessionID int64, managerID string, n sessionevent.Notification) {
	log := logger.Named("sessionbus")

	ps.mu.RLock()
	perSession := append([]chan sessionevent.Notification(nil), ps.subs[sessionID]...)
	globals := append([]chan controllerapi.SessionNotification(nil), ps.globals...)
	managerSubscribers := append(
		[]chan controllerapi.SessionNotification(nil), ps.managers[managerID]...,
	)
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
				"pubsub_slow_manager_subscriber",
				zap.Int64("session_id", sessionID),
				zap.String("manager_id", managerID),
				zap.String("type", string(n.Type)),
			)
		}
	}
}
