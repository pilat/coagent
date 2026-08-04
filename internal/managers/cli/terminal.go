package cli

import (
	"slices"
	"sync"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/logger"
)

// terminalQueueCap bounds one terminal's unwritten pushes; further behind is dropped.
// A dead terminal costs one person, a wedged forwarder costs everyone.
const terminalQueueCap = 128

// push is one rendered notification waiting on a terminal's writer.
type push struct {
	method string
	params any
}

// terminal is an attached connection plus the goroutine that owns its socket writes.
// ctl.Conn.Notify blocks until the peer drains, so it never runs on the forwarder.
type terminal struct {
	conn  *ctl.Conn
	queue chan push

	once sync.Once
	dead chan struct{}

	// exited closes when the writer goroutine has returned, so a dropped
	// terminal's release is observable rather than assumed.
	exited chan struct{}
}

func newTerminal(c *ctl.Conn) *terminal {
	return &terminal{
		conn:   c,
		queue:  make(chan push, terminalQueueCap),
		dead:   make(chan struct{}),
		exited: make(chan struct{}),
	}
}

// enqueue reports false when the queue is full — the caller's cue to drop this
// terminal rather than wait for it.
func (t *terminal) enqueue(p push) bool {
	select {
	case t.queue <- p:
		return true
	default:
		return false
	}
}

func (t *terminal) stopped() bool {
	select {
	case <-t.dead:
		return true
	default:
		return false
	}
}

// kill stops the writer and drops its connection: a writer already inside Notify
// is freed by nothing else, and a terminal reached here is finished either way.
func (t *terminal) kill() {
	t.once.Do(func() {
		close(t.dead)

		_ = t.conn.Close()
	})
}

// run writes queued pushes until the terminal is killed or the socket refuses.
func (t *terminal) run() {
	defer close(t.exited)

	for {
		select {
		case <-t.dead:
			return
		case p := <-t.queue:
			if err := t.conn.Notify(p.method, p.params); err != nil {
				logger.Named("managers.cli").Debug("push_failed", zap.Error(err))

				return
			}
		}
	}
}

// fanOut queues each event on every attached terminal. It runs on the forwarder,
// so it must never block: a terminal that cannot keep up is dropped instead.
func (m *Manager) fanOut(clients []*terminal, sessionID int64, events []controllerapi.SessionNotification) {
	for _, sn := range events {
		if sn.SessionID != sessionID {
			continue
		}

		method, payload := render(sn)

		for _, t := range clients {
			m.queue(t, push{method: method, params: payload})
		}
	}
}

func (m *Manager) queue(t *terminal, p push) {
	if t.stopped() || t.enqueue(p) {
		return
	}

	logger.Named("managers.cli").Warn(
		"terminal_dropped",
		zap.String("reason", "push queue overflow"),
		zap.Int("queue_cap", terminalQueueCap),
	)

	m.drop(t)
}

// attach subscribes one connection to the chat stream and returns its terminal, so a
// caller can push to that one alone. Concurrent terminals interleave by design.
func (m *Manager) attach(c *ctl.Conn) *terminal {
	m.mu.Lock()

	if i := slices.IndexFunc(m.clients, func(t *terminal) bool { return t.conn == c }); i >= 0 {
		existing := m.clients[i]
		m.mu.Unlock()

		return existing
	}

	t := newTerminal(c)
	m.clients = append(m.clients, t)
	m.mu.Unlock()

	go func() {
		t.run()
		m.drop(t)
	}()

	go func() {
		<-c.Done()
		m.drop(t)
	}()

	return t
}

// drop takes a terminal out of the fan-out and stops its writer. Idempotent: a
// queue overflow, a write error and the disconnect all race to it.
func (m *Manager) drop(t *terminal) {
	t.kill()

	m.mu.Lock()
	defer m.mu.Unlock()

	m.clients = slices.DeleteFunc(m.clients, func(other *terminal) bool { return other == t })
}

func (m *Manager) dropAll() {
	m.mu.Lock()
	clients := m.clients
	m.clients = nil
	m.mu.Unlock()

	for _, t := range clients {
		t.kill()
	}
}
