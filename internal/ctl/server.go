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
	"sync"
	"time"
)

// maxLineBytes caps one request line. Control payloads are small; this stops a
// runaway client from growing the read buffer without bound.
const maxLineBytes = 8 << 20

var ErrAlreadyServing = errors.New("control server is already serving")

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

	mu      sync.Mutex
	closed  bool
	serving bool
	ready   bool
	conns   map[*Conn]struct{}
}

// Conn is one live control connection. Requests run one at a time per
// connection, so the encoder needs no lock of its own.
type Conn struct {
	conn net.Conn
	enc  *json.Encoder

	closeOnce sync.Once
}

// NewServer binds the control socket. The caller must already hold the
// single-instance Lock: removing a stale socket is only safe for the process
// that proved no other daemon is running.
func NewServer(ctx context.Context, path, version string, deps Deps) (*Server, error) {
	ln, err := ListenUnix(ctx, path)
	if err != nil {
		return nil, err
	}

	return &Server{
		path:       path,
		deps:       deps,
		version:    version,
		bootID:     newBootID(),
		started:    time.Now(),
		listener:   ln,
		serveReady: make(chan struct{}),
		conns:      make(map[*Conn]struct{}),
	}, nil
}

// Close stops accepting, drops live connections and unlinks the socket. Live
// connections are closed rather than waited on: a status client sits idle on an
// open socket by design, and shutdown cannot wait for it to speak.
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

// Close drops this connection alone, indistinguishably from the peer hanging up.
// Idempotent.
func (c *Conn) Close() error {
	var err error

	c.closeOnce.Do(func() { err = c.conn.Close() })

	if err != nil {
		return fmt.Errorf("close control connection: %w", err)
	}

	return nil
}

func (s *Server) handleConn(ctx context.Context, nc net.Conn) error {
	c := &Conn{conn: nc, enc: json.NewEncoder(nc)}

	s.addConn(c)

	defer func() {
		s.removeConn(c)

		_ = c.Close()
	}()

	greeting := Greeting{App: AppName, BinaryVersion: s.version, ProtocolVersion: ProtocolVersion}

	if err := c.enc.Encode(greeting); err != nil {
		return fmt.Errorf("write greeting: %w", err)
	}

	scanner := bufio.NewScanner(nc)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if err := c.enc.Encode(s.dispatch(ctx, line)); err != nil {
			return fmt.Errorf("write response: %w", err)
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
