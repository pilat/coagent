package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/managers/cli"
)

// reconnectBudget bounds the wait for a daemon that went away to apply a config
// change. Past it the restart did not work, and saying so beats spinning.
const reconnectBudget = 60 * time.Second

const reconnectPoll = 250 * time.Millisecond

// secretQueueCap bounds unanswered masked prompts. Queueing rather than
// prompting on the push goroutine is what keeps the terminal single-owner.
const secretQueueCap = 8

// chat owns terminal state. Lock order is reconnectMu before mu or outMu;
// secretMu is independent and never nests with another lock.
type chat struct {
	socket string
	term   terminal
	out    io.Writer
	errOut io.Writer

	budget time.Duration
	poll   time.Duration

	secrets chan cli.SecretRequest

	// secretMu guards the two request-id sets the push reader and the input loop
	// share: what this terminal answered, and what was resolved without it.
	secretMu  sync.Mutex
	answered  map[string]bool
	dismissed map[string]bool
	requested map[string]bool
	replayed  map[string]cli.SecretRequest

	// pushLive/pushPeak count push readers. Exactly one owns the stream: a
	// reconnect that left the old one running would double-read the connection.
	pushLive        atomic.Int32
	pushPeak        atomic.Int32
	pushWG          sync.WaitGroup
	reconnectMu     sync.Mutex
	reconnectFailed *ctl.Client
	reconnectErr    error
	fatal           chan *chatFatalError

	mu         sync.Mutex
	client     *ctl.Client
	session    int64
	generation int64
	model      string
	busy       bool
	outputID   int64
	activity   int

	outMu sync.Mutex
}

func runChat(ctx context.Context, socket string) int {
	return newChat(socket, newTTYTerminal(os.Stdin), os.Stdout, os.Stderr).run(ctx)
}

func newChat(socket string, t terminal, out, errOut io.Writer) *chat {
	return &chat{
		socket:  socket,
		term:    t,
		out:     out,
		errOut:  errOut,
		budget:  reconnectBudget,
		poll:    reconnectPoll,
		secrets: make(chan cli.SecretRequest, secretQueueCap),
		fatal:   make(chan *chatFatalError, 1),

		answered:  make(map[string]bool),
		dismissed: make(map[string]bool),
		requested: make(map[string]bool),
		replayed:  make(map[string]cli.SecretRequest),
	}
}

func (c *chat) run(ctx context.Context) int {
	if err := c.connect(ctx); err != nil {
		c.errorf("%v", err)

		return exitError
	}

	c.println("Talking to coagent. Ctrl-D to leave, /stop to interrupt a turn, /model to switch model.")

	eventsCtx, cancelEvents := context.WithCancel(ctx)
	defer func() {
		cancelEvents()
		c.pushWG.Wait()
		c.closeClient()
	}()

	c.startEvents(eventsCtx)

	return c.readLines(ctx)
}

// connect dials, opens the chat and remembers which session it joined.
func (c *chat) connect(ctx context.Context) error {
	client, err := ctl.Dial(ctx, c.socket)
	if err != nil {
		return err
	}

	var res cli.OpenResult
	if err := client.Call(ctx, cli.OpChatOpen, struct{}{}, &res); err != nil {
		_ = client.Close()

		return err
	}

	c.mu.Lock()
	c.client = client
	c.session = res.SessionID

	c.generation = res.Generation
	if res.ProgressWatermark > c.outputID {
		c.outputID = res.ProgressWatermark
	}
	c.mu.Unlock()

	if res.Progress != "" {
		c.println(res.Progress)
	}

	return nil
}

func (c *chat) closeClient() {
	if client := c.currentClient(); client != nil {
		_ = client.Close()
	}
}

// send delivers one line, reconnecting once if the daemon restarted under it —
// which is the normal outcome of asking the agent to change the config.
func (c *chat) send(ctx context.Context, line string) error {
	c.setBusy(true)

	var res cli.SendResult

	failed, err := c.call(ctx, cli.OpChatSend, c.sendParams(line), &res)
	if err == nil {
		c.setSessionAt(res.SessionID, res.Generation)

		return nil
	}

	// Only a refused registration proves the message never left: anything else
	// may have been delivered, and resending it would duplicate the turn.
	if !errors.Is(err, ctl.ErrClosed) && !errors.Is(err, ctl.ErrNotRunning) {
		c.setBusy(false)

		return err
	}

	if err := c.reconnect(ctx, failed); err != nil {
		c.setBusy(false)

		return err
	}

	if _, err := c.call(ctx, cli.OpChatSend, c.sendParams(line), &res); err != nil {
		c.setBusy(false)

		return err
	}

	c.setSessionAt(res.SessionID, res.Generation)

	return nil
}

func (c *chat) stopTurn(ctx context.Context) {
	if c.currentSession() == 0 {
		c.prompt()

		return
	}

	if _, err := c.call(ctx, cli.OpChatStop, cli.SessionParams{SessionID: c.currentSession()}, nil); err != nil {
		c.errorf("%v", err)
	}
}

// call runs one op against whichever connection is live now, so a reconnect
// cannot be dereferenced mid-swap.
func (c *chat) call(ctx context.Context, method string, params, out any) (*ctl.Client, error) {
	client := c.currentClient()
	if client == nil {
		return nil, ctl.ErrClosed
	}

	err := client.Call(ctx, method, params, out)

	return client, err
}

// callIdempotent reconnects and safely repeats model discovery or selection.
// Both operations can be issued twice without duplicating a conversation turn.
func (c *chat) callIdempotent(ctx context.Context, method string, params func() any, out any) error {
	failed, err := c.call(ctx, method, params(), out)
	if !errors.Is(err, ctl.ErrClosed) && !errors.Is(err, ctl.ErrNotRunning) {
		return err
	}

	if err := c.reconnect(ctx, failed); err != nil {
		return err
	}

	_, err = c.call(ctx, method, params(), out)

	return err
}

func (c *chat) currentClient() *ctl.Client {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.client
}

func (c *chat) currentSession() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.session
}

func (c *chat) setSession(id int64) {
	c.setSessionAt(id, 0)
}

func (c *chat) setSessionAt(id, generation int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if generation < c.generation || generation == c.generation &&
		(generation != 0 || c.session != 0 || id == 0) {
		return
	}

	c.generation = generation
	c.session = id
	c.model = ""
}

func (c *chat) applyLifecycle(generation, oldSessionID, sessionID int64, kind string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if generation <= c.generation {
		return
	}

	c.generation = generation

	switch kind {
	case "session_opened":
		c.session = sessionID
	case "session_replaced":
		if c.session == oldSessionID || c.session == 0 {
			c.session = sessionID
		}
	case "session_closed":
		if c.session == sessionID {
			c.session = 0
		}
	}

	c.model = ""
}

func (c *chat) setBusy(busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.busy = busy
}

func (c *chat) recordOutput(id int64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if id > c.outputID {
		c.outputID = id
	}
}

func (c *chat) outputDelivered(id int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return id == 0 || c.outputID >= id
}

func (c *chat) isBusy() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.busy
}

func (c *chat) println(line string) { c.write(line + "\n") }

func (c *chat) printf(format string, args ...any) { c.write(fmt.Sprintf(format, args...)) }

func (c *chat) write(s string) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	_, _ = io.WriteString(c.out, s)
}

func (c *chat) errorf(format string, args ...any) {
	c.outMu.Lock()
	defer c.outMu.Unlock()

	_, _ = fmt.Fprintf(c.errOut, format+"\n", args...)
}
