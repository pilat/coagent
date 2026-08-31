package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/subagent"
)

type blockingRecoveryLinks struct {
	subagent.Store

	entered     chan struct{}
	cancelled   chan struct{}
	allowReturn chan struct{}
	once        sync.Once
}

func (s *blockingRecoveryLinks) ListRunningChildLinks(ctx context.Context) ([]subagent.Link, error) {
	s.once.Do(func() { close(s.entered) })
	<-ctx.Done()
	close(s.cancelled)
	<-s.allowReturn

	return nil, ctx.Err()
}

// Recovery is background work, but it must exit before shutdown lets its stores close.
func TestShutdownCancelsBackgroundRecovery(t *testing.T) {
	h := newSubagentHarness(t)
	links := &blockingRecoveryLinks{
		Store:       h.mgr.links,
		entered:     make(chan struct{}),
		cancelled:   make(chan struct{}),
		allowReturn: make(chan struct{}),
	}
	h.mgr.links = links

	require.NoError(t, h.mgr.Start(h.ctx))
	select {
	case <-links.entered:
	case <-time.After(time.Second):
		t.Fatal("background recovery did not reach the cancellation boundary")
	}

	shutdownDone := make(chan struct{})
	go func() {
		h.mgr.Shutdown(time.Second)
		close(shutdownDone)
	}()

	select {
	case <-links.cancelled:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not cancel background recovery")
	}

	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before background recovery exited")
	default:
	}

	close(links.allowReturn)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for background recovery")
	}
}

func TestStartDoesNotLaunchRecoveryAfterShutdown(t *testing.T) {
	h := newSubagentHarness(t)
	h.mgr.Shutdown(time.Second)

	require.NoError(t, h.mgr.Start(h.ctx))
	assert.False(t, h.mgr.recovery.Active())
}

func TestEnsureRunnerRejectsShutdown(t *testing.T) {
	h := newSubagentHarness(t)
	h.mgr.shuttingDown.Store(true)

	err := h.mgr.ensureRunner(h.ctx, 1, t.TempDir(), h.projectID, nil)
	require.ErrorIs(t, err, errDaemonShuttingDown)
}
