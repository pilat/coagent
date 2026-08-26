package cli

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/logger"
)

// terminalQueueCap bounds one terminal's unwritten pushes; further behind is dropped.
// A dead terminal costs one person, a wedged forwarder costs everyone.
const terminalQueueCap = 128

var _ = (*Manager).writeOutput

// push is one rendered notification waiting on a terminal's writer.
type push struct {
	method string
	params any
	result chan<- error
}

// terminal is an attached connection plus the goroutine that owns its socket writes.
// ctl.Conn.Notify blocks until the peer drains, so it never runs on the forwarder.
type terminal struct {
	conn  *ctl.Conn
	queue chan push

	// mu makes enqueue and the writer's exit mutually exclusive, so a push is
	// never accepted after the writer stopped answering results.
	mu     sync.Mutex
	once   sync.Once
	dead   chan struct{}
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

// enqueue reports false when the queue is full or the writer has exited — the
// caller's cue to drop this terminal rather than wait for it.
func (t *terminal) enqueue(p push) bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	// A separate check: a combined select could still pick the queue send when
	// both cases are ready, accepting a push nobody will ever answer.
	select {
	case <-t.exited:
		return false
	default:
	}

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
// On exit it answers every already-accepted push, so a delivery waiter is never
// stranded behind a result nobody will send.
func (t *terminal) run() {
	defer func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		close(t.exited)

		for {
			select {
			case p := <-t.queue:
				if p.result != nil {
					p.result <- errors.New("terminal writer stopped")
				}
			default:
				return
			}
		}
	}()

	for {
		select {
		case <-t.dead:
			return
		case p := <-t.queue:
			err := t.conn.NotifyWithin(p.method, p.params, 5*time.Second)
			if p.result != nil {
				p.result <- err
			}

			if err != nil {
				logger.Named("managers.cli").Debug("push_failed", zap.Error(err))

				return
			}
		}
	}
}

func (m *Manager) writeOutput(ctx context.Context, params any) error {
	m.mu.Lock()
	clients := slices.Clone(m.clients)
	m.mu.Unlock()

	if len(clients) == 0 {
		return errors.New("no attached terminal")
	}

	results := make(chan error, len(clients))
	queued := 0

	for _, terminal := range clients {
		if terminal.stopped() || !terminal.enqueue(push{method: EventMethod, params: params, result: results}) {
			m.drop(terminal)
			continue
		}

		queued++
	}

	if queued == 0 {
		return errors.New("no writable terminal")
	}

	for range queued {
		select {
		case err := <-results:
			if err == nil {
				return nil
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	return errors.New("all terminal writes failed")
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
