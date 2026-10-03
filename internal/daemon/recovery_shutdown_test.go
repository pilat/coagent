package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/progressruntime"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type blockingRecoveryLinks struct {
	subagent.Store

	entered     chan struct{}
	cancelled   chan struct{}
	allowReturn chan struct{}
	once        sync.Once
}

type blockingProgressStop struct {
	progressruntime.Service

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingProgressStop) Stop(context.Context) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release

	return nil
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

// Runner cancellation must not wait for an unrelated background owner to join:
// the composition root may close persistence as soon as Shutdown returns.
func TestShutdownCancelsRunnersBeforeWaitingForProgress(t *testing.T) {
	mgr, _, _ := newTestManager(t)

	runnerCtx, cancel := context.WithCancel(context.Background())
	activeRunner := newRunner(
		cancel, t.TempDir(), &sessionstore.SessionRecord{ID: 1}, waitingRunner{sessionID: 1}, false,
	)
	_, registered := mgr.runners.register(activeRunner)
	require.True(t, registered)

	go func() {
		<-runnerCtx.Done()
		activeRunner.Complete()
	}()

	progress := &blockingProgressStop{entered: make(chan struct{}), release: make(chan struct{})}
	mgr.progress = progress

	shutdownDone := make(chan struct{})
	go func() {
		mgr.Shutdown(time.Second)
		close(shutdownDone)
	}()

	<-progress.entered

	select {
	case <-runnerCtx.Done():
		close(progress.release)
	case <-time.After(250 * time.Millisecond):
		close(progress.release)
		<-shutdownDone
		t.Fatal("runner cancellation waited for progress shutdown")
	}

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not join the released progress owner and runner")
	}
}

func TestStartDoesNotLaunchRecoveryAfterShutdown(t *testing.T) {
	h := newSubagentHarness(t)
	links := &blockingRecoveryLinks{
		Store: h.mgr.links, entered: make(chan struct{}),
		cancelled: make(chan struct{}), allowReturn: make(chan struct{}),
	}
	close(links.allowReturn)
	h.mgr.links = links
	h.mgr.Shutdown(time.Second)

	require.ErrorIs(t, h.mgr.Start(h.ctx), errDaemonShuttingDown)
	assert.Never(t, func() bool {
		select {
		case <-links.entered:
			return true
		default:
			return false
		}
	}, 50*time.Millisecond, 5*time.Millisecond)
}

type blockingStartupProcesses struct {
	backgroundprocess.Service
	entered chan struct{}
	release chan struct{}
}

func (s *blockingStartupProcesses) InterruptNonterminal(ctx context.Context) (int, error) {
	close(s.entered)
	<-s.release
	return s.Service.InterruptNonterminal(ctx)
}

func TestShutdownWaitsForStartupProcessRecovery(t *testing.T) {
	mgr, _, _ := newTestManager(t)
	processes := &blockingStartupProcesses{
		Service: mgr.processes,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	mgr.processes = processes
	startupDone := make(chan error, 1)
	go func() { startupDone <- mgr.Start(t.Context()) }()
	requireBarrierSignal(t, processes.entered, "startup did not reach process recovery")
	shutdownDone := make(chan struct{})
	go func() { mgr.Shutdown(time.Second); close(shutdownDone) }()
	select {
	case <-shutdownDone:
		t.Fatal("shutdown returned before startup process recovery")
	case <-time.After(50 * time.Millisecond):
	}
	close(processes.release)
	require.ErrorIs(t, <-startupDone, errDaemonShuttingDown)
	requireBarrierSignal(t, shutdownDone, "shutdown did not join startup process recovery")
}

func TestStartLockedRejectsShutdown(t *testing.T) {
	h := newSubagentHarness(t)
	h.mgr.life.close()

	err := h.mgr.startLocked(h.ctx, 1)
	require.ErrorIs(t, err, errDaemonShuttingDown)
}
