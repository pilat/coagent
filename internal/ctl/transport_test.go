package ctl

import (
	"context"
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

// Concurrent status calls share one server. Every call must get its own answer
// back, and the server must keep serving all of them.
func TestClient_ConcurrentStatusCallsAreNotCrossWired(t *testing.T) {
	const callers = 24

	h := newHarness(t, &config.Config{})
	c := h.dial(t)

	var (
		wg      sync.WaitGroup
		results = make([]StatusResult, callers)
		errs    = make([]error, callers)
	)

	for i := range callers {
		wg.Go(func() {
			results[i], errs[i] = c.Status(context.Background())
		})
	}

	wg.Wait()

	for i := range callers {
		require.NoError(t, errs[i])
		assert.Equal(t, testVersion, results[i].BinaryVersion, "the response reached its own caller")
	}
}

// A client dropping is routine. The server must release it and keep serving
// the rest.
func TestServer_ADroppedConnectionIsReleasedWhileTheOthersKeepServing(t *testing.T) {
	h := newHarness(t, &config.Config{})

	first, second := h.dial(t), h.dial(t)

	_, err := first.Status(context.Background())
	require.NoError(t, err)
	_, err = second.Status(context.Background())
	require.NoError(t, err)
	require.Equal(t, 2, connCount(h.server))

	require.NoError(t, first.Close())
	require.Eventually(t, func() bool { return connCount(h.server) == 1 }, 3*time.Second, 10*time.Millisecond)

	_, err = second.Status(context.Background())
	require.NoError(t, err)

	third := h.dial(t)
	_, err = third.Status(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, connCount(h.server))
}

// The wire is newline-delimited and both sides read through buffers far smaller
// than a payload can be.
func TestTransport_LargeFramesSurviveInBothDirections(t *testing.T) {
	h := newHarness(t, &config.Config{
		UnifiedConfig: &config.UnifiedConfig{
			Managers: []config.ManagerEntry{{ID: strings.Repeat("m", 1000), Driver: "telegram", Enabled: enabled()}},
		},
	})
	c := h.dial(t)

	st, err := c.Status(context.Background())
	require.NoError(t, err)
	require.Len(t, st.Managers, 1)
	assert.Equal(t, strings.Repeat("m", 1000), st.Managers[0].ID)

	payload := strings.Repeat("ключ\tvalue \"quoted\" {json}\r\n", 40000)
	var replied StatusResult
	require.NoError(t, c.Call(context.Background(), OpStatus, payload, &replied))
	assert.Equal(t, testVersion, replied.BinaryVersion, "a large params frame must not break the status reply")
}
