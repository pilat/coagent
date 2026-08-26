package ctl

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// maxLineBytes caps one request line. Control payloads are small; this stops a
// runaway client from growing the read buffer without bound.
const maxLineBytes = 8 << 20

// maxSocketPath is the sun_path limit. The kernel reports an overrun as a bare
// EINVAL from bind, which reads as a bug in the daemon rather than as "your home
// directory is too deep" — so it is checked here where the number can be said.
const maxSocketPath = 100

var (
	ErrRegistrationClosed  = errors.New("control server handler registration is closed")
	ErrAlreadyServing      = errors.New("control server is already serving")
	ErrInvalidRegistration = errors.New("invalid control server handler registration")
	ErrOperationReserved   = errors.New("control operation is reserved")
	ErrHandlerRegistered   = errors.New("control operation already has a handler")
)

// Handler answers one op. An *Error makes the reply a JSON-RPC error, reserved
// for malformed input; any other value becomes a result, which is how a
// rejection verdict travels as a successful call.
type Handler func(ctx context.Context, c *Conn, params json.RawMessage) (any, *Error)

// Server accepts control connections on a unix socket.
type Server struct {
	path    string
	deps    Deps
	version string
	bootID  string
	started time.Time

	listener   net.Listener
	wg         sync.WaitGroup
	serveReady chan struct{}

	mu       sync.Mutex
	closed   bool
	serving  bool
	ready    bool
	handlers map[string]Handler
	conns    map[*Conn]struct{}
}

// Conn is one live control connection. Handlers receive it so an op can attach a
// server→client push stream to the connection that asked for it.
type Conn struct {
	conn net.Conn

	// writeMu serializes the encoder: pushes and responses share one socket, and
	// a half-written notification inside a response is not recoverable.
	writeMu sync.Mutex
	enc     *json.Encoder

	// afterReply runs once the current request's response is on the wire. An op
	// that takes the daemon down needs its answer sent before the drain starts.
	afterReply func()

	closeOnce sync.Once
	done      chan struct{}
}

// NewServer binds the control socket. The caller must already hold the
// single-instance Lock: removing a stale socket is only safe for the process
// that proved no other daemon is running.
func NewServer(ctx context.Context, path, version string, deps Deps) (*Server, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf(
			"control socket path is %d bytes, over the %d-byte unix limit: %s",
			len(path), maxSocketPath, path,
		)
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create socket dir: %w", err)
	}

	// Safe under the flock: whatever is here is a leftover, not a live daemon.
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// Bound to the owner before anyone can connect. net.Listen honours umask,
	// which on a permissive umask would leave the socket group-writable.
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()

		return nil, fmt.Errorf("chmod socket: %w", err)
	}

	return &Server{
		path:       path,
		deps:       deps,
		version:    version,
		bootID:     newBootID(),
		started:    time.Now(),
		listener:   ln,
		serveReady: make(chan struct{}),
		handlers:   make(map[string]Handler),
		conns:      make(map[*Conn]struct{}),
	}, nil
}

// Register adds an op owned by another layer, keeping ctl pure transport. It is
// open until MarkReady, rejects built-in or duplicate names, and never replaces.
func (s *Server) Register(op string, h Handler) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ready || s.closed {
		return fmt.Errorf("%w: %s", ErrRegistrationClosed, op)
	}

	if strings.TrimSpace(op) == "" || h == nil {
		return fmt.Errorf("%w: operation and handler are required", ErrInvalidRegistration)
	}

	if op == OpStatus {
		return fmt.Errorf("%w: %s", ErrOperationReserved, op)
	}

	if _, exists := s.handlers[op]; exists {
		return fmt.Errorf("%w: %s", ErrHandlerRegistered, op)
	}

	s.handlers[op] = h

	return nil
}

// Close stops accepting, drops live connections and unlinks the socket. Live
// connections are closed rather than waited on: a chat client sits idle on an
// open socket by design, and the restart drain cannot wait for it to speak.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()

		return nil
	}

	s.closed = true

	live := make([]*Conn, 0, len(s.conns))
	for c := range s.conns {
		live = append(live, c)
	}
	s.mu.Unlock()

	err := s.listener.Close()

	for _, c := range live {
		_ = c.Close()
	}

	s.wg.Wait()

	if rmErr := os.Remove(s.path); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = fmt.Errorf("remove socket: %w", rmErr)
	}

	if err != nil {
		return fmt.Errorf("close control socket: %w", err)
	}

	return nil
}

// Notify pushes one notification to this connection. It is safe to call from any
// goroutine and returns the write error so a broadcaster can drop a dead peer.
func (c *Conn) Notify(method string, params any) error {
	return c.notify(method, params, 0)
}

// NotifyWithin writes one complete push under a bounded socket deadline. The
// deadline is cleared before the shared connection returns to ordinary RPC use.
func (c *Conn) NotifyWithin(method string, params any, timeout time.Duration) error {
	if timeout <= 0 {
		return c.Notify(method, params)
	}

	return c.notify(method, params, timeout)
}

// Close drops this connection alone, indistinguishably from the peer hanging up:
// an in-flight Notify fails, the read loop ends and Done fires. Idempotent.
func (c *Conn) Close() error {
	var err error

	c.closeOnce.Do(func() { err = c.conn.Close() })

	if err != nil {
		return fmt.Errorf("close control connection: %w", err)
	}

	return nil
}

// AfterReply schedules fn to run once this request's response has been written.
// An op that restarts the daemon uses it so the caller has the verdict in hand
// before the socket goes away.
func (c *Conn) AfterReply(fn func()) { c.afterReply = fn }

// Done closes when the connection drops, so whatever subscribed on its behalf
// can unsubscribe.
func (c *Conn) Done() <-chan struct{} { return c.done }

func (c *Conn) notify(method string, params any, timeout time.Duration) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	if timeout > 0 {
		if err := c.conn.SetWriteDeadline(time.Now().Add(timeout)); err != nil {
			return fmt.Errorf("set push deadline: %w", err)
		}
		defer func() { _ = c.conn.SetWriteDeadline(time.Time{}) }()
	}

	raw, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encode %s params: %w", method, err)
	}

	if err := c.enc.Encode(Notification{JSONRPC: jsonrpcVersion, Method: method, Params: raw}); err != nil {
		return fmt.Errorf("push %s: %w", method, err)
	}

	return nil
}

func (s *Server) handleConn(ctx context.Context, nc net.Conn) error {
	c := &Conn{conn: nc, enc: json.NewEncoder(nc), done: make(chan struct{})}

	s.addConn(c)

	defer func() {
		s.removeConn(c)
		close(c.done)

		_ = c.Close()
	}()

	greeting := Greeting{App: AppName, BinaryVersion: s.version, ProtocolVersion: ProtocolVersion}

	c.writeMu.Lock()
	err := c.enc.Encode(greeting)
	c.writeMu.Unlock()

	if err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}

	scanner := bufio.NewScanner(nc)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		c.afterReply = nil
		resp := s.dispatch(ctx, c, line)

		c.writeMu.Lock()
		err := c.enc.Encode(resp)
		c.writeMu.Unlock()

		if err != nil {
			return fmt.Errorf("write response: %w", err)
		}

		if fn := c.afterReply; fn != nil {
			c.afterReply = nil

			fn()
		}
	}

	if err := scanner.Err(); err != nil && !errors.Is(err, net.ErrClosed) {
		return fmt.Errorf("read request: %w", err)
	}

	return nil
}

func (s *Server) addConn(c *Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.conns[c] = struct{}{}
}

func (s *Server) removeConn(c *Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.conns, c)
}

func (s *Server) handler(op string) (Handler, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	h, ok := s.handlers[op]

	return h, ok
}

func (s *Server) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.closed
}

// newBootID names this run of the daemon. Random rather than a start timestamp:
// clients compare it for equality, and a clock is not a source of identity.
func newBootID() string {
	var b [8]byte

	_, _ = rand.Read(b[:])

	return hex.EncodeToString(b[:])
}
