package ctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// dialTimeout bounds the connect and the greeting: a local socket answers at once
// or is still booting, which is a different answer than down. A var only for tests.
var dialTimeout = 3 * time.Second

// notifyBuffer is how far the push stream may run ahead of its reader. Pushes
// share the read loop with responses, so a wedged consumer must lose events
// rather than stall every pending call.
const notifyBuffer = 1024

// ErrNotRunning means the socket is absent or refuses connections — the daemon
// is not running. Connect success is the liveness check, so this is the whole
// test.
var ErrNotRunning = errors.New("coagent daemon is not running")

// ErrStarting means the daemon is bound but has not finished booting: it is
// coming up, not broken, so a client waits instead of reporting a failure.
var ErrStarting = errors.New("coagent daemon is starting")

// ErrClosed means the connection dropped while a call was in flight. During a
// restart-apply this is expected: the daemon execs itself and the client
// reconnects.
var ErrClosed = errors.New("control connection closed")

// Client is a control-socket connection. It multiplexes: one read loop sorts
// inbound lines into pending call replies and pushes, so a notification arriving
// between a request and its response is never mistaken for the response.
type Client struct {
	conn     net.Conn
	greeting Greeting

	notifications chan Notification
	dropped       atomic.Uint64

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan frame
	closed  bool
	readErr error
}

// Dial connects and reads the greeting. A refused or missing socket is
// ErrNotRunning, not a transport failure — "not installed yet" is the normal
// first-run state, not an error to report as one.
func Dial(ctx context.Context, path string) (*Client, error) {
	var d net.Dialer

	dialCtx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()

	// Every dial failure on a local socket means the same thing: nobody is
	// listening. Distinguishing ENOENT from ECONNREFUSED would give a UI two
	// words for one state.
	conn, err := d.DialContext(dialCtx, "unix", path)
	if err != nil {
		return nil, ErrNotRunning
	}

	c := &Client{
		conn:          conn,
		notifications: make(chan Notification, notifyBuffer),
		pending:       make(map[int64]chan frame),
	}

	reader := bufio.NewReader(conn)

	if err := c.readGreeting(reader); err != nil {
		_ = conn.Close()

		// Bound but silent: something owns the socket and has not started
		// answering. That is a daemon coming up, not one that is down.
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil, ErrStarting
		}

		return nil, err
	}

	go c.readLoop(reader)

	return c, nil
}

// Greeting reports what the daemon said on connect: its binary version and the
// protocol version it speaks. Skew is a warning for UIs, never a refusal.
func (c *Client) Greeting() Greeting { return c.greeting }

// SkewsFrom reports whether the daemon's protocol differs from this binary's.
func (c *Client) SkewsFrom() bool { return c.greeting.ProtocolVersion != ProtocolVersion }

// Notifications is the server→client push stream. It closes when the connection
// drops, which is how a chat client learns the daemon went away to restart.
func (c *Client) Notifications() <-chan Notification { return c.notifications }

// DroppedNotifications reports pushes discarded because Notifications was full.
// Push delivery is deliberately best effort so an unread consumer cannot stall
// RPC replies; this counter lets internal callers observe that loss.
func (c *Client) DroppedNotifications() uint64 { return c.dropped.Load() }

// Close ends the connection and fails every call still waiting on it.
func (c *Client) Close() error {
	if err := c.conn.Close(); err != nil {
		return fmt.Errorf("close control connection: %w", err)
	}

	return nil
}

// Call sends one request and decodes the result into out. A JSON-RPC error comes
// back as an error; a rejection verdict is a successful call whose result
// carries applied=false.
func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	req, err := c.newRequest(method, params)
	if err != nil {
		return err
	}

	reply, err := c.register(req.id)
	if err != nil {
		return err
	}

	defer c.unregister(req.id)

	if _, err := c.conn.Write(append(req.body, '\n')); err != nil {
		return fmt.Errorf("write request: %w", err)
	}

	select {
	case <-ctx.Done():
		return fmt.Errorf("%s: %w", method, ctx.Err())
	case f, ok := <-reply:
		if !ok {
			return c.closedErr(method)
		}

		return decodeResult(method, f, out)
	}
}

// Status is the liveness-plus-state call every UI header polls.
func (c *Client) Status(ctx context.Context) (StatusResult, error) {
	var out StatusResult
	err := c.Call(ctx, OpStatus, nil, &out)

	return out, err
}

type outboundRequest struct {
	id   int64
	body []byte
}

func (c *Client) newRequest(method string, params any) (outboundRequest, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	rawID, err := json.Marshal(id)
	if err != nil {
		return outboundRequest{}, fmt.Errorf("encode request id: %w", err)
	}

	req := Request{JSONRPC: jsonrpcVersion, ID: rawID, Method: method}

	if params != nil {
		raw, err := json.Marshal(params)
		if err != nil {
			return outboundRequest{}, fmt.Errorf("encode params: %w", err)
		}

		req.Params = raw
	}

	body, err := json.Marshal(req)
	if err != nil {
		return outboundRequest{}, fmt.Errorf("encode request: %w", err)
	}

	return outboundRequest{id: id, body: body}, nil
}

func (c *Client) register(id int64) (chan frame, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, ErrClosed
	}

	ch := make(chan frame, 1)
	c.pending[id] = ch

	return ch, nil
}

func (c *Client) unregister(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.pending, id)
}

func (c *Client) closedErr(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.readErr != nil {
		return fmt.Errorf("%s: %w", method, c.readErr)
	}

	return fmt.Errorf("%s: %w", method, ErrClosed)
}

// readLoop is the demultiplexer. It runs for the connection's whole life and is
// the only reader, so responses and pushes cannot be confused for one another.
func (c *Client) readLoop(reader *bufio.Reader) {
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			c.shutdown(err)

			return
		}

		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}

		if f.isResponse() {
			c.deliver(f)

			continue
		}

		if f.Method == "" {
			continue
		}

		select {
		case c.notifications <- Notification{JSONRPC: jsonrpcVersion, Method: f.Method, Params: f.Params}:
		default:
			c.dropped.Add(1)
		}
	}
}

func (c *Client) deliver(f frame) {
	var id int64
	if err := json.Unmarshal(f.ID, &id); err != nil {
		return
	}

	c.mu.Lock()
	ch, ok := c.pending[id]
	c.mu.Unlock()

	if !ok {
		return
	}

	select {
	case ch <- f:
	default:
	}
}

func (c *Client) shutdown(err error) {
	c.mu.Lock()

	if c.closed {
		c.mu.Unlock()

		return
	}

	c.closed = true
	c.readErr = err

	waiters := make([]chan frame, 0, len(c.pending))
	for _, ch := range c.pending {
		waiters = append(waiters, ch)
	}

	clear(c.pending)
	c.mu.Unlock()

	for _, ch := range waiters {
		close(ch)
	}

	close(c.notifications)
}

func (c *Client) readGreeting(reader *bufio.Reader) error {
	_ = c.conn.SetReadDeadline(time.Now().Add(dialTimeout))
	defer func() { _ = c.conn.SetReadDeadline(time.Time{}) }()

	line, err := reader.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}

	if err := json.Unmarshal(line, &c.greeting); err != nil {
		return fmt.Errorf("decode greeting: %w", err)
	}

	if c.greeting.App != AppName {
		return fmt.Errorf("socket is not a coagent daemon (greeted as %q)", c.greeting.App)
	}

	return nil
}

func decodeResult(method string, f frame, out any) error {
	if f.Error != nil {
		if f.Error.Code == CodeStarting {
			return fmt.Errorf("%s: %w", method, ErrStarting)
		}

		return fmt.Errorf("%s: %w", method, f.Error)
	}

	if out == nil || len(f.Result) == 0 {
		return nil
	}

	if err := json.Unmarshal(f.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}

	return nil
}
