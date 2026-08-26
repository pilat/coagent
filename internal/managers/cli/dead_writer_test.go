package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/ctl"
)

// deadWriterTerminal builds the exact enqueue-vs-drop race snapshot: a terminal
// whose writer goroutine has exited after a failed socket write (exited closed,
// dead still open) while it is still listed in the manager's fan-out.
func deadWriterTerminal(t *testing.T) (*Manager, *terminal) {
	t.Helper()

	ctx := context.Background()
	socket := scenarioSocket(t)

	srv, err := ctl.NewServer(ctx, socket, "test", ctl.Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	var mu sync.Mutex
	var captured *ctl.Conn
	require.NoError(t, srv.Register("test_capture", func(
		_ context.Context, c *ctl.Conn, _ json.RawMessage,
	) (any, *ctl.Error) {
		mu.Lock()
		captured = c
		mu.Unlock()

		return map[string]any{}, nil
	}))

	go func() { _ = srv.Serve(ctx) }()

	raw, err := net.Dial("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = raw.Close() })

	_, err = fmt.Fprintf(
		raw, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"test_capture\"}\n",
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()

		return captured != nil
	}, 3*time.Second, 5*time.Millisecond)

	mu.Lock()
	conn := captured
	mu.Unlock()

	manager := &Manager{}
	term := newTerminal(conn)

	manager.mu.Lock()
	manager.clients = []*terminal{term}
	manager.mu.Unlock()

	go term.run()

	results := make(chan error, 1)
	require.True(t, term.enqueue(push{
		method: EventMethod,
		params: Event{SessionID: chatSessionID, Type: "message"},
		result: results,
	}))

	// The peer vanishing makes this push's socket write fail: the writer sends
	// its error result and returns without anyone calling kill — drop has not
	// run yet, which is precisely the window writeOutput can be called in.
	require.NoError(t, raw.Close())

	select {
	case <-results:
	case <-time.After(3 * time.Second):
		t.Fatal("the terminal writer never reported the failed push")
	}

	select {
	case <-term.exited:
	case <-time.After(3 * time.Second):
		t.Fatal("the terminal writer did not exit after a failed push")
	}

	assert.False(t, term.stopped(), "the race snapshot must not be marked dead yet")

	return manager, term
}

// A delivery accepted by a terminal whose writer already exited must fail fast:
// nobody will ever send that result, so waiting for it silently froze the whole
// manager-global delivery FIFO while Alive() kept reporting true.
func TestHarnessScenario_DeadWriterTerminalCannotStallDelivery(t *testing.T) {
	manager, term := deadWriterTerminal(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- manager.writeOutput(ctx, Event{SessionID: chatSessionID, Type: "message", Message: "payload"})
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		require.NotErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(4 * time.Second):
		t.Fatalf(
			"writeOutput stalled behind terminal %p whose writer has exited",
			term,
		)
	}
}
