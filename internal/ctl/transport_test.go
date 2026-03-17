package ctl

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// connCount reports how many connections the server still tracks.
func connCount(s *Server) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.conns)
}

func drainNotifications(t *testing.T, c *Client, want int) []Notification {
	t.Helper()

	out := make([]Notification, 0, want)
	deadline := time.After(5 * time.Second)

	for len(out) < want {
		select {
		case n, ok := <-c.Notifications():
			require.True(t, ok, "push stream closed after %d of %d notifications", len(out), want)

			out = append(out, n)
		case <-deadline:
			t.Fatalf("only %d of %d notifications arrived", len(out), want)
		}
	}

	return out
}

// Responses and pushes share one connection. Every call must get its own answer
// back, and the pushes a handler emits must stay with that handler's turn.
func TestClient_ConcurrentCallsAreNotCrossWiredWithPushes(t *testing.T) {
	const (
		callers      = 24
		pushesPerOp  = 3
		markerKey    = "marker"
		echoWithPush = "echo_with_push"
	)

	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register(echoWithPush, func(
			_ context.Context,
			conn *Conn,
			params json.RawMessage,
		) (any, *Error) {
			var p map[string]string
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
			}

			for seq := range pushesPerOp {
				if err := conn.Notify("chat_event", map[string]any{markerKey: p[markerKey], "seq": seq}); err != nil {
					return nil, &Error{Code: CodeInternal, Message: err.Error()}
				}
			}

			return p, nil
		}))
	})

	c := h.dial(t)

	var (
		wg      sync.WaitGroup
		results = make([]map[string]string, callers)
		errs    = make([]error, callers)
	)

	for i := range callers {
		wg.Go(func() {
			marker := fmt.Sprintf("m%02d", i)
			errs[i] = c.Call(context.Background(), echoWithPush, map[string]string{markerKey: marker}, &results[i])
		})
	}

	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, fmt.Sprintf("m%02d", i), results[i][markerKey], "the response reached its own caller")
	}

	// The server answers one request at a time, so each turn's pushes arrive as a
	// contiguous, complete run — never spliced into another call's.
	pushes := drainNotifications(t, c, callers*pushesPerOp)
	seen := map[string]bool{}

	for start := 0; start < len(pushes); start += pushesPerOp {
		var first map[string]any

		require.NoError(t, json.Unmarshal(pushes[start].Params, &first))

		marker, _ := first[markerKey].(string)
		require.False(t, seen[marker], "marker %q pushed in two separate runs", marker)
		seen[marker] = true

		for seq := range pushesPerOp {
			var got map[string]any

			require.NoError(t, json.Unmarshal(pushes[start+seq].Params, &got))
			assert.Equal(t, "chat_event", pushes[start+seq].Method)
			assert.Equal(t, marker, got[markerKey])
			assert.InDelta(t, seq, got["seq"], 0.0)
		}
	}

	assert.Len(t, seen, callers)
}

// A terminal dropping is routine. The server must release it, keep serving the
// rest, and report the dead peer to whoever pushes to it.
func TestServer_ADroppedConnectionIsReleasedWhileTheOthersKeepServing(t *testing.T) {
	var (
		mu   sync.Mutex
		conn = map[string]*Conn{}
	)

	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("hello", func(
			_ context.Context,
			c *Conn,
			params json.RawMessage,
		) (any, *Error) {
			var p map[string]string
			if err := json.Unmarshal(params, &p); err != nil {
				return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
			}

			mu.Lock()
			conn[p["name"]] = c
			mu.Unlock()

			return p, nil
		}))
	})

	first, second := h.dial(t), h.dial(t)
	hello(t, first, "first")
	hello(t, second, "second")
	require.Equal(t, 2, connCount(h.server))

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return connCount(h.server) == 1 }, 3*time.Second, 10*time.Millisecond)

	mu.Lock()
	dropped, alive := conn["first"], conn["second"]
	mu.Unlock()

	select {
	case <-dropped.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("the dropped connection never signalled Done")
	}

	select {
	case <-alive.Done():
		t.Fatal("a live connection signalled Done")
	default:
	}

	require.Eventually(t, func() bool {
		return dropped.Notify("chat_event", map[string]string{"to": "nobody"}) != nil
	}, 3*time.Second, 10*time.Millisecond, "a push to a dead peer must report the error")

	require.NoError(t, alive.Notify("chat_event", map[string]string{"to": "second"}))

	n := drainNotifications(t, second, 1)
	assert.JSONEq(t, `{"to":"second"}`, string(n[0].Params))

	_, err := second.Status(context.Background())
	require.NoError(t, err)

	third := h.dial(t)
	hello(t, third, "third")
	assert.Equal(t, 2, connCount(h.server))
}

// The wire is newline-delimited and both sides read through buffers far smaller
// than a payload can be.
func TestTransport_LargeFramesSurviveInBothDirections(t *testing.T) {
	payload := strings.Repeat("ключ\tvalue \"quoted\" {json}\r\n", 40000)

	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("echo_big", func(
			_ context.Context,
			conn *Conn,
			params json.RawMessage,
		) (any, *Error) {
			var text string
			if err := json.Unmarshal(params, &text); err != nil {
				return nil, &Error{Code: CodeInvalidParams, Message: err.Error()}
			}

			if err := conn.Notify("chat_event", text); err != nil {
				return nil, &Error{Code: CodeInternal, Message: err.Error()}
			}

			return text, nil
		}))
	})

	c := h.dial(t)

	var echoed string

	require.NoError(t, c.Call(context.Background(), "echo_big", payload, &echoed))
	require.Len(t, echoed, len(payload))
	require.Equal(t, payload, echoed, "the request survived the codec")

	var pushed string

	require.NoError(t, json.Unmarshal(drainNotifications(t, c, 1)[0].Params, &pushed))
	require.Len(t, pushed, len(payload))
	require.Equal(t, payload, pushed, "the push survived the codec")
}

func hello(t *testing.T, c *Client, name string) {
	t.Helper()

	var out map[string]string

	require.NoError(t, c.Call(context.Background(), "hello", map[string]string{"name": name}, &out))
	require.Equal(t, name, out["name"])
}
