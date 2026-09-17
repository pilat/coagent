package ctl

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// Closing one connection must not take the server or its peers down: a status
// client sits idle on an open socket by design, and shutdown cannot wait for it.
func TestConn_CloseReleasesOnlyItself(t *testing.T) {
	h := newHarness(t, &config.Config{})

	first, second := h.dial(t), h.dial(t)
	require.Equal(t, 2, connCount(h.server))

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return connCount(h.server) == 1 }, 3*time.Second, 10*time.Millisecond)

	_, err := second.Status(context.Background())
	require.NoError(t, err, "the server keeps serving everyone else")

	require.NoError(t, second.Close())
	require.NoError(t, second.Close(), "a second Close stays silent")
}

// The server's own shutdown drops every live connection and unlinks the socket.
func TestConn_ServerCloseDropsLiveConnections(t *testing.T) {
	h := newHarness(t, &config.Config{})

	first, second := h.dial(t), h.dial(t)
	require.Equal(t, 2, connCount(h.server))

	require.NoError(t, h.server.Close())
	assert.Equal(t, 0, connCount(h.server))

	require.Error(t, first.Call(context.Background(), OpStatus, nil, nil))
	require.Error(t, second.Call(context.Background(), OpStatus, nil, nil))
}
