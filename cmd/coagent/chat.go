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

// chat is the terminal side of the local chat. The input loop is the only code
// that ever reads the terminal; the push reader hands it secrets over a channel.
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

	// pushLive/pushPeak count push readers. Exactly one owns the stream: a
	// reconnect that left the old one running would double-read the connection.
	pushLive atomic.Int32
	pushPeak atomic.Int32
	pushWG   sync.WaitGroup

	mu       sync.Mutex
	client   *ctl.Client
	session  int64
	busy     bool
	activity int

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

		answered:  make(map[string]bool),
		dismissed: make(map[string]bool),
	}
}

func (c *chat) run(ctx context.Context) int {
	if err := c.connect(ctx); err != nil {
		c.errorf("%v", err)

		return exitError
	}

	defer func() {
		c.closeClient()
		c.pushWG.Wait()
	}()

	c.println("Talking to coagent. Ctrl-D to leave, /stop to interrupt a turn.")

	c.startEvents(ctx)

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
	c.mu.Unlock()

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

	err := c.call(ctx, cli.OpChatSend, cli.SendParams{SessionID: c.currentSession(), Text: line}, &res)
	if err == nil {
		c.setSession(res.SessionID)

		return nil
	}

	// Only a refused registration proves the message never left: anything else
	// may have been delivered, and resending it would duplicate the turn.
	if !errors.Is(err, ctl.ErrClosed) && !errors.Is(err, ctl.ErrNotRunning) {
		c.setBusy(false)

		return err
	}

	c.println("daemon restarting…")

	if err := c.reconnect(ctx); err != nil {
		c.setBusy(false)

		return err
	}

	if err := c.call(ctx, cli.OpChatSend, cli.SendParams{SessionID: c.currentSession(), Text: line}, &res); err != nil {
		c.setBusy(false)

		return err
	}

	c.setSession(res.SessionID)

	return nil
}

// reconnect waits for the daemon to come back and re-attaches to the same
// conversation. The budget is a real answer: past it, the restart failed.
func (c *chat) reconnect(ctx context.Context) error {
	c.closeClient()

	// The dropped connection ends the old push reader; waiting for it keeps the
	// stream single-owner across the restart.
	c.pushWG.Wait()

	deadline := time.Now().Add(c.budget)

	for time.Now().Before(deadline) {
		if err := c.connect(ctx); err == nil {
			c.startEvents(ctx)
			c.println("reconnected.")

			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the daemon: %w", ctx.Err())
		case <-time.After(c.poll):
		}
	}

	return fmt.Errorf("the daemon did not come back within %s — check `coagent status`", c.budget)
}

func (c *chat) stopTurn(ctx context.Context) {
	if c.currentSession() == 0 {
		c.prompt()

		return
	}

	if err := c.call(ctx, cli.OpChatStop, cli.SessionParams{SessionID: c.currentSession()}, nil); err != nil {
		c.errorf("%v", err)
	}
}

// call runs one op against whichever connection is live now, so a reconnect
// cannot be dereferenced mid-swap.
func (c *chat) call(ctx context.Context, method string, params, out any) error {
	client := c.currentClient()
	if client == nil {
		return ctl.ErrClosed
	}

	return client.Call(ctx, method, params, out)
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
	c.mu.Lock()
	defer c.mu.Unlock()

	c.session = id
}

func (c *chat) setBusy(busy bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.busy = busy
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
