package localapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func TestLoopback_BrowserClientStreamsOverHTTP1(t *testing.T) {
	// This proves neither a real browser's fetch streaming nor its CORS enforcement.
	f := newAPIFixture(t)
	listener, _ := loopbackListener(t)
	h := f.serveLoopback(t, listener, []string{"http://localhost:5173"})
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	outputs := coagentv1connect.NewOutputServiceClient(
		h.tcpClient(t, "Bearer "+f.tokens["alpha"], "http://localhost:5173"),
	)
	stream, err := outputs.TailOutput(ctx, &coagentv1.TailOutputRequest{SessionId: id})
	require.NoError(t, err)
	defer func() { _ = stream.Close() }()
	_ = nextOutput(t, stream)
	want := f.append(t, id, "after the browser-like stream started")
	f.wake(id)
	row := nextOutput(t, stream)
	assert.Equal(t, want, row.GetId())
	assert.Equal(t, "after the browser-like stream started", row.GetContent())
	events, err := outputs.StreamSessionEvents(ctx, &coagentv1.StreamSessionEventsRequest{})
	require.NoError(t, err)
	defer func() { _ = events.Close() }()
	event, err := events.Receive()
	require.NoError(t, err)
	assert.Zero(t, event.GetSessionId())
	assert.Equal(t, coagentv1.SessionEventType_SESSION_EVENT_TYPE_ACTIVITY, event.GetType())
}

func TestServer_CloseDoesNotWaitForAStalledLoopbackClient(t *testing.T) {
	f := newAPIFixture(t)
	listener, addr := loopbackListener(t)
	blocked := make(chan struct{}, 1)
	h := f.serveLoopback(t, &observedListener{Listener: listener, blocked: blocked}, []string{"http://localhost:5173"})
	id := f.root(t, h, "alpha")
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	client := coagentv1connect.NewOutputServiceClient(
		h.tcpClient(t, "Bearer "+f.tokens["alpha"], "http://localhost:5173"),
	)
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
	_, err = (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	require.Error(t, err)
}
