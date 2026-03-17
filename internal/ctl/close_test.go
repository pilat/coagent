package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// wedgedPush is one push in the loop that fills a stopped peer's socket buffer.
// Large enough that a handful of them park the writer, small enough to encode fast.
const wedgedPush = 64 << 10

// requireStalled waits until the pushing goroutine stops making progress, which on
// a peer that never reads means it is parked inside the socket write itself.
func requireStalled(t *testing.T, sent *atomic.Int64) {
	t.Helper()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		before := sent.Load()

		time.Sleep(300 * time.Millisecond)

		if before > 0 && sent.Load() == before {
			return
		}
	}

	t.Fatal("the wedged peer never stopped absorbing pushes")
}

// grabbedConn dials the socket raw, never reads it again — a suspended terminal
// from the server's side — and hands back the *Conn the server made for it.
func grabbedConn(t *testing.T, h *harness, grabbed <-chan *Conn) *Conn {
	t.Helper()

	peer, err := net.Dial("unix", h.socket)
	require.NoError(t, err)

	t.Cleanup(func() { _ = peer.Close() })

	_, err = fmt.Fprint(peer, "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"grab\"}\n")
	require.NoError(t, err)

	select {
	case c := <-grabbed:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("the server never dispatched the grab op")

		return nil
	}
}

func newGrabHarness(t *testing.T) (*harness, <-chan *Conn) {
	t.Helper()

	grabbed := make(chan *Conn, 4)
	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("grab", func(
			_ context.Context,
			c *Conn,
			_ json.RawMessage,
		) (any, *Error) {
			grabbed <- c

			return "ok", nil
		}))
	})

	return h, grabbed
}

// A push to a peer that stopped reading parks inside the socket write. Whoever
// owns that connection must be able to end it alone, without taking the server down.
func TestConn_CloseUnblocksAnInFlightNotify(t *testing.T) {
	h, grabbed := newGrabHarness(t)
	conn := grabbedConn(t, h, grabbed)

	var sent atomic.Int64

	pushed := make(chan error, 1)

	go func() {
		payload := strings.Repeat("x", wedgedPush)

		for {
			if err := conn.Notify("chat_event", payload); err != nil {
				pushed <- err

				return
			}

			sent.Add(1)
		}
	}()

	requireStalled(t, &sent)
	require.NoError(t, conn.Close())

	select {
	case err := <-pushed:
		require.Error(t, err, "a push interrupted mid-write must report the failure")
	case <-time.After(3 * time.Second):
		t.Fatal("Notify stayed blocked on the wedged peer")
	}

	select {
	case <-conn.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the closed connection never signalled Done")
	}

	require.Eventually(t, func() bool { return connCount(h.server) == 0 }, 3*time.Second, 10*time.Millisecond,
		"a self-closed connection is released by the server")

	// The read loop closes done, so a second Close must not race it into a
	// double close — and must stay silent.
	require.NoError(t, conn.Close())
	require.NoError(t, conn.Close())

	select {
	case <-conn.Done():
	default:
		t.Fatal("Done reopened")
	}

	// The server keeps serving everyone else.
	second := h.dial(t)
	_, err := second.Status(context.Background())
	assert.NoError(t, err)
}

// The server's own shutdown must survive a connection that already closed
// itself: the bookkeeping releases it exactly once.
func TestConn_ServerCloseAfterASelfClosedConnection(t *testing.T) {
	h, grabbed := newGrabHarness(t)
	first := grabbedConn(t, h, grabbed)
	second := grabbedConn(t, h, grabbed)

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return connCount(h.server) == 1 }, 3*time.Second, 10*time.Millisecond)

	select {
	case <-second.Done():
		t.Fatal("closing one connection took another one down")
	default:
	}

	require.NoError(t, h.server.Close())

	select {
	case <-second.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the server shutdown left a connection open")
	}

	assert.Equal(t, 0, connCount(h.server))
}
