package cli

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/sessionevent"
)

// ProjectName is the reserved project the local chat lives in. It is a real
// folder-project like any other, so its sessions persist, resume and show up in
// every listing.
const ProjectName = "coagent"

// Push methods the daemon sends a terminal.
const (
	// EventMethod carries chat output.
	EventMethod = "chat_event"
	// SecretRequestMethod asks the terminal to open a masked prompt. It is a
	// separate method because a credential must never travel as chat text.
	SecretRequestMethod = "secret_request"
	// SecretResolvedMethod closes a masked prompt somebody else answered. Every
	// attached terminal may be showing it; only one of them resolved it.
	SecretResolvedMethod = "secret_resolved"
)

// channelAttribute marks a session as belonging to the terminal. Tools that only
// make sense with a person at a keyboard — request_secret — check it.
const (
	channelAttribute = "channel"
	channelCLI       = "cli"
)

// pendingEventCap bounds what is held while the chat session id is unknown: the
// daemon publishes for a session it has started before it hands the id back.
const pendingEventCap = 64

var _ interface {
	ID() string
	Start(context.Context) error
	Stop(context.Context) error
	Alive() bool
} = (*Manager)(nil)

// Manager is the built-in local chat, a peer of the Telegram manager over the
// control socket. Not a config entry: it is how a daemon with no config gets one.
type Manager struct {
	controller controllerapi.ChatController
	server     *ctl.Server
	model      string
	secrets    SecretRequests

	// createMu serializes the check-then-create of the chat session; mu guards
	// the fields themselves.
	createMu sync.Mutex

	mu        sync.Mutex
	projectID int64
	workDir   string
	sessionID int64
	clients   []*terminal
	pending   []controllerapi.SessionNotification

	subscription <-chan controllerapi.SessionNotification
	adopted      chan struct{}
	cancel       context.CancelFunc
	done         chan struct{}
}

// New builds the local chat manager. model is what a new chat session runs on.
func New(
	controller controllerapi.ChatController,
	server *ctl.Server,
	model string,
	secrets SecretRequests,
) *Manager {
	return &Manager{
		controller: controller,
		server:     server,
		model:      model,
		secrets:    secrets,
		adopted:    make(chan struct{}, 1),
	}
}

func (m *Manager) ID() string { return "cli" }

// Start provisions the reserved project and forwards session events. It runs
// outside the config-driven loop, so it is up on a configless daemon.
//
//nolint:contextcheck // runCtx is the manager's own long-lived root context, canceled by Stop
func (m *Manager) Start(ctx context.Context) error {
	project, err := m.controller.CreateProject(ctx, controllerapi.ProjectCreateData{Name: ProjectName})
	if err != nil {
		return fmt.Errorf("provision the %s project: %w", ProjectName, err)
	}

	m.mu.Lock()
	m.projectID = project.ID
	m.workDir = project.Path
	m.mu.Unlock()

	if err := m.registerOps(); err != nil {
		return fmt.Errorf("register local chat ops: %w", err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel

	// done is read by Alive from whichever goroutine asks for status, so the
	// handoff to the forwarder is published under the lock.
	done := make(chan struct{})

	m.mu.Lock()
	m.done = done
	m.mu.Unlock()

	m.subscription = m.controller.SubscribeAll()

	go func() {
		defer close(done)
		defer m.dropAll()

		m.forward(runCtx)
	}()

	return nil
}

// Alive reports whether the event forwarder is still running: a chat whose
// forwarder exited is not serving terminals, whatever Start returned.
func (m *Manager) Alive() bool {
	m.mu.Lock()
	done := m.done
	m.mu.Unlock()

	if done == nil {
		return false
	}

	select {
	case <-done:
		return false
	default:
		return true
	}
}

func (m *Manager) Stop(ctx context.Context) error {
	if m.cancel != nil {
		m.cancel()
	}

	if m.subscription != nil {
		m.controller.UnsubscribeAll(m.subscription)
	}

	if m.done == nil {
		return nil
	}

	select {
	case <-m.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop cli manager: %w", ctx.Err())
	}
}

// forward pushes the chat session's events to every attached terminal. Other
// sessions' events are not this manager's business.
func (m *Manager) forward(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-m.adopted:
			m.flush()
		case sn, ok := <-m.subscription:
			if !ok {
				return
			}

			m.publish(sn)
		}
	}
}

func (m *Manager) publish(sn controllerapi.SessionNotification) {
	m.mu.Lock()

	// An event published before the chat session id came back is held, not
	// dropped: it is the terminal's own first output.
	if m.sessionID == 0 {
		m.hold(sn)
		m.mu.Unlock()

		return
	}

	sessionID, clients, held := m.sessionID, slices.Clone(m.clients), m.takeHeld()
	m.mu.Unlock()

	m.fanOut(clients, sessionID, append(held, sn))
}

// adopt records which session the chat is on and wakes the forwarder to release
// what was published before the id was known.
func (m *Manager) adopt(sessionID int64) {
	m.mu.Lock()
	m.sessionID = sessionID
	waiting := len(m.pending) > 0
	m.mu.Unlock()

	if sessionID == 0 || !waiting {
		return
	}

	select {
	case m.adopted <- struct{}{}:
	default:
	}
}

// flush releases held events when no further one is coming to carry them out.
// It runs on the forwarder, so it cannot interleave with publish.
func (m *Manager) flush() {
	m.mu.Lock()
	sessionID, clients, held := m.sessionID, slices.Clone(m.clients), m.takeHeld()
	m.mu.Unlock()

	m.fanOut(clients, sessionID, held)
}

// hold and takeHeld are called with mu held.
func (m *Manager) hold(sn controllerapi.SessionNotification) {
	m.pending = append(m.pending, sn)
	if len(m.pending) > pendingEventCap {
		m.pending = m.pending[1:]
	}
}

func (m *Manager) takeHeld() []controllerapi.SessionNotification {
	held := m.pending
	m.pending = nil

	return held
}

// render turns a session event into what the terminal has to do about it. The
// masked prompt is its own pair of methods because neither is a line to print —
// one opens a prompt the client must own, the other closes it.
func render(sn controllerapi.SessionNotification) (string, any) {
	switch sn.Notification.Type { //nolint:exhaustive // everything else is a chat line
	case sessionevent.NotifySecretRequest:
		return SecretRequestMethod, SecretRequest{
			SessionID: sn.SessionID,
			RequestID: sn.Notification.RequestID,
			Name:      sn.Notification.SecretName,
			Purpose:   sn.Notification.Message,
		}
	case sessionevent.NotifySecretResolved:
		return SecretResolvedMethod, SecretResolved{
			SessionID: sn.SessionID,
			RequestID: sn.Notification.RequestID,
			Name:      sn.Notification.SecretName,
		}
	}

	return EventMethod, Event{
		SessionID: sn.SessionID,
		Type:      string(sn.Notification.Type),
		Message:   sn.Notification.Message,
		Status:    string(sn.Notification.Status),
	}
}
