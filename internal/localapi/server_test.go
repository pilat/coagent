package localapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

type observedListener struct {
	net.Listener
	blocked chan struct{}
}

type observedConn struct {
	net.Conn
	blocked chan struct{}
}

func (l *observedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, fmt.Errorf("accept observed API connection: %w", err)
	}
	return &observedConn{Conn: conn, blocked: l.blocked}, nil
}

func (c *observedConn) Write(data []byte) (int, error) {
	timer := time.AfterFunc(100*time.Millisecond, func() {
		select {
		case c.blocked <- struct{}{}:
		default:
		}
	})
	defer timer.Stop()
	n, err := c.Conn.Write(data)
	if err != nil {
		return n, fmt.Errorf("write observed API connection: %w", err)
	}
	return n, nil
}

func TestServer_CloseRemovesSocket(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	stream := f.tail(t, h, id, 0)
	_ = nextOutput(t, stream)
	info, err := os.Stat(h.socket)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	h.close(t)
	_, err = os.Stat(h.socket)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = stream.Receive()
	require.Error(t, err)
	assert.Equal(t, connect.CodeUnavailable, connect.CodeOf(err))
}

func TestServer_CloseDropsASilentStatusClient(t *testing.T) {
	f := newAPIFixture(t)
	statusListener, statusSocket := apiListener(t)
	listener, socket := apiListener(t)
	srv := New(t.Context(), statusListener, StatusSource{})
	require.NoError(t, srv.AttachAPI(listener, f.clients()))
	srv.MarkReady()
	accepted := make(chan struct{}, 1)
	srv.status.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			accepted <- struct{}{}
		}
	}
	h := &apiServer{srv: srv, socket: socket, statusSocket: statusSocket, done: make(chan error, 2)}
	go func() { h.done <- srv.ServeStatus() }()
	go func() { h.done <- srv.ServeAPI() }()
	t.Cleanup(func() { h.close(t) })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", statusSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	select {
	case <-accepted:
	case <-ctx.Done():
		t.Fatal("status connection was not accepted")
	}

	started := time.Now()
	h.close(t)
	assert.Less(t, time.Since(started), time.Second)
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF)
}

func TestServer_CloseDoesNotWaitForAStalledClient(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	listener, socket := apiListener(t)
	blocked := make(chan struct{}, 1)
	h := f.serve(t, &observedListener{Listener: listener, blocked: blocked}, socket)
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := coagentv1connect.NewOutputServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	stream, err := client.TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: id})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	_ = nextOutput(t, stream)
	payload := make([]byte, 3<<16)
	for range 40 {
		_, err := rand.Read(payload)
		require.NoError(t, err)
		f.append(t, id, base64.RawStdEncoding.EncodeToString(payload))
	}
	f.wake(id)
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("client never blocked a server write")
	}
	started := time.Now()
	h.close(t)
	assert.Less(t, time.Since(started), closeGrace+time.Second)
}
